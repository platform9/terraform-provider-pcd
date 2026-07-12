// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0
//
// Ported from terraform-provider-openstack v3.4.0
// (openstack/data_source_openstack_lb_loadbalancer_v2.go), adapted for the
// terraform-plugin-framework and PCD.

package loadbalancer

import (
	"context"
	"fmt"

	"github.com/gophercloud/gophercloud/v2/openstack/loadbalancer/v2/loadbalancers"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/platform9/terraform-provider-pcd/internal/clients"
)

var (
	_ datasource.DataSource              = (*loadBalancerDataSource)(nil)
	_ datasource.DataSourceWithConfigure = (*loadBalancerDataSource)(nil)
)

// NewLoadBalancerDataSource is the factory registered with the provider.
func NewLoadBalancerDataSource() datasource.DataSource {
	return &loadBalancerDataSource{}
}

type loadBalancerDataSource struct {
	config *clients.Config
}

type loadBalancerDataSourceModel struct {
	ID                 types.String `tfsdk:"id"`
	LoadBalancerID     types.String `tfsdk:"loadbalancer_id"`
	Name               types.String `tfsdk:"name"`
	VipAddress         types.String `tfsdk:"vip_address"`
	VipPortID          types.String `tfsdk:"vip_port_id"`
	VipSubnetID        types.String `tfsdk:"vip_subnet_id"`
	ProvisioningStatus types.String `tfsdk:"provisioning_status"`
	OperatingStatus    types.String `tfsdk:"operating_status"`
	Tags               types.List   `tfsdk:"tags"`
	Region             types.String `tfsdk:"region"`
}

func (d *loadBalancerDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_lb_loadbalancer"
}

func (d *loadBalancerDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Look up an Octavia load balancer by ID or name. Exactly one load balancer must match.",
		Attributes: map[string]schema.Attribute{
			"id":                  schema.StringAttribute{Computed: true, MarkdownDescription: "The load balancer ID."},
			"loadbalancer_id":     schema.StringAttribute{Optional: true, MarkdownDescription: "Look up by load balancer ID (takes precedence over name)."},
			"name":                schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "Look up by (and report) the name."},
			"vip_address":         schema.StringAttribute{Computed: true, MarkdownDescription: "The VIP address."},
			"vip_port_id":         schema.StringAttribute{Computed: true, MarkdownDescription: "The VIP port ID."},
			"vip_subnet_id":       schema.StringAttribute{Computed: true, MarkdownDescription: "The VIP subnet ID."},
			"provisioning_status": schema.StringAttribute{Computed: true, MarkdownDescription: "The provisioning status."},
			"operating_status":    schema.StringAttribute{Computed: true, MarkdownDescription: "The operating status."},
			"tags":                schema.ListAttribute{Computed: true, ElementType: types.StringType, MarkdownDescription: "Tags applied to the load balancer."},
			"region":              schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "The region. Defaults to the provider's region."},
		},
	}
}

func (d *loadBalancerDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	d.config = configureClient(req.ProviderData, &resp.Diagnostics)
}

func (d *loadBalancerDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var data loadBalancerDataSourceModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := d.config.LoadBalancerV2Client()
	if err != nil {
		resp.Diagnostics.AddError("loadbalancer: building v2 client", err.Error())
		return
	}

	var lb *loadbalancers.LoadBalancer
	switch {
	case data.LoadBalancerID.ValueString() != "":
		lb, err = loadbalancers.Get(ctx, client, data.LoadBalancerID.ValueString()).Extract()
		if err != nil {
			resp.Diagnostics.AddError("loadbalancer: getting load balancer", err.Error())
			return
		}
	case data.Name.ValueString() != "":
		pages, err := loadbalancers.List(client, loadbalancers.ListOpts{Name: data.Name.ValueString()}).AllPages(ctx)
		if err != nil {
			resp.Diagnostics.AddError("loadbalancer: listing load balancers", err.Error())
			return
		}
		all, err := loadbalancers.ExtractLoadBalancers(pages)
		if err != nil {
			resp.Diagnostics.AddError("loadbalancer: extracting load balancers", err.Error())
			return
		}
		switch len(all) {
		case 0:
			resp.Diagnostics.AddError("No load balancer found", "No load balancer matched the given name.")
			return
		case 1:
			lb = &all[0]
		default:
			resp.Diagnostics.AddError("Multiple load balancers found",
				fmt.Sprintf("%d load balancers matched; refine the criteria.", len(all)))
			return
		}
	default:
		resp.Diagnostics.AddError("Missing lookup key", "Set one of loadbalancer_id or name.")
		return
	}

	data.ID = types.StringValue(lb.ID)
	data.LoadBalancerID = types.StringValue(lb.ID)
	data.Name = types.StringValue(lb.Name)
	data.VipAddress = types.StringValue(lb.VipAddress)
	data.VipPortID = types.StringValue(lb.VipPortID)
	data.VipSubnetID = types.StringValue(lb.VipSubnetID)
	data.ProvisioningStatus = types.StringValue(lb.ProvisioningStatus)
	data.OperatingStatus = types.StringValue(lb.OperatingStatus)

	tagVals := lb.Tags
	if tagVals == nil {
		tagVals = []string{}
	}
	tags, diags := types.ListValueFrom(ctx, types.StringType, tagVals)
	resp.Diagnostics.Append(diags...)
	data.Tags = tags

	if data.Region.IsNull() || data.Region.IsUnknown() {
		data.Region = types.StringValue(d.config.Region)
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}
