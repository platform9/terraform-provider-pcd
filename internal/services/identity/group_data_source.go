// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0
//
// Ported from terraform-provider-openstack v3.4.0
// (openstack/data_source_openstack_identity_group_v3.go), adapted for the
// terraform-plugin-framework and PCD.

package identity

import (
	"context"
	"fmt"

	"github.com/gophercloud/gophercloud/v2/openstack/identity/v3/groups"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/platform9/terraform-provider-pcd/internal/clients"
)

var (
	_ datasource.DataSource              = (*groupDataSource)(nil)
	_ datasource.DataSourceWithConfigure = (*groupDataSource)(nil)
)

// NewGroupDataSource is the factory registered with the provider.
func NewGroupDataSource() datasource.DataSource {
	return &groupDataSource{}
}

type groupDataSource struct {
	config *clients.Config
}

type groupDataSourceModel struct {
	ID          types.String `tfsdk:"id"`
	GroupID     types.String `tfsdk:"group_id"`
	Name        types.String `tfsdk:"name"`
	Description types.String `tfsdk:"description"`
	DomainID    types.String `tfsdk:"domain_id"`
	Region      types.String `tfsdk:"region"`
}

func (d *groupDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_identity_group"
}

func (d *groupDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Look up a group in PCD's Keystone identity service by name or ID.",
		Attributes: map[string]schema.Attribute{
			"id":          schema.StringAttribute{Computed: true, MarkdownDescription: "The group ID."},
			"group_id":    schema.StringAttribute{Optional: true, MarkdownDescription: "Look up the group by ID (takes precedence over name)."},
			"name":        schema.StringAttribute{Optional: true, MarkdownDescription: "Look up the group by name."},
			"description": schema.StringAttribute{Computed: true, MarkdownDescription: "The group description."},
			"domain_id":   schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "Restrict the lookup to (and report) this domain."},
			"region":      schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "The region. Defaults to the provider's region."},
		},
	}
}

func (d *groupDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	d.config = configureClient(req.ProviderData, &resp.Diagnostics)
}

func (d *groupDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var data groupDataSourceModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := d.config.IdentityV3Client()
	if err != nil {
		resp.Diagnostics.AddError("identity: building v3 client", err.Error())
		return
	}

	var group *groups.Group
	if v := data.GroupID.ValueString(); v != "" {
		group, err = groups.Get(ctx, client, v).Extract()
		if err != nil {
			resp.Diagnostics.AddError("identity: getting group by id", err.Error())
			return
		}
	} else {
		pages, err := groups.List(client, groups.ListOpts{
			Name:     data.Name.ValueString(),
			DomainID: data.DomainID.ValueString(),
		}).AllPages(ctx)
		if err != nil {
			resp.Diagnostics.AddError("identity: listing groups", err.Error())
			return
		}
		all, err := groups.ExtractGroups(pages)
		if err != nil {
			resp.Diagnostics.AddError("identity: extracting groups", err.Error())
			return
		}
		switch len(all) {
		case 0:
			resp.Diagnostics.AddError("No group found", "No group matched the given criteria.")
			return
		case 1:
			group = &all[0]
		default:
			resp.Diagnostics.AddError("Multiple groups found",
				fmt.Sprintf("%d groups matched; refine name/domain_id to select exactly one.", len(all)))
			return
		}
	}

	data.ID = types.StringValue(group.ID)
	data.GroupID = types.StringValue(group.ID)
	data.Name = types.StringValue(group.Name)
	data.Description = types.StringValue(group.Description)
	data.DomainID = types.StringValue(group.DomainID)
	if data.Region.IsNull() || data.Region.IsUnknown() {
		data.Region = types.StringValue(d.config.Region)
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}
