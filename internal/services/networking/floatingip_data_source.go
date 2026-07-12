// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0
//
// Ported from terraform-provider-openstack v3.4.0
// (openstack/data_source_openstack_networking_floatingip_v2.go), adapted for the
// terraform-plugin-framework and PCD.

package networking

import (
	"context"
	"fmt"

	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/extensions/layer3/floatingips"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/platform9/terraform-provider-pcd/internal/clients"
)

var (
	_ datasource.DataSource              = (*floatingIPDataSource)(nil)
	_ datasource.DataSourceWithConfigure = (*floatingIPDataSource)(nil)
)

// NewFloatingIPDataSource is the factory registered with the provider.
func NewFloatingIPDataSource() datasource.DataSource {
	return &floatingIPDataSource{}
}

type floatingIPDataSource struct {
	config *clients.Config
}

type floatingIPDataSourceModel struct {
	ID                types.String `tfsdk:"id"`
	Address           types.String `tfsdk:"address"`
	Description       types.String `tfsdk:"description"`
	FloatingNetworkID types.String `tfsdk:"floating_network_id"`
	PortID            types.String `tfsdk:"port_id"`
	FixedIP           types.String `tfsdk:"fixed_ip"`
	Status            types.String `tfsdk:"status"`
	RouterID          types.String `tfsdk:"router_id"`
	TenantID          types.String `tfsdk:"tenant_id"`
	Region            types.String `tfsdk:"region"`
}

func (d *floatingIPDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_networking_floatingip"
}

func (d *floatingIPDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Look up a floating IP by address or filters. Exactly one floating IP must match.",
		Attributes: map[string]schema.Attribute{
			"id":                  schema.StringAttribute{Computed: true, MarkdownDescription: "The floating IP ID."},
			"address":             schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "Filter by (and report) the floating IP address."},
			"description":         schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "Filter by (and report) the description."},
			"floating_network_id": schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "Filter by (and report) the external network ID."},
			"port_id":             schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "Filter by (and report) the associated port."},
			"fixed_ip":            schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "Filter by (and report) the mapped fixed IP."},
			"status":              schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "Filter by (and report) the operational status."},
			"router_id":           schema.StringAttribute{Computed: true, MarkdownDescription: "The router through which the floating IP is routed."},
			"tenant_id":           schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "Filter by (and report) the owning project."},
			"region":              schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "The region. Defaults to the provider's region."},
		},
	}
}

func (d *floatingIPDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	d.config = configureClient(req.ProviderData, &resp.Diagnostics)
}

func (d *floatingIPDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var data floatingIPDataSourceModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := d.config.NetworkV2Client()
	if err != nil {
		resp.Diagnostics.AddError("networking: building v2 client", err.Error())
		return
	}

	listOpts := floatingips.ListOpts{
		FloatingIP:        data.Address.ValueString(),
		Description:       data.Description.ValueString(),
		FloatingNetworkID: data.FloatingNetworkID.ValueString(),
		PortID:            data.PortID.ValueString(),
		FixedIP:           data.FixedIP.ValueString(),
		Status:            data.Status.ValueString(),
		TenantID:          data.TenantID.ValueString(),
	}
	pages, err := floatingips.List(client, listOpts).AllPages(ctx)
	if err != nil {
		resp.Diagnostics.AddError("networking: listing floating IPs", err.Error())
		return
	}
	all, err := floatingips.ExtractFloatingIPs(pages)
	if err != nil {
		resp.Diagnostics.AddError("networking: extracting floating IPs", err.Error())
		return
	}

	var fip *floatingips.FloatingIP
	switch len(all) {
	case 0:
		resp.Diagnostics.AddError("No floating IP found", "No floating IP matched the given criteria.")
		return
	case 1:
		fip = &all[0]
	default:
		resp.Diagnostics.AddError("Multiple floating IPs found",
			fmt.Sprintf("%d floating IPs matched; refine the filters.", len(all)))
		return
	}

	data.ID = types.StringValue(fip.ID)
	data.Address = types.StringValue(fip.FloatingIP)
	data.Description = types.StringValue(fip.Description)
	data.FloatingNetworkID = types.StringValue(fip.FloatingNetworkID)
	data.PortID = types.StringValue(fip.PortID)
	data.FixedIP = types.StringValue(fip.FixedIP)
	data.Status = types.StringValue(fip.Status)
	data.RouterID = types.StringValue(fip.RouterID)
	data.TenantID = types.StringValue(fip.TenantID)
	if data.Region.IsNull() || data.Region.IsUnknown() {
		data.Region = types.StringValue(d.config.Region)
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}
