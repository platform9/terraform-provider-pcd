// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0
//
// Ported from terraform-provider-openstack v3.4.0
// (openstack/data_source_openstack_networking_subnet_ids_v2.go), adapted for the
// terraform-plugin-framework and PCD.

package networking

import (
	"context"

	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/subnets"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/platform9/terraform-provider-pcd/internal/clients"
)

var (
	_ datasource.DataSource              = (*subnetIDsDataSource)(nil)
	_ datasource.DataSourceWithConfigure = (*subnetIDsDataSource)(nil)
)

// NewSubnetIDsDataSource is the factory registered with the provider.
func NewSubnetIDsDataSource() datasource.DataSource {
	return &subnetIDsDataSource{}
}

type subnetIDsDataSource struct {
	config *clients.Config
}

type subnetIDsDataSourceModel struct {
	ID        types.String `tfsdk:"id"`
	Name      types.String `tfsdk:"name"`
	NetworkID types.String `tfsdk:"network_id"`
	CIDR      types.String `tfsdk:"cidr"`
	IPVersion types.Int64  `tfsdk:"ip_version"`
	GatewayIP types.String `tfsdk:"gateway_ip"`
	TenantID  types.String `tfsdk:"tenant_id"`
	IDs       types.List   `tfsdk:"ids"`
	Region    types.String `tfsdk:"region"`
}

func (d *subnetIDsDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_networking_subnet_ids"
}

func (d *subnetIDsDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Look up the IDs of all Neutron subnets matching the given filters, sorted ascending.",
		Attributes: map[string]schema.Attribute{
			"id":         schema.StringAttribute{Computed: true, MarkdownDescription: "A stable hash of the matched subnet IDs."},
			"name":       schema.StringAttribute{Optional: true, MarkdownDescription: "Filter by name."},
			"network_id": schema.StringAttribute{Optional: true, MarkdownDescription: "Filter by network."},
			"cidr":       schema.StringAttribute{Optional: true, MarkdownDescription: "Filter by CIDR."},
			"ip_version": schema.Int64Attribute{Optional: true, MarkdownDescription: "Filter by IP version (4 or 6)."},
			"gateway_ip": schema.StringAttribute{Optional: true, MarkdownDescription: "Filter by gateway IP."},
			"tenant_id":  schema.StringAttribute{Optional: true, MarkdownDescription: "Filter by owning project."},
			"ids":        schema.ListAttribute{Computed: true, ElementType: types.StringType, MarkdownDescription: "The matched subnet IDs, sorted ascending."},
			"region":     schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "The region. Defaults to the provider's region."},
		},
	}
}

func (d *subnetIDsDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	d.config = configureClient(req.ProviderData, &resp.Diagnostics)
}

func (d *subnetIDsDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var data subnetIDsDataSourceModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := d.config.NetworkV2Client()
	if err != nil {
		resp.Diagnostics.AddError("networking: building v2 client", err.Error())
		return
	}

	listOpts := subnets.ListOpts{
		Name:      data.Name.ValueString(),
		NetworkID: data.NetworkID.ValueString(),
		CIDR:      data.CIDR.ValueString(),
		GatewayIP: data.GatewayIP.ValueString(),
		TenantID:  data.TenantID.ValueString(),
	}
	if !data.IPVersion.IsNull() && !data.IPVersion.IsUnknown() {
		listOpts.IPVersion = int(data.IPVersion.ValueInt64())
	}

	pages, err := subnets.List(client, listOpts).AllPages(ctx)
	if err != nil {
		resp.Diagnostics.AddError("networking: listing subnets", err.Error())
		return
	}
	all, err := subnets.ExtractSubnets(pages)
	if err != nil {
		resp.Diagnostics.AddError("networking: extracting subnets", err.Error())
		return
	}

	ids := make([]string, 0, len(all))
	for _, s := range all {
		ids = append(ids, s.ID)
	}
	sorted, hash := sortedIDsHash(ids)

	idList, diags := types.ListValueFrom(ctx, types.StringType, sorted)
	resp.Diagnostics.Append(diags...)
	data.IDs = idList
	data.ID = types.StringValue(hash)
	if data.Region.IsNull() || data.Region.IsUnknown() {
		data.Region = types.StringValue(d.config.Region)
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}
