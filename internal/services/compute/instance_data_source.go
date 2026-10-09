// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0
//
// Ported from terraform-provider-openstack v3.4.0
// (openstack/data_source_openstack_compute_instance_v2.go), adapted for the
// terraform-plugin-framework and PCD. Unlike upstream, it looks an instance up
// by name as well as by ID, fills network uuid and port from Neutron, and does
// not fail on statuses outside a fixed list.

package compute

import (
	"context"
	"fmt"
	"net/http"
	"net/netip"
	"sort"
	"strings"
	"time"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/flavors"
	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/servers"
	"github.com/gophercloud/gophercloud/v2/openstack/image/v2/images"
	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/ports"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/platform9/terraform-provider-pcd/internal/clients"
)

// computeMicroversionServerTags is the microversion the instance data source
// reads at: 2.26 adds the server's tags to its body, and anything below 2.47
// still reports the flavor by ID.
const computeMicroversionServerTags = "2.26"

var (
	_ datasource.DataSource              = (*instanceDataSource)(nil)
	_ datasource.DataSourceWithConfigure = (*instanceDataSource)(nil)
)

// NewInstanceDataSource is the factory registered with the provider.
func NewInstanceDataSource() datasource.DataSource {
	return &instanceDataSource{}
}

type instanceDataSource struct {
	config *clients.Config
}

type instanceDataSourceModel struct {
	ID                 types.String `tfsdk:"id"`
	InstanceID         types.String `tfsdk:"instance_id"`
	Name               types.String `tfsdk:"name"`
	ProjectID          types.String `tfsdk:"project_id"`
	UserID             types.String `tfsdk:"user_id"`
	Status             types.String `tfsdk:"status"`
	VMState            types.String `tfsdk:"vm_state"`
	ImageID            types.String `tfsdk:"image_id"`
	ImageName          types.String `tfsdk:"image_name"`
	FlavorID           types.String `tfsdk:"flavor_id"`
	FlavorName         types.String `tfsdk:"flavor_name"`
	KeyPair            types.String `tfsdk:"key_pair"`
	SecurityGroups     types.Set    `tfsdk:"security_groups"`
	AvailabilityZone   types.String `tfsdk:"availability_zone"`
	AccessIPv4         types.String `tfsdk:"access_ip_v4"`
	AccessIPv6         types.String `tfsdk:"access_ip_v6"`
	Metadata           types.Map    `tfsdk:"metadata"`
	MigrationPriority  types.String `tfsdk:"migration_priority"`
	Tags               types.Set    `tfsdk:"tags"`
	Host               types.String `tfsdk:"host"`
	HypervisorHostname types.String `tfsdk:"hypervisor_hostname"`
	Created            types.String `tfsdk:"created"`
	Updated            types.String `tfsdk:"updated"`
	Network            types.List   `tfsdk:"network"`
	Region             types.String `tfsdk:"region"`
}

type instanceDataSourceNetworkModel struct {
	UUID      types.String `tfsdk:"uuid"`
	Name      types.String `tfsdk:"name"`
	Port      types.String `tfsdk:"port"`
	FixedIPv4 types.String `tfsdk:"fixed_ip_v4"`
	FixedIPv6 types.String `tfsdk:"fixed_ip_v6"`
	MAC       types.String `tfsdk:"mac"`
}

// instanceDataSourceNetworkType is the element type of the network list.
var instanceDataSourceNetworkType = types.ObjectType{AttrTypes: map[string]attr.Type{
	"uuid":        types.StringType,
	"name":        types.StringType,
	"port":        types.StringType,
	"fixed_ip_v4": types.StringType,
	"fixed_ip_v6": types.StringType,
	"mac":         types.StringType,
}}

func (d *instanceDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_compute_instance"
}

