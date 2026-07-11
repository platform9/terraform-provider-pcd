// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0
//
// Ported from terraform-provider-openstack v3.4.0
// (openstack/data_source_openstack_networking_network_v2.go), adapted for the
// terraform-plugin-framework and PCD.

package networking

import (
	"context"
	"fmt"

	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/networks"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/platform9/terraform-provider-pcd/internal/clients"
)

var (
	_ datasource.DataSource              = (*networkDataSource)(nil)
	_ datasource.DataSourceWithConfigure = (*networkDataSource)(nil)
)

// NewNetworkDataSource is the factory registered with the provider.
func NewNetworkDataSource() datasource.DataSource {
	return &networkDataSource{}
}

type networkDataSource struct {
	config *clients.Config
}

type networkDataSourceModel struct {
	ID           types.String `tfsdk:"id"`
	NetworkID    types.String `tfsdk:"network_id"`
	Name         types.String `tfsdk:"name"`
	Description  types.String `tfsdk:"description"`
	AdminStateUp types.Bool   `tfsdk:"admin_state_up"`
	Shared       types.Bool   `tfsdk:"shared"`
	External     types.Bool   `tfsdk:"external"`
	TenantID     types.String `tfsdk:"tenant_id"`
	Region       types.String `tfsdk:"region"`
}

func (d *networkDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_networking_network"
}

func (d *networkDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Look up a Neutron network by name or ID.",
		Attributes: map[string]schema.Attribute{
			"id":             schema.StringAttribute{Computed: true, MarkdownDescription: "The network ID."},
			"network_id":     schema.StringAttribute{Optional: true, MarkdownDescription: "Look up by network ID (takes precedence over name)."},
			"name":           schema.StringAttribute{Optional: true, MarkdownDescription: "Look up by name."},
			"description":    schema.StringAttribute{Computed: true, MarkdownDescription: "The network description."},
			"admin_state_up": schema.BoolAttribute{Computed: true, MarkdownDescription: "The administrative state."},
			"shared":         schema.BoolAttribute{Computed: true, MarkdownDescription: "Whether the network is shared."},
			"external":       schema.BoolAttribute{Computed: true, MarkdownDescription: "Whether the network is external."},
			"tenant_id":      schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "Filter by (and report) the owning project."},
			"region":         schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "The region. Defaults to the provider's region."},
		},
	}
}

func (d *networkDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	d.config = configureClient(req.ProviderData, &resp.Diagnostics)
}

func (d *networkDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var data networkDataSourceModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := d.config.NetworkV2Client()
	if err != nil {
		resp.Diagnostics.AddError("networking: building v2 client", err.Error())
		return
	}

	id := data.NetworkID.ValueString()
	if id == "" {
		pages, err := networks.List(client, networks.ListOpts{
			Name:     data.Name.ValueString(),
			TenantID: data.TenantID.ValueString(),
		}).AllPages(ctx)
		if err != nil {
			resp.Diagnostics.AddError("networking: listing networks", err.Error())
			return
		}
		all, err := networks.ExtractNetworks(pages)
		if err != nil {
			resp.Diagnostics.AddError("networking: extracting networks", err.Error())
			return
		}
		switch len(all) {
		case 0:
			resp.Diagnostics.AddError("No network found", "No network matched the given criteria.")
			return
		case 1:
			id = all[0].ID
		default:
			resp.Diagnostics.AddError("Multiple networks found",
				fmt.Sprintf("%d networks matched; refine name/tenant_id.", len(all)))
			return
		}
	}

	var n networkExtended
	if err := networks.Get(ctx, client, id).ExtractInto(&n); err != nil {
		resp.Diagnostics.AddError("networking: getting network", err.Error())
		return
	}

	data.ID = types.StringValue(n.ID)
	data.NetworkID = types.StringValue(n.ID)
	data.Name = types.StringValue(n.Name)
	data.Description = types.StringValue(n.Description)
	data.AdminStateUp = types.BoolValue(n.AdminStateUp)
	data.Shared = types.BoolValue(n.Shared)
	data.External = types.BoolValue(n.NetworkExternalExt.External)
	data.TenantID = types.StringValue(n.TenantID)
	if data.Region.IsNull() || data.Region.IsUnknown() {
		data.Region = types.StringValue(d.config.Region)
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}
