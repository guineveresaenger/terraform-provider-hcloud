package server

import (
	"context"
	"crypto/sha1"
	"encoding/base64"
	"fmt"
	"log"
	"net"
	"strconv"
	"strings"

	"github.com/hashicorp/terraform-plugin-sdk/v2/diag"

	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"
	"github.com/hetznercloud/hcloud-go/hcloud"
	"github.com/hetznercloud/terraform-provider-hcloud/internal/control"
	"github.com/hetznercloud/terraform-provider-hcloud/internal/hcclient"
)

// ResourceType is the type name of the Hetzner Cloud Server resource.
const ResourceType = "hcloud_server"

// Resource creates a Terraform schema for the hcloud_server resource.
func Resource() *schema.Resource {
	return &schema.Resource{
		CreateContext: resourceServerCreate,
		ReadContext:   resourceServerRead,
		UpdateContext: resourceServerUpdate,
		DeleteContext: resourceServerDelete,
		Importer: &schema.ResourceImporter{
			StateContext: schema.ImportStatePassthroughContext,
		},

		Schema: map[string]*schema.Schema{
			"name": {
				Type:     schema.TypeString,
				Required: true,
			},
			"server_type": {
				Type:     schema.TypeString,
				Required: true,
			},
			"image": {
				Type:     schema.TypeString,
				Required: true,
				ForceNew: true,
				ValidateFunc: func(val interface{}, key string) (i []string, errors []error) {
					image := val.(string)
					if len(image) == 0 {
						errors = append(errors, fmt.Errorf("%q must have more then 0 characters. Have you set the name instead of an ID?", key))
					}
					return
				},
			},
			"location": {
				Type:     schema.TypeString,
				Optional: true,
				ForceNew: true,
				Computed: true,
			},
			"datacenter": {
				Type:     schema.TypeString,
				Optional: true,
				ForceNew: true,
				Computed: true,
			},
			"user_data": {
				Type:             schema.TypeString,
				Optional:         true,
				ForceNew:         true,
				DiffSuppressFunc: userDataDiffSuppress,
				StateFunc: func(v interface{}) string {
					switch x := v.(type) {
					case string:
						return userDataHashSum(x)
					default:
						return ""
					}
				},
			},
			"ssh_keys": {
				Type:     schema.TypeList,
				Optional: true,
				Elem:     &schema.Schema{Type: schema.TypeString},
				ForceNew: true,
			},
			"keep_disk": {
				Type:     schema.TypeBool,
				Optional: true,
				Default:  false,
			},
			"backup_window": {
				Type:       schema.TypeString,
				Deprecated: "You should remove this property from your terraform configuration.",
				Computed:   true,
			},
			"backups": {
				Type:     schema.TypeBool,
				Optional: true,
				Default:  false,
			},
			"ipv4_address": {
				Type:     schema.TypeString,
				Computed: true,
			},
			"ipv6_address": {
				Type:     schema.TypeString,
				Computed: true,
			},
			"ipv6_network": {
				Type:     schema.TypeString,
				Computed: true,
			},
			"status": {
				Type:     schema.TypeString,
				Computed: true,
			},
			"iso": {
				Type:     schema.TypeString,
				Optional: true,
			},
			"rescue": {
				Type:     schema.TypeString,
				Optional: true,
			},
			"labels": {
				Type:     schema.TypeMap,
				Optional: true,
			},
			"network": {
				Type:     schema.TypeSet,
				Optional: true,
				Elem: &schema.Resource{
					Schema: map[string]*schema.Schema{
						"network_id": {
							Type:     schema.TypeInt,
							Required: true,
						},
						"ip": {
							Type:     schema.TypeString,
							Computed: true,
							Optional: true,
						},
						"alias_ips": {
							Type:     schema.TypeSet,
							Elem:     &schema.Schema{Type: schema.TypeString},
							Optional: true,
						},
						"mac_address": {
							Type:     schema.TypeString,
							Computed: true,
						},
					},
				},
			},
		},
	}
}