func (d *instanceDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	computed := func(desc string) schema.StringAttribute {
		return schema.StringAttribute{Computed: true, MarkdownDescription: desc}
	}
	resp.Schema = schema.Schema{
		MarkdownDescription: "Look up an existing instance by ID or by exact name, including one Terraform does not " +
			"manage (created in the PCD UI, say), to reference its ID, ports and addresses elsewhere. Set " +
			"`instance_id` or `name`; `instance_id` takes precedence. A name must match exactly one instance in the " +
			"project the provider is scoped to (or in `project_id`). Reading an instance does not manage it: to take " +
			"it over, import it into `pcd_compute_instance`.",
		Attributes: map[string]schema.Attribute{
			"id": computed("The instance ID."),
			"instance_id": schema.StringAttribute{Optional: true, Computed: true,
				MarkdownDescription: "Look up by instance ID (takes precedence over `name`)."},
			"name": schema.StringAttribute{Optional: true, Computed: true,
				MarkdownDescription: "Look up by exact name. Zero matches, or more than one, is an error; use " +
					"`instance_id` for a name that is not unique. With `instance_id`, it must match the instance's name."},
			"project_id": schema.StringAttribute{Optional: true, Computed: true,
				MarkdownDescription: "The instance's project. With `name`, searches this project instead of the " +
					"provider's (needs the admin role). With `instance_id`, the instance must be in it."},
			"user_id":  computed("The ID of the user that owns the instance."),
			"status":   computed("The Nova status, as `pcd_compute_instance` reports it (for example `ACTIVE`, `SHUTOFF`, `ERROR`)."),
			"vm_state": computed("The Nova VM state the PCD UI shows (for example `active`, `stopped`, `suspended`)."),
			"image_id": computed("The image the instance was booted from; `\"\"` for an instance booted from a volume."),
			"image_name": computed("The name of `image_id`; `\"\"` when the instance was booted from a volume, the image " +
				"is gone, or Glance cannot be reached."),
			"flavor_id": computed("The flavor ID."),
			"flavor_name": computed("The flavor name; `\"\"` when the flavor has been deleted or is a private flavor " +
				"the caller cannot see (Nova answers both with a 404)."),
			"key_pair": computed("The key pair injected at boot; `\"\"` when none."),
			"security_groups": schema.SetAttribute{Computed: true, ElementType: types.StringType,
				MarkdownDescription: "The names of the security groups on the instance's ports."},
			"availability_zone": computed("The availability zone."),
			"access_ip_v4": computed("The instance's accessIPv4 when set; otherwise the first IPv4 fixed address in " +
				"`network` order."),
			"access_ip_v6": computed("The instance's accessIPv6 when set; otherwise the first IPv6 fixed address in " +
				"`network` order."),
			"metadata": schema.MapAttribute{Computed: true, ElementType: types.StringType,
				MarkdownDescription: "The instance metadata, without the `migration-priority` key."},
			"migration_priority": computed("The Dynamic Resource Rebalancing priority (`normal`, `low`, `high` or `never`); " +
				"`\"\"` when unset."),
			"tags": schema.SetAttribute{Computed: true, ElementType: types.StringType,
				MarkdownDescription: "The server tags."},
			"host": computed("The compute host the instance runs on (Nova's `OS-EXT-SRV-ATTR:host`; on PCD, the host's ID). " +
				"`\"\"` when the cloud's policy does not show it to the caller, which by default means a caller " +
				"without the admin role."),
			"hypervisor_hostname": computed("The hostname of the hypervisor the instance runs on; `\"\"` when the cloud's " +
				"policy does not show it to the caller, as for `host`."),
			"created": computed("When the instance was created (RFC3339)."),
			"updated": computed("When the instance last changed (RFC3339)."),
			"network": schema.ListNestedAttribute{
				Computed: true,
				MarkdownDescription: "The instance's ports, oldest first, with the network each is on, in an order " +
					"that stays the same across reads. Neutron records creation times to the second, so ports created " +
					"in the same second, as the ports Nova creates at boot often are, are ordered by port ID rather " +
					"than boot order: on an instance with several ports, select the entry by `uuid` or `name` rather " +
					"than by index. `port` is what `pcd_networking_floatingip_associate` and " +
					"`pcd_networking_port_secgroup_associate` take.",
				NestedObject: schema.NestedAttributeObject{Attributes: map[string]schema.Attribute{
					"uuid":        computed("The network ID."),
					"name":        computed("The network name; `\"\"` for a port with no address."),
					"port":        computed("The port ID."),
					"fixed_ip_v4": computed("The port's first IPv4 address; `\"\"` when none."),
					"fixed_ip_v6": computed("The port's first IPv6 address; `\"\"` when none."),
					"mac":         computed("The port's MAC address."),
				}},
			},
			"region": schema.StringAttribute{Optional: true, Computed: true,
				MarkdownDescription: "The region. Defaults to the provider's region."},
		},
	}
}

