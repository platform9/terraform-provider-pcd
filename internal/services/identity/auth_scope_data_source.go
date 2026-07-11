// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0
//
// Ported from terraform-provider-openstack v3.4.0
// (openstack/data_source_openstack_identity_auth_scope_v3.go), adapted for the
// terraform-plugin-framework and PCD.

package identity

import (
	"context"
	"fmt"

	"github.com/gophercloud/gophercloud/v2/openstack/identity/v3/tokens"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/platform9/terraform-provider-pcd/internal/clients"
)

var (
	_ datasource.DataSource              = (*authScopeDataSource)(nil)
	_ datasource.DataSourceWithConfigure = (*authScopeDataSource)(nil)
)

// NewAuthScopeDataSource is the factory registered with the provider.
func NewAuthScopeDataSource() datasource.DataSource {
	return &authScopeDataSource{}
}

type authScopeDataSource struct {
	config *clients.Config
}

type authScopeModel struct {
	ID                types.String    `tfsdk:"id"`
	Name              types.String    `tfsdk:"name"`
	Region            types.String    `tfsdk:"region"`
	UserID            types.String    `tfsdk:"user_id"`
	UserName          types.String    `tfsdk:"user_name"`
	UserDomainID      types.String    `tfsdk:"user_domain_id"`
	UserDomainName    types.String    `tfsdk:"user_domain_name"`
	ProjectID         types.String    `tfsdk:"project_id"`
	ProjectName       types.String    `tfsdk:"project_name"`
	ProjectDomainID   types.String    `tfsdk:"project_domain_id"`
	ProjectDomainName types.String    `tfsdk:"project_domain_name"`
	DomainID          types.String    `tfsdk:"domain_id"`
	DomainName        types.String    `tfsdk:"domain_name"`
	Roles             []authScopeRole `tfsdk:"roles"`
}

type authScopeRole struct {
	RoleID   types.String `tfsdk:"role_id"`
	RoleName types.String `tfsdk:"role_name"`
}

func (d *authScopeDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_identity_auth_scope"
}

func (d *authScopeDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Details about the authentication scope (user, project, domain, and roles) " +
			"of the token the provider is currently using. Useful for wiring identity attributes into other resources.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Mirrors `name`; present so the data source has a stable identifier.",
			},
			"name": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "An arbitrary name for this scope lookup (used as the data source identifier).",
			},
			"region": schema.StringAttribute{
				Optional:            true,
				Computed:            true,
				MarkdownDescription: "The region the scope was resolved in. Defaults to the provider's region.",
			},
			"user_id":             schema.StringAttribute{Computed: true, MarkdownDescription: "ID of the authenticated user."},
			"user_name":           schema.StringAttribute{Computed: true, MarkdownDescription: "Name of the authenticated user."},
			"user_domain_id":      schema.StringAttribute{Computed: true, MarkdownDescription: "Domain ID of the authenticated user."},
			"user_domain_name":    schema.StringAttribute{Computed: true, MarkdownDescription: "Domain name of the authenticated user."},
			"project_id":          schema.StringAttribute{Computed: true, MarkdownDescription: "ID of the scoped project (empty for a domain-scoped token)."},
			"project_name":        schema.StringAttribute{Computed: true, MarkdownDescription: "Name of the scoped project."},
			"project_domain_id":   schema.StringAttribute{Computed: true, MarkdownDescription: "Domain ID of the scoped project."},
			"project_domain_name": schema.StringAttribute{Computed: true, MarkdownDescription: "Domain name of the scoped project."},
			"domain_id":           schema.StringAttribute{Computed: true, MarkdownDescription: "ID of the scoped domain (for a domain-scoped token)."},
			"domain_name":         schema.StringAttribute{Computed: true, MarkdownDescription: "Name of the scoped domain."},
			"roles": schema.ListNestedAttribute{
				Computed:            true,
				MarkdownDescription: "Roles the user holds in the current scope.",
				NestedObject: schema.NestedAttributeObject{
					Attributes: map[string]schema.Attribute{
						"role_id":   schema.StringAttribute{Computed: true, MarkdownDescription: "Role ID."},
						"role_name": schema.StringAttribute{Computed: true, MarkdownDescription: "Role name."},
					},
				},
			},
		},
	}
}

func (d *authScopeDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	if req.ProviderData == nil {
		return
	}
	config, ok := req.ProviderData.(*clients.Config)
	if !ok {
		resp.Diagnostics.AddError(
			"Unexpected provider data type",
			fmt.Sprintf("Expected *clients.Config, got %T. This is a bug in the provider.", req.ProviderData),
		)
		return
	}
	d.config = config
}

func (d *authScopeDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var m authScopeModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := d.config.IdentityV3Client()
	if err != nil {
		resp.Diagnostics.AddError("identity: building v3 client", err.Error())
		return
	}

	// Inspect the token the provider is already using.
	result := tokens.Get(ctx, client, client.Token())
	if result.Err != nil {
		resp.Diagnostics.AddError("identity: reading token scope", result.Err.Error())
		return
	}

	user, err := result.ExtractUser()
	if err != nil {
		resp.Diagnostics.AddError("identity: extracting user from token", err.Error())
		return
	}
	m.UserID = types.StringValue(user.ID)
	m.UserName = types.StringValue(user.Name)
	m.UserDomainID = types.StringValue(user.Domain.ID)
	m.UserDomainName = types.StringValue(user.Domain.Name)

	// Project (may be empty for a domain-scoped token).
	if project, err := result.ExtractProject(); err == nil && project != nil {
		m.ProjectID = types.StringValue(project.ID)
		m.ProjectName = types.StringValue(project.Name)
		m.ProjectDomainID = types.StringValue(project.Domain.ID)
		m.ProjectDomainName = types.StringValue(project.Domain.Name)
	} else {
		m.ProjectID = types.StringValue("")
		m.ProjectName = types.StringValue("")
		m.ProjectDomainID = types.StringValue("")
		m.ProjectDomainName = types.StringValue("")
	}

	// Domain (populated for a domain-scoped token).
	if domain, err := result.ExtractDomain(); err == nil && domain != nil {
		m.DomainID = types.StringValue(domain.ID)
		m.DomainName = types.StringValue(domain.Name)
	} else {
		m.DomainID = types.StringValue("")
		m.DomainName = types.StringValue("")
	}

	roles, err := result.ExtractRoles()
	if err != nil {
		resp.Diagnostics.AddError("identity: extracting roles from token", err.Error())
		return
	}
	m.Roles = make([]authScopeRole, 0, len(roles))
	for _, r := range roles {
		m.Roles = append(m.Roles, authScopeRole{
			RoleID:   types.StringValue(r.ID),
			RoleName: types.StringValue(r.Name),
		})
	}

	if m.Region.IsNull() || m.Region.IsUnknown() {
		m.Region = types.StringValue(d.config.Region)
	}
	m.ID = m.Name

	resp.Diagnostics.Append(resp.State.Set(ctx, &m)...)
}