func userDataHashSum(userData string) string {
	sum := sha1.Sum([]byte(userData))
	return base64.StdEncoding.EncodeToString(sum[:])
}

func userDataDiffSuppress(k, old, new string, d *schema.ResourceData) bool {
	userData := d.Get(k).(string)
	if new != "" && userData != "" {
		if _, err := base64.StdEncoding.DecodeString(old); err != nil {
			return userDataHashSum(old) == new
		}
	}
	return strings.TrimSpace(old) == strings.TrimSpace(new)
}

func resourceServerCreate(ctx context.Context, d *schema.ResourceData, m interface{}) diag.Diagnostics {
	c := m.(*hcloud.Client)

	var err error
	image, _, err := c.Image.Get(ctx, d.Get("image").(string))
	if err != nil {
		return diag.FromErr(err)
	}

	opts := hcloud.ServerCreateOpts{
		Name: d.Get("name").(string),
		ServerType: &hcloud.ServerType{
			Name: d.Get("server_type").(string),
		},
		Image:    image,
		UserData: d.Get("user_data").(string),
	}

	opts.SSHKeys, err = getSSHkeys(ctx, c, d)
	if err != nil {
		return diag.FromErr(err)
	}

	if datacenter, ok := d.GetOk("datacenter"); ok {
		opts.Datacenter = &hcloud.Datacenter{Name: datacenter.(string)}
	}

	if location, ok := d.GetOk("location"); ok {
		opts.Location = &hcloud.Location{Name: location.(string)}
	}
	if labels, ok := d.GetOk("labels"); ok {
		tmpLabels := make(map[string]string)
		for k, v := range labels.(map[string]interface{}) {
			tmpLabels[k] = v.(string)
		}
		opts.Labels = tmpLabels
	}

	res, _, err := c.Server.Create(ctx, opts)
	if err != nil {
		return diag.FromErr(err)
	}
	d.SetId(strconv.Itoa(res.Server.ID))

	if err := hcclient.WaitForAction(ctx, &c.Action, res.Action); err != nil {
		return diag.FromErr(err)
	}
	for _, nextAction := range res.NextActions {
		if err := hcclient.WaitForAction(ctx, &c.Action, nextAction); err != nil {
			return diag.FromErr(err)
		}
	}

	if nwSet, ok := d.GetOk("network"); ok {
		for _, item := range nwSet.(*schema.Set).List() {
			nwData := item.(map[string]interface{})
			if err := inlineAttachServerToNetwork(ctx, c, res.Server, nwData); err != nil {
				return diag.FromErr(err)
			}
		}
	}

	backups := d.Get("backups").(bool)
	if err := setBackups(ctx, c, res.Server, backups); err != nil {
		return diag.FromErr(err)
	}

	if iso, ok := d.GetOk("iso"); ok {
		if err := setISO(ctx, c, res.Server, iso.(string)); err != nil {
			return diag.FromErr(err)
		}
	}

	if rescue, ok := d.GetOk("rescue"); ok {
		if err := setRescue(ctx, c, res.Server, rescue.(string), opts.SSHKeys); err != nil {
			return diag.FromErr(err)
		}
	}

	return resourceServerRead(ctx, d, m)
}

func resourceServerRead(ctx context.Context, d *schema.ResourceData, m interface{}) diag.Diagnostics {
	client := m.(*hcloud.Client)

	server, _, err := client.Server.Get(ctx, d.Id())
	if err != nil {
		if resourceServerIsNotFound(err, d) {
			return nil
		}
		return diag.FromErr(err)
	}
	if server == nil {
		d.SetId("")
		return nil
	}
	setServerSchema(d, server)

	d.SetConnInfo(map[string]string{
		"type": "ssh",
		"host": server.PublicNet.IPv4.IP.String(),
	})

	return nil
}

