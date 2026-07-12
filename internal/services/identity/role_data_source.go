// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0
//
// Ported from terraform-provider-openstack v3.4.0
// (openstack/data_source_openstack_identity_role_v3.go), adapted for the
// terraform-plugin-framework and PCD.

package identity

import (
	"context"
	"fmt"

	"github.com/gophercloud/gophercloud/v2/openstack/identity/v3/roles"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/platform9/terraform-provider-pcd/internal/clients"
)

var (
	_ datasource.DataSource              = (*roleDataSource)(nil)
	_ datasource.DataSourceWithConfigure = (*roleDataSource)(nil)
)

// NewRoleDataSource is the factory registered with the provider.
func NewRoleDataSource() datasource.DataSource {
	return &roleDataSource{}
}

type roleDataSource struct {
	config *clients.Config
}

type roleDataSourceModel struct {
	ID       types.String `tfsdk:"id"`
	RoleID   types.String `tfsdk:"role_id"`
	Name     types.String `tfsdk:"name"`
	DomainID types.String `tfsdk:"domain_id"`
	Region   types.String `tfsdk:"region"`
}

func (d *roleDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_identity_role"
}

func (d *roleDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Look up a role in PCD's Keystone identity service by name or ID.",
		Attributes: map[string]schema.Attribute{
			"id":        schema.StringAttribute{Computed: true, MarkdownDescription: "The role ID."},
			"role_id":   schema.StringAttribute{Optional: true, MarkdownDescription: "Look up the role by ID (takes precedence over name)."},
			"name":      schema.StringAttribute{Optional: true, MarkdownDescription: "Look up the role by name."},
			"domain_id": schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "Restrict the lookup to (and report) this domain."},
			"region":    schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "The region. Defaults to the provider's region."},
		},
	}
}

func (d *roleDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	d.config = configureClient(req.ProviderData, &resp.Diagnostics)
}

func (d *roleDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var data roleDataSourceModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := d.config.IdentityV3Client()
	if err != nil {
		resp.Diagnostics.AddError("identity: building v3 client", err.Error())
		return
	}

	var role *roles.Role
	if v := data.RoleID.ValueString(); v != "" {
		role, err = roles.Get(ctx, client, v).Extract()
		if err != nil {
			resp.Diagnostics.AddError("identity: getting role by id", err.Error())
			return
		}
	} else {
		pages, err := roles.List(client, roles.ListOpts{
			Name:     data.Name.ValueString(),
			DomainID: data.DomainID.ValueString(),
		}).AllPages(ctx)
		if err != nil {
			resp.Diagnostics.AddError("identity: listing roles", err.Error())
			return
		}
		all, err := roles.ExtractRoles(pages)
		if err != nil {
			resp.Diagnostics.AddError("identity: extracting roles", err.Error())
			return
		}
		switch len(all) {
		case 0:
			resp.Diagnostics.AddError("No role found", "No role matched the given criteria.")
			return
		case 1:
			role = &all[0]
		default:
			resp.Diagnostics.AddError("Multiple roles found",
				fmt.Sprintf("%d roles matched; refine name/domain_id to select exactly one.", len(all)))
			return
		}
	}

	data.ID = types.StringValue(role.ID)
	data.RoleID = types.StringValue(role.ID)
	data.Name = types.StringValue(role.Name)
	data.DomainID = types.StringValue(role.DomainID)
	if data.Region.IsNull() || data.Region.IsUnknown() {
		data.Region = types.StringValue(d.config.Region)
	}

	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}