func (d *instanceDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	d.config = configureClient(req.ProviderData, &resp.Diagnostics)
}

func (d *instanceDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var cfg instanceDataSourceModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &cfg)...)
	if resp.Diagnostics.HasError() {
		return
	}

	base, err := d.config.ForRegion(cfg.Region.ValueString()).ComputeV2Client()
	if err != nil {
		resp.Diagnostics.AddError("compute: building v2 client", err.Error())
		return
	}
	server := lookupInstance(ctx, withMicroversion(base, computeMicroversionServerTags), &cfg, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}
	if v := cfg.ProjectID.ValueString(); v != "" && server.TenantID != v {
		resp.Diagnostics.AddAttributeError(path.Root("project_id"), "Instance is in another project",
			fmt.Sprintf("Instance %s is in project %s, not %s.", server.ID, server.TenantID, v))
		return
	}
	if v := cfg.Name.ValueString(); v != "" && server.Name != v {
		resp.Diagnostics.AddAttributeError(path.Root("name"), "Instance name does not match",
			fmt.Sprintf("Instance %s is named %q, not %q. instance_id takes precedence over name; correct or "+
				"remove name.", server.ID, server.Name, v))
		return
	}

	flavorID, _ := server.Flavor["id"].(string)
	flavorName := ""
	if flavorID != "" {
		// A 200 without the object is ErrNoObject, not a deleted flavor.
		flavor, err := clients.RequireObject(flavors.Get(ctx, base, flavorID).Extract())
		switch {
		case err == nil:
			flavorName = flavor.Name
		case !gophercloud.ResponseCodeIs(err, http.StatusNotFound):
			resp.Diagnostics.AddError("compute: reading the instance's flavor", err.Error())
			return
		}
	}

	imageID := serverImageID(server)
	imageName := ""
	if imageID != "" {
		// Best effort: the image may be gone, and the provider's Glance endpoint
		// is not reachable from everywhere; image_id is what matters.
		if imageClient, err := d.config.ForRegion(cfg.Region.ValueString()).ImageV2Client(); err == nil {
			if img, err := clients.RequireObject(images.Get(ctx, imageClient, imageID).Extract()); err == nil {
				imageName = img.Name
			}
		}
	}

	networkClient, err := d.config.ForRegion(cfg.Region.ValueString()).NetworkV2Client()
	if err != nil {
		resp.Diagnostics.AddError("networking: building v2 client", err.Error())
		return
	}
	pages, err := ports.List(networkClient, ports.ListOpts{DeviceID: server.ID}).AllPages(ctx)
	if err != nil {
		resp.Diagnostics.AddError("networking: listing the instance's ports", err.Error())
		return
	}
	portList, err := ports.ExtractPorts(pages)
	if err != nil {
		resp.Diagnostics.AddError("networking: reading the instance's ports", err.Error())
		return
	}

	region := cfg.Region.ValueString()
	if region == "" {
		region = d.config.Region
	}
	state := instanceDataSourceState(ctx, server, flavorName, imageID, imageName, portList, region, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

// lookupInstance finds the instance by instance_id, or else by exact name.
// Nova's name filter is a database regular expression that may also ignore
// case, so only an exact, case-sensitive match from the list counts.
func lookupInstance(ctx context.Context, client *gophercloud.ServiceClient, cfg *instanceDataSourceModel, diags *diag.Diagnostics) *servers.Server {
	if id := cfg.InstanceID.ValueString(); id != "" {
		server, err := clients.RequireObject(servers.Get(ctx, client, id).Extract())
		// servers.Get decodes a 200 without the object to a zeroed server, not
		// to nil, so RequireObject passes it; its empty ID would list every port
		// and be saved as the data source's ID. It is not a missing instance.
		if err == nil && server.ID == "" {
			err = clients.ErrNoObject
		}
		if err != nil {
			if gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
				diags.AddAttributeError(path.Root("instance_id"), "No instance found",
					fmt.Sprintf("No instance with ID %q.", id))
				return nil
			}
			diags.AddError("compute: reading instance", err.Error())
			return nil
		}
		return server
	}

	name := cfg.Name.ValueString()
	if name == "" {
		diags.AddError("Missing lookup key", "Set instance_id or name.")
		return nil
	}
	opts := servers.ListOpts{}
	if nameRegexSafe(name) {
		opts.Name = "^" + name + "$"
	}
	if project := cfg.ProjectID.ValueString(); project != "" {
		opts.AllTenants = true
		opts.TenantID = project
	}
	pages, err := servers.List(client, opts).AllPages(ctx)
	if err != nil {
		detail := err.Error()
		if opts.AllTenants && gophercloud.ResponseCodeIs(err, http.StatusForbidden) {
			detail += "\n\n`project_id` needs the admin role."
		}
		diags.AddError("compute: listing instances", detail)
		return nil
	}
	all, err := servers.ExtractServers(pages)
	if err != nil {
		diags.AddError("compute: reading instances", err.Error())
		return nil
	}
	var matches []servers.Server
	for _, s := range all {
		if s.Name == name {
			matches = append(matches, s)
		}
	}
	switch len(matches) {
	case 0:
		diags.AddAttributeError(path.Root("name"), "No instance found", fmt.Sprintf("No instance named %q.", name))
		return nil
	case 1:
		return &matches[0]
	default:
		ids := make([]string, 0, len(matches))
		for _, s := range matches {
			ids = append(ids, s.ID)
		}
		sort.Strings(ids)
		diags.AddAttributeError(path.Root("name"), "Multiple instances found",
			fmt.Sprintf("%d instances are named %q (%s); set instance_id instead.", len(matches), name, strings.Join(ids, ", ")))
		return nil
	}
}

