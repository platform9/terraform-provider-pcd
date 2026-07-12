// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0
//
// Ported from terraform-provider-openstack v3.4.0
// (openstack/data_source_openstack_identity_user_v3.go), adapted for the
// terraform-plugin-framework and PCD.

package identity

import (
	"context"
	"fmt"

	"github.com/gophercloud/gophercloud/v2/openstack/identity/v3/users"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/platform9/terraform-provider-pcd/internal/clients"
)

var (
	_ datasource.DataSource              = (*userDataSource)(nil)
	_ datasource.DataSourceWithConfigure = (*userDataSource)(nil)
)

// NewUserDataSource is the factory registered with the provider.
func NewUserDataSource() datasource.DataSource {
	return &userDataSource{}
}

type userDataSource struct {
	config *clients.Config
}

type userDataSourceModel struct {
	ID               types.String `tfsdk:"id"`
	UserID           types.String `tfsdk:"user_id"`
	Name             types.String `tfsdk:"name"`
	Description      types.String `tfsdk:"description"`
	DomainID         types.String `tfsdk:"domain_id"`
	DefaultProjectID types.String `tfsdk:"default_project_id"`
	Enabled          types.Bool   `tfsdk:"enabled"`
	Region           types.String `tfsdk:"region"`
}

func (d *userDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_identity_user"
}

func (d *userDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Look up a user in PCD's Keystone identity service by name or ID.",
		Attributes: map[string]schema.Attribute{
			"id":                 schema.StringAttribute{Computed: true, MarkdownDescription: "The user ID."},
			"user_id":            schema.StringAttribute{Optional: true, MarkdownDescription: "Look up the user by ID (takes precedence over name)."},
			"name":               schema.StringAttribute{Optional: true, MarkdownDescription: "Look up the user by name."},
			"domain_id":          schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "Restrict the lookup to (and report) this domain."},
			"description":        schema.StringAttribute{Computed: true, MarkdownDescription: "The user description."},
			"default_project_id": schema.StringAttribute{Computed: true, MarkdownDescription: "The user's default project ID."},
			"enabled":            schema.BoolAttribute{Computed: true, MarkdownDescription: "Whether the user is enabled."},
			"region":             schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "The region. Defaults to the provider's region."},
		},
	}
}

func (d *userDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	d.config = configureClient(req.ProviderData, &resp.Diagnostics)
}

func (d *userDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var data userDataSourceModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := d.config.IdentityV3Client()
	if err != nil {
		resp.Diagnostics.AddError("identity: building v3 client", err.Error())
		return
	}

	var user *users.User
	if v := data.UserID.ValueString(); v != "" {
		user, err = users.Get(ctx, client, v).Extract()
		if err != nil {
			resp.Diagnostics.AddError("identity: getting user by id", err.Error())
			return
		}
	} else {
		pages, err := users.List(client, users.ListOpts{
			Name:     data.Name.ValueString(),
			DomainID: data.DomainID.ValueString(),
		}).AllPages(ctx)
		if err != nil {
			resp.Diagnostics.AddError("identity: listing users", err.Error())
			return
		}
		all, err := users.ExtractUsers(pages)
		if err != nil {
			resp.Diagnostics.AddError("identity: extracting users", err.Error())
			return
		}
		switch len(all) {
		case 0:
			resp.Diagnostics.AddError("No user found", "No user matched the given criteria.")
			return
		case 1:
			user = &all[0]
		default:
			resp.Diagnostics.AddError("Multiple users found",
				fmt.Sprintf("%d users matched; refine name/domain_id to select exactly one.", len(all)))
			return
		}
	}

	data.ID = types.StringValue(user.ID)
	data.UserID = types.StringValue(user.ID)
	data.Name = types.StringValue(user.Name)
	data.Description = types.StringValue(user.Description)
	data.DomainID = types.StringValue(user.DomainID)
	data.DefaultProjectID = types.StringValue(user.DefaultProjectID)
	data.Enabled = types.BoolValue(user.Enabled)
	if data.Region.IsNull() || data.Region.IsUnknown() {
		data.Region = types.StringValue(d.config.Region)
	}

	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}
