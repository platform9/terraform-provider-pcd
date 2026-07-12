// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0
//
// Ported from terraform-provider-openstack v3.4.0
// (openstack/data_source_openstack_networking_port_v2.go), adapted for the
// terraform-plugin-framework and PCD.

package networking

import (
	"context"
	"fmt"

	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/ports"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/platform9/terraform-provider-pcd/internal/clients"
)

var (
	_ datasource.DataSource              = (*portDataSource)(nil)
	_ datasource.DataSourceWithConfigure = (*portDataSource)(nil)
)

// NewPortDataSource is the factory registered with the provider.
func NewPortDataSource() datasource.DataSource {
	return &portDataSource{}
}

type portDataSource struct {
	config *clients.Config
}

type portDataSourceModel struct {
	ID               types.String `tfsdk:"id"`
	PortID           types.String `tfsdk:"port_id"`
	Name             types.String `tfsdk:"name"`
	Description      types.String `tfsdk:"description"`
	NetworkID        types.String `tfsdk:"network_id"`
	DeviceID         types.String `tfsdk:"device_id"`
	DeviceOwner      types.String `tfsdk:"device_owner"`
	MACAddress       types.String `tfsdk:"mac_address"`
	Status           types.String `tfsdk:"status"`
	AdminStateUp     types.Bool   `tfsdk:"admin_state_up"`
	TenantID         types.String `tfsdk:"tenant_id"`
	SecurityGroupIDs types.Set    `tfsdk:"security_group_ids"`
	AllFixedIPs      types.List   `tfsdk:"all_fixed_ips"`
	Region           types.String `tfsdk:"region"`
}

func (d *portDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_networking_port"
}

func (d *portDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Look up a Neutron port by ID or filters. Exactly one port must match.",
		Attributes: map[string]schema.Attribute{
			"id":                 schema.StringAttribute{Computed: true, MarkdownDescription: "The port ID."},
			"port_id":            schema.StringAttribute{Optional: true, MarkdownDescription: "Look up by port ID (takes precedence over filters)."},
			"name":               schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "Filter by (and report) the name."},
			"description":        schema.StringAttribute{Computed: true, MarkdownDescription: "The port description."},
			"network_id":         schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "Filter by (and report) the network."},
			"device_id":          schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "Filter by (and report) the device ID."},
			"device_owner":       schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "Filter by (and report) the device owner."},
			"mac_address":        schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "Filter by (and report) the MAC address."},
			"status":             schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "Filter by (and report) the operational status."},
			"admin_state_up":     schema.BoolAttribute{Optional: true, Computed: true, MarkdownDescription: "Filter by (and report) the administrative state."},
			"tenant_id":          schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "Filter by (and report) the owning project."},
			"security_group_ids": schema.SetAttribute{Computed: true, ElementType: types.StringType, MarkdownDescription: "Security groups applied to the port."},
			"all_fixed_ips":      schema.ListAttribute{Computed: true, ElementType: types.StringType, MarkdownDescription: "The IP addresses assigned to the port."},
			"region":             schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "The region. Defaults to the provider's region."},
		},
	}
}

func (d *portDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	d.config = configureClient(req.ProviderData, &resp.Diagnostics)
}

func (d *portDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var data portDataSourceModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := d.config.NetworkV2Client()
	if err != nil {
		resp.Diagnostics.AddError("networking: building v2 client", err.Error())
		return
	}

	var port *ports.Port
	if v := data.PortID.ValueString(); v != "" {
		port, err = ports.Get(ctx, client, v).Extract()
		if err != nil {
			resp.Diagnostics.AddError("networking: getting port", err.Error())
			return
		}
	} else {
		listOpts := ports.ListOpts{
			Name:        data.Name.ValueString(),
			Description: data.Description.ValueString(),
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
		switch len(all) {
		case 0:
			resp.Diagnostics.AddError("No port found", "No port matched the given criteria.")
			return
		case 1:
			port = &all[0]
		default:
			resp.Diagnostics.AddError("Multiple ports found",
				fmt.Sprintf("%d ports matched; refine the filters.", len(all)))
			return
		}
	}

	data.ID = types.StringValue(port.ID)
	data.PortID = types.StringValue(port.ID)
	data.Name = types.StringValue(port.Name)
	data.Description = types.StringValue(port.Description)
	data.NetworkID = types.StringValue(port.NetworkID)
	data.DeviceID = types.StringValue(port.DeviceID)
	data.DeviceOwner = types.StringValue(port.DeviceOwner)
	data.MACAddress = types.StringValue(port.MACAddress)
	data.Status = types.StringValue(port.Status)
	data.AdminStateUp = types.BoolValue(port.AdminStateUp)
	data.TenantID = types.StringValue(port.TenantID)

	sgVals := port.SecurityGroups
	if sgVals == nil {
		sgVals = []string{}
	}
	sgs, diags := types.SetValueFrom(ctx, types.StringType, sgVals)
	resp.Diagnostics.Append(diags...)
	data.SecurityGroupIDs = sgs

	ips := make([]string, 0, len(port.FixedIPs))
	for _, fip := range port.FixedIPs {
		ips = append(ips, fip.IPAddress)
	}
	allIPs, diags := types.ListValueFrom(ctx, types.StringType, ips)
	resp.Diagnostics.Append(diags...)
	data.AllFixedIPs = allIPs

	if data.Region.IsNull() || data.Region.IsUnknown() {
		data.Region = types.StringValue(d.config.Region)
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}
