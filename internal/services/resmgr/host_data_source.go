// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package resmgr

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/platform9/terraform-provider-pcd/internal/clients"
)

var (
	_ datasource.DataSource              = (*hostDataSource)(nil)
	_ datasource.DataSourceWithConfigure = (*hostDataSource)(nil)
)

// NewHostDataSource is the factory registered with the provider.
func NewHostDataSource() datasource.DataSource {
	return &hostDataSource{}
}

type hostDataSource struct {
	config *clients.Config
}

type hostDataSourceModel struct {
	ID           types.String `tfsdk:"id"`
	Name         types.String `tfsdk:"name"`
	Roles        types.List   `tfsdk:"roles"`
	HostConfigID types.String `tfsdk:"host_config_id"`
	Responding   types.Bool   `tfsdk:"responding"`
}

func (d *hostDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_host"
}

func (d *hostDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Looks up a host known to the PCD resource manager by the hostname it reports and returns its " +
			"resmgr UUID: the `host_id` that `pcd_host_config_assignment`, `pcd_host_cluster_role`, and `pcd_host_role` take. " +
			"A host appears here once its host agent has been authorized and has reported in; until then it has no hostname.",
		Attributes: map[string]schema.Attribute{
			"name": schema.StringAttribute{Required: true, MarkdownDescription: "The hostname the host agent reports, matched exactly. " +
				"Hosts usually report their fully qualified name (`hostname -f` on the host; the Hosts page in the PCD UI shows the same value)."},
			"id": schema.StringAttribute{Computed: true, MarkdownDescription: "The resmgr host UUID."},
			"roles": schema.ListAttribute{Computed: true, ElementType: types.StringType, MarkdownDescription: "The cluster roles assigned to the host " +
				"(`hypervisor`, `image-library`, `persistent-storage`, `dns`); empty for a host that has only been authorized."},
			"host_config_id": schema.StringAttribute{Computed: true, MarkdownDescription: "The host configuration assigned to the host, or empty when none is."},
			"responding":     schema.BoolAttribute{Computed: true, MarkdownDescription: "Whether the host agent is currently reporting to the control plane."},
		},
	}
}

func (d *hostDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	d.config = configureClient(req.ProviderData, &resp.Diagnostics)
}

func (d *hostDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var data hostDataSourceModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := d.config.ResmgrV2Client()
	if err != nil {
		resp.Diagnostics.AddError("resmgr: building client", err.Error())
		return
	}

	// The host list is the authority (see hostRecord): it keeps reporting a host
	// through the windows in which the per-host endpoint answers 404.
	var hosts []hostAPI
	if err := getJSONList(ctx, client, client.ServiceURL("hosts"), &hosts); err != nil {
		resp.Diagnostics.AddError("resmgr: listing hosts", err.Error())
		return
	}

	host, err := hostByName(hosts, data.Name.ValueString())
	if err != nil {
		resp.Diagnostics.AddAttributeError(path.Root("name"), "Host not found", err.Error())
		return
	}

	data, diags := hostDataFromAPI(ctx, host)
	resp.Diagnostics.Append(diags...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}

// hostByName returns the one host whose reported hostname equals name. The
// match is exact: hosts usually report an FQDN, and a short name that happened
// to match one host today could match two tomorrow. A host that has not
// reported yet carries no info block and so can never match.
func hostByName(hosts []hostAPI, name string) (hostAPI, error) {
	var matches []hostAPI
	var known []string
	for _, h := range hosts {
		if h.Info == nil || h.Info.Hostname == "" {
			continue
		}
		known = append(known, h.Info.Hostname)
		if h.Info.Hostname == name {
			matches = append(matches, h)
		}
	}
	switch len(matches) {
	case 1:
		return matches[0], nil
	case 0:
		sort.Strings(known)
		list := "none"
		if len(known) > 0 {
			list = strings.Join(known, ", ")
		}
		return hostAPI{}, fmt.Errorf("no host named %q. The resource manager reports these hostnames: %s. "+
			"The name must match exactly (hosts usually report their fully qualified name), and a host that "+
			"has not reported to the control plane yet has no hostname", name, list)
	default:
		ids := make([]string, 0, len(matches))
		for _, h := range matches {
			ids = append(ids, h.ID)
		}
		return hostAPI{}, fmt.Errorf("%d hosts are named %q (ids %s); the name does not identify one host", len(matches), name, strings.Join(ids, ", "))
	}
}

// hostDataFromAPI maps a host record onto the data source model with every
// attribute known: nil roles become an empty list and a missing info block
// reads as not responding.
func hostDataFromAPI(ctx context.Context, h hostAPI) (hostDataSourceModel, diag.Diagnostics) {
	roles := h.Roles
	if roles == nil {
		roles = []string{}
	}
	rv, diags := types.ListValueFrom(ctx, types.StringType, roles)
	m := hostDataSourceModel{
		ID:           types.StringValue(h.ID),
		Name:         types.StringValue(""),
		Roles:        rv,
		HostConfigID: types.StringValue(h.HostConfigID),
		Responding:   types.BoolValue(false),
	}
	if h.Info != nil {
		m.Name = types.StringValue(h.Info.Hostname)
		m.Responding = types.BoolValue(h.Info.Responding)
	}
	return m, diags
}
