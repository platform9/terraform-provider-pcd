// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0
//
// Ported from terraform-provider-openstack v3.4.0
// (openstack/data_source_openstack_identity_project_v3.go), adapted for the
// terraform-plugin-framework and PCD.

package identity

import (
	"context"
	"fmt"

	"github.com/gophercloud/gophercloud/v2/openstack/identity/v3/projects"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/platform9/terraform-provider-pcd/internal/clients"
)

var (
	_ datasource.DataSource              = (*projectDataSource)(nil)
	_ datasource.DataSourceWithConfigure = (*projectDataSource)(nil)
)

// NewProjectDataSource is the factory registered with the provider.
func NewProjectDataSource() datasource.DataSource {
	return &projectDataSource{}
}

type projectDataSource struct {
	config *clients.Config
}

type projectDataSourceModel struct {
	ID          types.String `tfsdk:"id"`
	ProjectID   types.String `tfsdk:"project_id"`
	Name        types.String `tfsdk:"name"`
	Description types.String `tfsdk:"description"`
	DomainID    types.String `tfsdk:"domain_id"`
	Enabled     types.Bool   `tfsdk:"enabled"`
	IsDomain    types.Bool   `tfsdk:"is_domain"`
	ParentID    types.String `tfsdk:"parent_id"`
	Tags        types.Set    `tfsdk:"tags"`
	Region      types.String `tfsdk:"region"`
}

func (d *projectDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_identity_project"
}

func (d *projectDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Look up a project (tenant) in PCD's Keystone identity service by name or ID.",
		Attributes: map[string]schema.Attribute{
			"id":          schema.StringAttribute{Computed: true, MarkdownDescription: "The project ID."},
			"project_id":  schema.StringAttribute{Optional: true, MarkdownDescription: "Look up the project by ID (takes precedence over name)."},
			"name":        schema.StringAttribute{Optional: true, MarkdownDescription: "Look up the project by name."},
			"domain_id":   schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "Restrict the lookup to (and report) this domain."},
			"description": schema.StringAttribute{Computed: true, MarkdownDescription: "The project description."},
			"enabled":     schema.BoolAttribute{Computed: true, MarkdownDescription: "Whether the project is enabled."},
			"is_domain":   schema.BoolAttribute{Computed: true, MarkdownDescription: "Whether the project behaves as a domain."},
			"parent_id":   schema.StringAttribute{Computed: true, MarkdownDescription: "The parent project ID."},
			"tags":        schema.SetAttribute{Computed: true, ElementType: types.StringType, MarkdownDescription: "Tags applied to the project."},
			"region":      schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "The region. Defaults to the provider's region."},
		},
	}
}

func (d *projectDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	d.config = configureClient(req.ProviderData, &resp.Diagnostics)
}

func (d *projectDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var data projectDataSourceModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := d.config.IdentityV3Client()
	if err != nil {
		resp.Diagnostics.AddError("identity: building v3 client", err.Error())
		return
	}

	var project *projects.Project
	if v := data.ProjectID.ValueString(); v != "" {
		project, err = projects.Get(ctx, client, v).Extract()
		if err != nil {
			resp.Diagnostics.AddError("identity: getting project by id", err.Error())
			return
		}
	} else {
		pages, err := projects.List(client, projects.ListOpts{
			Name:     data.Name.ValueString(),
			DomainID: data.DomainID.ValueString(),
		}).AllPages(ctx)
		if err != nil {
			resp.Diagnostics.AddError("identity: listing projects", err.Error())
			return
		}
		all, err := projects.ExtractProjects(pages)
		if err != nil {
			resp.Diagnostics.AddError("identity: extracting projects", err.Error())
			return
		}
		switch len(all) {
		case 0:
			resp.Diagnostics.AddError("No project found", "No project matched the given criteria.")
			return
		case 1:
			project = &all[0]
		default:
			resp.Diagnostics.AddError("Multiple projects found",
				fmt.Sprintf("%d projects matched; refine name/domain_id to select exactly one.", len(all)))
			return
		}
	}

	data.ID = types.StringValue(project.ID)
	data.ProjectID = types.StringValue(project.ID)
	data.Name = types.StringValue(project.Name)
	data.Description = types.StringValue(project.Description)
	data.DomainID = types.StringValue(project.DomainID)
	data.Enabled = types.BoolValue(project.Enabled)
	data.IsDomain = types.BoolValue(project.IsDomain)
	data.ParentID = types.StringValue(project.ParentID)

	tagVals := project.Tags
	if tagVals == nil {
		tagVals = []string{}
	}
	tags, diags := types.SetValueFrom(ctx, types.StringType, tagVals)
	resp.Diagnostics.Append(diags...)
	data.Tags = tags

	if data.Region.IsNull() || data.Region.IsUnknown() {
		data.Region = types.StringValue(d.config.Region)
	}

	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}
