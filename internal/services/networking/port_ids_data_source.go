// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0
//
// Ported from terraform-provider-openstack v3.4.0
// (openstack/data_source_openstack_networking_port_ids_v2.go), adapted for the
// terraform-plugin-framework and PCD.

package networking

import (
	"context"

	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/ports"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/platform9/terraform-provider-pcd/internal/clients"
)

var (
	_ datasource.DataSource              = (*portIDsDataSource)(nil)
	_ datasource.DataSourceWithConfigure = (*portIDsDataSource)(nil)
)

// NewPortIDsDataSource is the factory registered with the provider.
func NewPortIDsDataSource() datasource.DataSource {
	return &portIDsDataSource{}
}

type portIDsDataSource struct {
	config *clients.Config
}

type portIDsDataSourceModel struct {
	ID           types.String `tfsdk:"id"`
	Name         types.String `tfsdk:"name"`
	NetworkID    types.String `tfsdk:"network_id"`
	DeviceID     types.String `tfsdk:"device_id"`
	DeviceOwner  types.String `tfsdk:"device_owner"`
	MACAddress   types.String `tfsdk:"mac_address"`
	Status       types.String `tfsdk:"status"`
	AdminStateUp types.Bool   `tfsdk:"admin_state_up"`
	TenantID     types.String `tfsdk:"tenant_id"`
	IDs          types.List   `tfsdk:"ids"`
	Region       types.String `tfsdk:"region"`
}

func (d *portIDsDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_networking_port_ids"
}

func (d *portIDsDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Look up the IDs of all Neutron ports matching the given filters, sorted ascending.",
		Attributes: map[string]schema.Attribute{
			"id":             schema.StringAttribute{Computed: true, MarkdownDescription: "A stable hash of the matched port IDs."},
			"name":           schema.StringAttribute{Optional: true, MarkdownDescription: "Filter by name."},
			"network_id":     schema.StringAttribute{Optional: true, MarkdownDescription: "Filter by network."},
			"device_id":      schema.StringAttribute{Optional: true, MarkdownDescription: "Filter by device ID."},
			"device_owner":   schema.StringAttribute{Optional: true, MarkdownDescription: "Filter by device owner."},
			"mac_address":    schema.StringAttribute{Optional: true, MarkdownDescription: "Filter by MAC address."},
			"status":         schema.StringAttribute{Optional: true, MarkdownDescription: "Filter by operational status."},
			"admin_state_up": schema.BoolAttribute{Optional: true, MarkdownDescription: "Filter by administrative state."},
			"tenant_id":      schema.StringAttribute{Optional: true, MarkdownDescription: "Filter by owning project."},
			"ids":            schema.ListAttribute{Computed: true, ElementType: types.StringType, MarkdownDescription: "The matched port IDs, sorted ascending."},
			"region":         schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "The region. Defaults to the provider's region."},
		},
	}
}

func (d *portIDsDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	d.config = configureClient(req.ProviderData, &resp.Diagnostics)
}

func (d *portIDsDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var data portIDsDataSourceModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := d.config.NetworkV2Client()
	if err != nil {
		resp.Diagnostics.AddError("networking: building v2 client", err.Error())
		return
	}

	listOpts := ports.ListOpts{
		Name:        data.Name.ValueString(),
		NetworkID:   data.NetworkID.ValueString(),
		DeviceID:    data.DeviceID.ValueString(),
		DeviceOwner: data.DeviceOwner.ValueString(),
		MACAddress:  data.MACAddress.ValueString(),
		Status:      data.Status.ValueString(),
		TenantID:    data.TenantID.ValueString(),
	}
	if !data.AdminStateUp.IsNull() && !data.AdminStateUp.IsUnknown() {
		v := data.AdminStateUp.ValueBool()
		listOpts.AdminStateUp = &v
	}

	pages, err := ports.List(client, listOpts).AllPages(ctx)
	if err != nil {
		resp.Diagnostics.AddError("networking: listing ports", err.Error())
		return
	}
	all, err := ports.ExtractPorts(pages)
	if err != nil {
		resp.Diagnostics.AddError("networking: extracting ports", err.Error())
		return
	}

	ids := make([]string, 0, len(all))
	for _, p := range all {
		ids = append(ids, p.ID)
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
