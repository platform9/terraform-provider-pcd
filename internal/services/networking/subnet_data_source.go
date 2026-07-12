// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0
//
// Ported from terraform-provider-openstack v3.4.0
// (openstack/data_source_openstack_networking_subnet_v2.go), adapted for the
// terraform-plugin-framework and PCD.

package networking

import (
	"context"
	"fmt"

	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/subnets"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/platform9/terraform-provider-pcd/internal/clients"
)

var (
	_ datasource.DataSource              = (*subnetDataSource)(nil)
	_ datasource.DataSourceWithConfigure = (*subnetDataSource)(nil)
)

// NewSubnetDataSource is the factory registered with the provider.
func NewSubnetDataSource() datasource.DataSource {
	return &subnetDataSource{}
}

type subnetDataSource struct {
	config *clients.Config
}

type subnetDataSourceModel struct {
	ID         types.String `tfsdk:"id"`
	SubnetID   types.String `tfsdk:"subnet_id"`
	Name       types.String `tfsdk:"name"`
	NetworkID  types.String `tfsdk:"network_id"`
	CIDR       types.String `tfsdk:"cidr"`
	IPVersion  types.Int64  `tfsdk:"ip_version"`
	GatewayIP  types.String `tfsdk:"gateway_ip"`
	EnableDHCP types.Bool   `tfsdk:"enable_dhcp"`
	TenantID   types.String `tfsdk:"tenant_id"`
	Region     types.String `tfsdk:"region"`
}

func (d *subnetDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_networking_subnet"
}

func (d *subnetDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Look up a Neutron subnet by ID or filters.",
		Attributes: map[string]schema.Attribute{
			"id":          schema.StringAttribute{Computed: true, MarkdownDescription: "The subnet ID."},
			"subnet_id":   schema.StringAttribute{Optional: true, MarkdownDescription: "Look up by subnet ID (takes precedence over filters)."},
			"name":        schema.StringAttribute{Optional: true, MarkdownDescription: "Filter by name."},
			"network_id":  schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "Filter by (and report) the network."},
			"cidr":        schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "Filter by (and report) the CIDR."},
			"ip_version":  schema.Int64Attribute{Computed: true, MarkdownDescription: "The IP version."},
			"gateway_ip":  schema.StringAttribute{Computed: true, MarkdownDescription: "The gateway IP."},
			"enable_dhcp": schema.BoolAttribute{Computed: true, MarkdownDescription: "Whether DHCP is enabled."},
			"tenant_id":   schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "Filter by (and report) the owning project."},
			"region":      schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "The region. Defaults to the provider's region."},
		},
	}
}

func (d *subnetDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	d.config = configureClient(req.ProviderData, &resp.Diagnostics)
}

func (d *subnetDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var data subnetDataSourceModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := d.config.NetworkV2Client()
	if err != nil {
		resp.Diagnostics.AddError("networking: building v2 client", err.Error())
		return
	}

	var sub *subnets.Subnet
	if v := data.SubnetID.ValueString(); v != "" {
		sub, err = subnets.Get(ctx, client, v).Extract()
		if err != nil {
			resp.Diagnostics.AddError("networking: getting subnet", err.Error())
			return
		}
	} else {
		pages, err := subnets.List(client, subnets.ListOpts{
			Name:      data.Name.ValueString(),
			NetworkID: data.NetworkID.ValueString(),
			CIDR:      data.CIDR.ValueString(),
			TenantID:  data.TenantID.ValueString(),
		}).AllPages(ctx)
		if err != nil {
			resp.Diagnostics.AddError("networking: listing subnets", err.Error())
			return
		}
		all, err := subnets.ExtractSubnets(pages)
		if err != nil {
			resp.Diagnostics.AddError("networking: extracting subnets", err.Error())
			return
		}
		switch len(all) {
		case 0:
			resp.Diagnostics.AddError("No subnet found", "No subnet matched the given criteria.")
			return
		case 1:
			sub = &all[0]
		default:
			resp.Diagnostics.AddError("Multiple subnets found",
				fmt.Sprintf("%d subnets matched; refine the filters.", len(all)))
			return
		}
	}

	data.ID = types.StringValue(sub.ID)
	data.SubnetID = types.StringValue(sub.ID)
	data.Name = types.StringValue(sub.Name)
	data.NetworkID = types.StringValue(sub.NetworkID)
	data.CIDR = types.StringValue(sub.CIDR)
	data.IPVersion = types.Int64Value(int64(sub.IPVersion))
	data.GatewayIP = types.StringValue(sub.GatewayIP)
	data.EnableDHCP = types.BoolValue(sub.EnableDHCP)
	data.TenantID = types.StringValue(sub.TenantID)
	if data.Region.IsNull() || data.Region.IsUnknown() {
		data.Region = types.StringValue(d.config.Region)
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}