func resourceServerUpdate(ctx context.Context, d *schema.ResourceData, m interface{}) diag.Diagnostics {
	c := m.(*hcloud.Client)

	server, _, err := c.Server.Get(ctx, d.Id())
	if err != nil {
		return diag.FromErr(err)
	}
	if server == nil {
		d.SetId("")
		return nil
	}

	d.Partial(true)
	if d.HasChange("name") {
		newName := d.Get("name")
		_, _, err := c.Server.Update(ctx, server, hcloud.ServerUpdateOpts{
			Name: newName.(string),
		})
		if err != nil {
			if resourceServerIsNotFound(err, d) {
				return nil
			}
			return diag.FromErr(err)
		}
	}
	if d.HasChange("labels") {
		labels := d.Get("labels")
		tmpLabels := make(map[string]string)
		for k, v := range labels.(map[string]interface{}) {
			tmpLabels[k] = v.(string)
		}
		_, _, err := c.Server.Update(ctx, server, hcloud.ServerUpdateOpts{
			Labels: tmpLabels,
		})
		if err != nil {
			if resourceServerIsNotFound(err, d) {
				return nil
			}
			return diag.FromErr(err)
		}
	}
	if d.HasChange("server_type") {
		serverType := d.Get("server_type").(string)
		keepDisk := d.Get("keep_disk").(bool)

		if server.Status == hcloud.ServerStatusRunning {
			action, _, err := c.Server.Poweroff(ctx, server)
			if err != nil {
				return diag.FromErr(err)
			}
			if err := hcclient.WaitForAction(ctx, &c.Action, action); err != nil {
				return diag.FromErr(err)
			}
		}

		action, _, err := c.Server.ChangeType(ctx, server, hcloud.ServerChangeTypeOpts{
			ServerType:  &hcloud.ServerType{Name: serverType},
			UpgradeDisk: !keepDisk,
		})
		if err != nil {
			return diag.FromErr(err)
		}
		if err := hcclient.WaitForAction(ctx, &c.Action, action); err != nil {
			return diag.FromErr(err)
		}
	}

	if d.HasChange("backups") {
		backups := d.Get("backups").(bool)
		if err := setBackups(ctx, c, server, backups); err != nil {
			return diag.FromErr(err)
		}
	}

	if d.HasChange("iso") {
		iso := d.Get("iso").(string)
		if err := setISO(ctx, c, server, iso); err != nil {
			return diag.FromErr(err)
		}
	}

	if d.HasChange("rescue") {
		rescue := d.Get("rescue").(string)
		sshKeys, err := getSSHkeys(ctx, c, d)
		if err != nil {
			return diag.FromErr(err)
		}
		if err := setRescue(ctx, c, server, rescue, sshKeys); err != nil {
			return diag.FromErr(err)
		}
	}

	if d.HasChange("network") {
		data := d.Get("network").(*schema.Set)
		if err := updateServerInlineNetworkAttachments(ctx, c, data, server); err != nil {
			return diag.FromErr(err)
		}
	}

	d.Partial(false)
	return resourceServerRead(ctx, d, m)
}

func resourceServerDelete(ctx context.Context, d *schema.ResourceData, m interface{}) diag.Diagnostics {
	client := m.(*hcloud.Client)

	serverID, err := strconv.Atoi(d.Id())
	if err != nil {
		log.Printf("[WARN] invalid server id (%s), removing from state: %v", d.Id(), err)
		d.SetId("")
		return nil
	}
	if _, err := client.Server.Delete(ctx, &hcloud.Server{ID: serverID}); err != nil {
		return diag.FromErr(err)
	}

	return nil
}

func resourceServerIsNotFound(err error, d *schema.ResourceData) bool {
	if hcerr, ok := err.(hcloud.Error); ok && hcerr.Code == hcloud.ErrorCodeNotFound {
		log.Printf("[WARN] Server (%s) not found, removing from state", d.Id())
		d.SetId("")
		return true
	}
	return false
}

