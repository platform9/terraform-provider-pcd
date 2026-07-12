// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0
//
// Ported from terraform-provider-openstack v3.4.0
// (openstack/data_source_openstack_networking_secgroup_v2.go), adapted for the
// terraform-plugin-framework and PCD.

package networking

import (
	"context"
	"fmt"

	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/extensions/security/groups"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/platform9/terraform-provider-pcd/internal/clients"
)

var (
	_ datasource.DataSource              = (*secgroupDataSource)(nil)
	_ datasource.DataSourceWithConfigure = (*secgroupDataSource)(nil)
)

// NewSecgroupDataSource is the factory registered with the provider.
func NewSecgroupDataSource() datasource.DataSource {
	return &secgroupDataSource{}
}

type secgroupDataSource struct {
	config *clients.Config
}

type secgroupDataSourceModel struct {
	ID          types.String `tfsdk:"id"`
	SecgroupID  types.String `tfsdk:"secgroup_id"`
	Name        types.String `tfsdk:"name"`
	Description types.String `tfsdk:"description"`
	TenantID    types.String `tfsdk:"tenant_id"`
	Region      types.String `tfsdk:"region"`
}

func (d *secgroupDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_networking_secgroup"
}

func (d *secgroupDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Look up a Neutron security group by name or ID.",
		Attributes: map[string]schema.Attribute{
			"id":          schema.StringAttribute{Computed: true, MarkdownDescription: "The security group ID."},
			"secgroup_id": schema.StringAttribute{Optional: true, MarkdownDescription: "Look up by ID (takes precedence over name)."},
			"name":        schema.StringAttribute{Optional: true, MarkdownDescription: "Look up by name."},
			"description": schema.StringAttribute{Computed: true, MarkdownDescription: "The description."},
			"tenant_id":   schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "Filter by (and report) the owning project."},
			"region":      schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "The region. Defaults to the provider's region."},
		},
	}
}

func (d *secgroupDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	d.config = configureClient(req.ProviderData, &resp.Diagnostics)
}

func (d *secgroupDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var data secgroupDataSourceModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := d.config.NetworkV2Client()
	if err != nil {
		resp.Diagnostics.AddError("networking: building v2 client", err.Error())
		return
	}

	var sg *groups.SecGroup
	if v := data.SecgroupID.ValueString(); v != "" {
		sg, err = groups.Get(ctx, client, v).Extract()
		if err != nil {
			resp.Diagnostics.AddError("networking: getting security group", err.Error())
			return
		}
	} else {
		pages, err := groups.List(client, groups.ListOpts{
			Name:     data.Name.ValueString(),
			TenantID: data.TenantID.ValueString(),
		}).AllPages(ctx)
		if err != nil {
			resp.Diagnostics.AddError("networking: listing security groups", err.Error())
			return
		}
		all, err := groups.ExtractGroups(pages)
		if err != nil {
			resp.Diagnostics.AddError("networking: extracting security groups", err.Error())
			return
		}
		switch len(all) {
		case 0:
			resp.Diagnostics.AddError("No security group found", "No security group matched the given criteria.")
			return
		case 1:
			sg = &all[0]
		default:
			resp.Diagnostics.AddError("Multiple security groups found",
				fmt.Sprintf("%d matched; refine name/tenant_id.", len(all)))
			return
		}
	}

	data.ID = types.StringValue(sg.ID)
	data.SecgroupID = types.StringValue(sg.ID)
	data.Name = types.StringValue(sg.Name)
	data.Description = types.StringValue(sg.Description)
	data.TenantID = types.StringValue(sg.TenantID)
	if data.Region.IsNull() || data.Region.IsUnknown() {
		data.Region = types.StringValue(d.config.Region)
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}
