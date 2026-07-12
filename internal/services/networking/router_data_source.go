// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0
//
// Ported from terraform-provider-openstack v3.4.0
// (openstack/data_source_openstack_networking_router_v2.go), adapted for the
// terraform-plugin-framework and PCD.

package networking

import (
	"context"
	"fmt"

	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/extensions/layer3/routers"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/platform9/terraform-provider-pcd/internal/clients"
)

var (
	_ datasource.DataSource              = (*routerDataSource)(nil)
	_ datasource.DataSourceWithConfigure = (*routerDataSource)(nil)
)

// NewRouterDataSource is the factory registered with the provider.
func NewRouterDataSource() datasource.DataSource {
	return &routerDataSource{}
}

type routerDataSource struct {
	config *clients.Config
}

type routerDataSourceModel struct {
	ID                types.String `tfsdk:"id"`
	RouterID          types.String `tfsdk:"router_id"`
	Name              types.String `tfsdk:"name"`
	Description       types.String `tfsdk:"description"`
	AdminStateUp      types.Bool   `tfsdk:"admin_state_up"`
	Status            types.String `tfsdk:"status"`
	ExternalNetworkID types.String `tfsdk:"external_network_id"`
	EnableSNAT        types.Bool   `tfsdk:"enable_snat"`
	TenantID          types.String `tfsdk:"tenant_id"`
	Region            types.String `tfsdk:"region"`
}

func (d *routerDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_networking_router"
}

func (d *routerDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Look up a Neutron router by ID or filters. Exactly one router must match.",
		Attributes: map[string]schema.Attribute{
			"id":                  schema.StringAttribute{Computed: true, MarkdownDescription: "The router ID."},
			"router_id":           schema.StringAttribute{Optional: true, MarkdownDescription: "Look up by router ID (takes precedence over filters)."},
			"name":                schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "Filter by (and report) the name."},
			"description":         schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "Filter by (and report) the description."},
			"admin_state_up":      schema.BoolAttribute{Optional: true, Computed: true, MarkdownDescription: "Filter by (and report) the administrative state."},
			"status":              schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "Filter by (and report) the operational status."},
			"external_network_id": schema.StringAttribute{Computed: true, MarkdownDescription: "The external network the router's gateway is attached to."},
			"enable_snat":         schema.BoolAttribute{Computed: true, MarkdownDescription: "Whether source NAT is enabled on the gateway."},
			"tenant_id":           schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "Filter by (and report) the owning project."},
			"region":              schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "The region. Defaults to the provider's region."},
		},
	}
}

func (d *routerDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	d.config = configureClient(req.ProviderData, &resp.Diagnostics)
}

func (d *routerDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var data routerDataSourceModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := d.config.NetworkV2Client()
	if err != nil {
		resp.Diagnostics.AddError("networking: building v2 client", err.Error())
		return
	}

	var router *routers.Router
	if v := data.RouterID.ValueString(); v != "" {
		router, err = routers.Get(ctx, client, v).Extract()
		if err != nil {
			resp.Diagnostics.AddError("networking: getting router", err.Error())
			return
		}
	} else {
		listOpts := routers.ListOpts{
			Name:        data.Name.ValueString(),
			Description: data.Description.ValueString(),
			Status:      data.Status.ValueString(),
			TenantID:    data.TenantID.ValueString(),
		}
		if !data.AdminStateUp.IsNull() && !data.AdminStateUp.IsUnknown() {
			v := data.AdminStateUp.ValueBool()
			listOpts.AdminStateUp = &v
		}
		pages, err := routers.List(client, listOpts).AllPages(ctx)
		if err != nil {
			resp.Diagnostics.AddError("networking: listing routers", err.Error())
			return
		}
		all, err := routers.ExtractRouters(pages)
		if err != nil {
			resp.Diagnostics.AddError("networking: extracting routers", err.Error())
			return
		}
		switch len(all) {
		case 0:
			resp.Diagnostics.AddError("No router found", "No router matched the given criteria.")
			return
		case 1:
			router = &all[0]
		default:
			resp.Diagnostics.AddError("Multiple routers found",
				fmt.Sprintf("%d routers matched; refine the filters.", len(all)))
			return
		}
	}

	data.ID = types.StringValue(router.ID)
	data.RouterID = types.StringValue(router.ID)
	data.Name = types.StringValue(router.Name)
	data.Description = types.StringValue(router.Description)
	data.AdminStateUp = types.BoolValue(router.AdminStateUp)
	data.Status = types.StringValue(router.Status)
	data.TenantID = types.StringValue(router.TenantID)
	data.ExternalNetworkID = types.StringValue(router.GatewayInfo.NetworkID)
	if router.GatewayInfo.EnableSNAT != nil {
		data.EnableSNAT = types.BoolValue(*router.GatewayInfo.EnableSNAT)
	} else {
		data.EnableSNAT = types.BoolNull()
	}
	if data.Region.IsNull() || data.Region.IsUnknown() {
		data.Region = types.StringValue(d.config.Region)
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}