// nameRegexSafe reports whether name, sent as Nova's name regular expression,
// still matches itself. "." matches any character, itself included, but the
// other metacharacters can make a name miss itself; such a name is not sent,
// and the project's instances are listed and matched here instead.
func nameRegexSafe(name string) bool {
	return !strings.ContainsAny(name, `\+*?()|[]{}^$`)
}

// instanceNIC is one port of an instance, flattened for the network list.
type instanceNIC struct {
	NetworkID, NetworkName, PortID, FixedIPv4, FixedIPv6, MAC string
}

// instanceNICs lists the instance's ports oldest first (ties by port ID), so
// network[0] does not move between reads; Neutron's list order and Go's map
// order are not stable. Neutron's created_at has whole seconds, so ports Nova
// creates at boot often tie, and the port ID order is not the boot order. Each port's network name comes from Nova's addresses,
// which are keyed by network name and carry each address's MAC.
func instanceNICs(addresses map[string]any, portList []ports.Port) []instanceNIC {
	netByMAC := map[string]string{}
	for netName, v := range addresses {
		list, _ := v.([]any)
		for _, a := range list {
			entry, _ := a.(map[string]any)
			if mac, ok := entry["OS-EXT-IPS-MAC:mac_addr"].(string); ok && mac != "" {
				netByMAC[strings.ToLower(mac)] = netName
			}
		}
	}

	sorted := append([]ports.Port(nil), portList...)
	sort.SliceStable(sorted, func(i, j int) bool {
		if !sorted[i].CreatedAt.Equal(sorted[j].CreatedAt) {
			return sorted[i].CreatedAt.Before(sorted[j].CreatedAt)
		}
		return sorted[i].ID < sorted[j].ID
	})

	nics := make([]instanceNIC, 0, len(sorted))
	for _, p := range sorted {
		nic := instanceNIC{
			NetworkID:   p.NetworkID,
			NetworkName: netByMAC[strings.ToLower(p.MACAddress)],
			PortID:      p.ID,
			MAC:         p.MACAddress,
		}
		for _, ip := range p.FixedIPs {
			addr, err := netip.ParseAddr(ip.IPAddress)
			if err != nil {
				continue
			}
			if addr.Is4() && nic.FixedIPv4 == "" {
				nic.FixedIPv4 = ip.IPAddress
			}
			if !addr.Is4() && nic.FixedIPv6 == "" {
				nic.FixedIPv6 = ip.IPAddress
			}
		}
		nics = append(nics, nic)
	}
	return nics
}