func setBackups(ctx context.Context, c *hcloud.Client, server *hcloud.Server, backups bool) error {
	if server.BackupWindow != "" && !backups {
		action, _, err := c.Server.DisableBackup(ctx, server)
		if err != nil {
			return err
		}
		if err := hcclient.WaitForAction(ctx, &c.Action, action); err != nil {
			return err
		}
		return nil
	}
	if server.BackupWindow == "" && backups {
		action, _, err := c.Server.EnableBackup(ctx, server, "")
		if err != nil {
			return err
		}
		if err := hcclient.WaitForAction(ctx, &c.Action, action); err != nil {
			return err
		}
	}
	return nil
}

func setISO(ctx context.Context, c *hcloud.Client, server *hcloud.Server, isoIDOrName string) error {
	isoChange := false
	if server.ISO != nil {
		isoChange = true
		action, _, err := c.Server.DetachISO(ctx, server)
		if err != nil {
			return err
		}
		if err := hcclient.WaitForAction(ctx, &c.Action, action); err != nil {
			return err
		}
	}
	if isoIDOrName != "" {
		isoChange = true

		iso, _, err := c.ISO.Get(ctx, isoIDOrName)
		if err != nil {
			return err
		}

		if iso == nil {
			return fmt.Errorf("ISO not found: %s", isoIDOrName)
		}

		a, _, err := c.Server.AttachISO(ctx, server, iso)
		if err != nil {
			return err
		}
		if err := hcclient.WaitForAction(ctx, &c.Action, a); err != nil {
			return err
		}
	}

	if isoChange {
		a, _, err := c.Server.Reset(ctx, server)
		if err != nil {
			return err
		}
		if err := hcclient.WaitForAction(ctx, &c.Action, a); err != nil {
			return err
		}
	}
	return nil
}

func setRescue(ctx context.Context, c *hcloud.Client, server *hcloud.Server, rescue string, sshKeys []*hcloud.SSHKey) error {
	const op = "hcloud/setRescue"

	rescueChanged := false
	if server.RescueEnabled {
		rescueChanged = true
		a, _, err := c.Server.DisableRescue(ctx, server)
		if err != nil {
			return fmt.Errorf("%s: %v", op, err)
		}
		if err := hcclient.WaitForAction(ctx, &c.Action, a); err != nil {
			return fmt.Errorf("%s: %v", op, err)
		}
	}
	if rescue != "" {
		rescueChanged = true
		err := control.Retry(control.DefaultRetries, func() error {
			res, _, err := c.Server.EnableRescue(ctx, server, hcloud.ServerEnableRescueOpts{
				Type:    hcloud.ServerRescueType(rescue),
				SSHKeys: sshKeys,
			})
			if err != nil {
				return err
			}
			return hcclient.WaitForAction(ctx, &c.Action, res.Action)
		})
		if err != nil {
			return fmt.Errorf("%s: %v", op, err)
		}
	}
	if rescueChanged {
		action, _, err := c.Server.Reset(ctx, server)
		if err != nil {
			return fmt.Errorf("%s: %v", op, err)
		}
		if err := hcclient.WaitForAction(ctx, &c.Action, action); err != nil {
			return fmt.Errorf("%s: %v", op, err)
		}
	}
	return nil
}

func getSSHkeys(ctx context.Context, client *hcloud.Client, d *schema.ResourceData) (sshKeys []*hcloud.SSHKey, err error) {
	for _, sshKeyValue := range d.Get("ssh_keys").([]interface{}) {
		sshKeyIDOrName := sshKeyValue.(string)
		var sshKey *hcloud.SSHKey
		sshKey, _, err = client.SSHKey.Get(ctx, sshKeyIDOrName)
		if err != nil {
			return
		}
		if sshKey == nil {
			err = fmt.Errorf("SSH key not found: %s", sshKeyIDOrName)
			return
		}
		sshKeys = append(sshKeys, sshKey)
	}
	return
}