// instanceAccessIPs returns the server's accessIPv4/accessIPv6 when set, and
// otherwise the first fixed address of each family in network order.
// (pcd_compute_instance's firstIPv4 walks a map, so its pick can change between
// reads; a data source re-read on every plan cannot afford that.)
func instanceAccessIPs(server *servers.Server, nics []instanceNIC) (v4, v6 string) {
	v4, v6 = server.AccessIPv4, server.AccessIPv6
	for _, nic := range nics {
		if v4 == "" {
			v4 = nic.FixedIPv4
		}
		if v6 == "" {
			v6 = nic.FixedIPv6
		}
	}
	return v4, v6
}

// instanceSecurityGroupNames returns the server's security group names once
// each: Nova lists a group once per port that carries it.
func instanceSecurityGroupNames(server *servers.Server) []string {
	seen := map[string]bool{}
	names := []string{}
	for _, sg := range server.SecurityGroups {
		if name, ok := sg["name"].(string); ok && !seen[name] {
			seen[name] = true
			names = append(names, name)
		}
	}
	return names
}

// instanceDataSourceState flattens what Read gathered into the model.
func instanceDataSourceState(ctx context.Context, server *servers.Server, flavorName, imageID, imageName string,
	portList []ports.Port, region string, diags *diag.Diagnostics) instanceDataSourceModel {
	flavorID, _ := server.Flavor["id"].(string)
	nics := instanceNICs(server.Addresses, portList)
	accessV4, accessV6 := instanceAccessIPs(server, nics)
	meta, prio := splitMigrationPriority(server.Metadata)

	tagVals := []string{}
	if server.Tags != nil {
		tagVals = *server.Tags
	}

	networks := make([]instanceDataSourceNetworkModel, 0, len(nics))
	for _, nic := range nics {
		networks = append(networks, instanceDataSourceNetworkModel{
			UUID:      types.StringValue(nic.NetworkID),
			Name:      types.StringValue(nic.NetworkName),
			Port:      types.StringValue(nic.PortID),
			FixedIPv4: types.StringValue(nic.FixedIPv4),
			FixedIPv6: types.StringValue(nic.FixedIPv6),
			MAC:       types.StringValue(nic.MAC),
		})
	}

	m := instanceDataSourceModel{
		ID:                 types.StringValue(server.ID),
		InstanceID:         types.StringValue(server.ID),
		Name:               types.StringValue(server.Name),
		ProjectID:          types.StringValue(server.TenantID),
		UserID:             types.StringValue(server.UserID),
		Status:             types.StringValue(server.Status),
		VMState:            types.StringValue(server.VmState),
		ImageID:            types.StringValue(imageID),
		ImageName:          types.StringValue(imageName),
		FlavorID:           types.StringValue(flavorID),
		FlavorName:         types.StringValue(flavorName),
		KeyPair:            types.StringValue(server.KeyName),
		AvailabilityZone:   types.StringValue(server.AvailabilityZone),
		AccessIPv4:         types.StringValue(accessV4),
		AccessIPv6:         types.StringValue(accessV6),
		MigrationPriority:  types.StringValue(prio),
		Host:               types.StringValue(server.Host),
		HypervisorHostname: types.StringValue(server.HypervisorHostname),
		Created:            types.StringValue(server.Created.Format(time.RFC3339)),
		Updated:            types.StringValue(server.Updated.Format(time.RFC3339)),
		Region:             types.StringValue(region),
	}
	var d diag.Diagnostics
	m.SecurityGroups, d = types.SetValueFrom(ctx, types.StringType, instanceSecurityGroupNames(server))
	diags.Append(d...)
	m.Metadata, d = types.MapValueFrom(ctx, types.StringType, meta)
	diags.Append(d...)
	m.Tags, d = types.SetValueFrom(ctx, types.StringType, tagVals)
	diags.Append(d...)
	m.Network, d = types.ListValueFrom(ctx, instanceDataSourceNetworkType, networks)
	diags.Append(d...)
	return m
}