func inlineAttachServerToNetwork(ctx context.Context, c *hcloud.Client, s *hcloud.Server, nwData map[string]interface{}) error {
	const op = "hcloud/inlineAttachServerToNetwork"
	var aliasIPs []net.IP

	nw := &hcloud.Network{ID: nwData["network_id"].(int)}
	ip := net.ParseIP(nwData["ip"].(string))
	for _, v := range nwData["alias_ips"].(*schema.Set).List() {
		aliasIP := net.ParseIP(v.(string))
		aliasIPs = append(aliasIPs, aliasIP)
	}
	if err := attachServerToNetwork(ctx, c, s, nw, ip, aliasIPs); err != nil {
		return fmt.Errorf("%s: %v", op, err)
	}

	return nil
}

func updateServerInlineNetworkAttachments(ctx context.Context, c *hcloud.Client, data *schema.Set, s *hcloud.Server) error {
	const op = "hcloud/updateServerInlineNetworkAttachments"

	log.Printf("[INFO] Updating inline network attachments for server %d", s.ID)

	cfgNetworks := make(map[int]map[string]interface{}, data.Len())
	for _, v := range data.List() {
		nwData := v.(map[string]interface{})
		nwID := nwData["network_id"].(int)
		cfgNetworks[nwID] = nwData
	}

	for _, n := range s.PrivateNet {
		nwData, ok := cfgNetworks[n.Network.ID]
		if !ok {
			// The server should no longer be a member of this network.
			// Detach it.
			if err := detachServerFromNetwork(ctx, c, s, n.Network); err != nil {
				return fmt.Errorf("%s: %v", op, err)
			}
			continue
		}
		// Remove the network from the cfgNetworks map. We are going to
		// handle it right now.
		delete(cfgNetworks, n.Network.ID)

		if nwData["ip"].(string) != n.IP.String() {
			// IP changed. Our API provides now way to change this. So we
			// need to detach and re-attach. Alias IPs are updated, too. This
			// saves us from the next step.
			if err := detachServerFromNetwork(ctx, c, s, n.Network); err != nil {
				return fmt.Errorf("%s: %v", op, err)
			}
			if err := inlineAttachServerToNetwork(ctx, c, s, nwData); err != nil {
				return fmt.Errorf("%s: %v", op, err)
			}
			continue
		}
		cfgAliasIPs := nwData["alias_ips"].(*schema.Set)
		curAliasIPs := newIPSet(cfgAliasIPs.F, n.Aliases)
		if !cfgAliasIPs.Equal(curAliasIPs) {
			if err := updateServerAliasIPs(ctx, c, s, n.Network, cfgAliasIPs); err != nil {
				return fmt.Errorf("%s: %v", op, err)
			}
			continue
		}
	}

	// Whatever remains in cfgNetworks now is a newly added network. We attach
	// the server to it.
	for _, nwData := range cfgNetworks {
		if err := inlineAttachServerToNetwork(ctx, c, s, nwData); err != nil {
			return fmt.Errorf("%s: %v", op, err)
		}
	}

	return nil
}

func newIPSet(f schema.SchemaSetFunc, ips []net.IP) *schema.Set {
	ss := make([]interface{}, len(ips))
	for i, ip := range ips {
		ss[i] = ip.String()
	}
	return schema.NewSet(f, ss)
}

func setServerSchema(d *schema.ResourceData, s *hcloud.Server) {
	d.SetId(strconv.Itoa(s.ID))
	d.Set("name", s.Name)
	d.Set("datacenter", s.Datacenter.Name)
	d.Set("location", s.Datacenter.Location.Name)
	d.Set("status", s.Status)
	d.Set("server_type", s.ServerType.Name)
	d.Set("ipv4_address", s.PublicNet.IPv4.IP.String())
	d.Set("ipv6_address", s.PublicNet.IPv6.IP.String()+"1")
	d.Set("ipv6_network", s.PublicNet.IPv6.Network.String())
	d.Set("backup_window", s.BackupWindow)
	d.Set("backups", s.BackupWindow != "")
	d.Set("labels", s.Labels)
	if s.Image != nil {
		if s.Image.Name != "" {
			d.Set("image", s.Image.Name)
		} else {
			d.Set("image", fmt.Sprintf("%d", s.Image.ID))
		}
	}
}
