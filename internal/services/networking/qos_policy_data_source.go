// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0
//
// Ported from terraform-provider-openstack v3.4.0
// (openstack/data_source_openstack_networking_qos_policy_v2.go), adapted for the
// terraform-plugin-framework and PCD.

package networking

import (
	"context"
	"fmt"

	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/extensions/qos/policies"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/platform9/terraform-provider-pcd/internal/clients"
)

var (
	_ datasource.DataSource              = (*qosPolicyDataSource)(nil)
	_ datasource.DataSourceWithConfigure = (*qosPolicyDataSource)(nil)
)

// NewQoSPolicyDataSource is the factory registered with the provider.
func NewQoSPolicyDataSource() datasource.DataSource {
	return &qosPolicyDataSource{}
}

type qosPolicyDataSource struct {
	config *clients.Config
}

type qosPolicyDataSourceModel struct {
	ID          types.String `tfsdk:"id"`
	PolicyID    types.String `tfsdk:"policy_id"`
	Name        types.String `tfsdk:"name"`
	Description types.String `tfsdk:"description"`
	Shared      types.Bool   `tfsdk:"shared"`
	IsDefault   types.Bool   `tfsdk:"is_default"`
	TenantID    types.String `tfsdk:"tenant_id"`
	ProjectID   types.String `tfsdk:"project_id"`
	Tags        types.Set    `tfsdk:"tags"`
	Region      types.String `tfsdk:"region"`
}

func (d *qosPolicyDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_networking_qos_policy"
}

func (d *qosPolicyDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Look up a Neutron QoS policy by name or ID.",
		Attributes: map[string]schema.Attribute{
			"id":          schema.StringAttribute{Computed: true, MarkdownDescription: "The QoS policy ID."},
			"policy_id":   schema.StringAttribute{Optional: true, MarkdownDescription: "Look up by policy ID (takes precedence over name)."},
			"name":        schema.StringAttribute{Optional: true, MarkdownDescription: "Look up by name."},
			"description": schema.StringAttribute{Computed: true, MarkdownDescription: "The policy description."},
			"shared":      schema.BoolAttribute{Computed: true, MarkdownDescription: "Whether the policy is shared across all projects."},
			"is_default":  schema.BoolAttribute{Computed: true, MarkdownDescription: "Whether this is the default policy for the project."},
			"tenant_id":   schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "Filter by (and report) the owning project."},
			"project_id":  schema.StringAttribute{Computed: true, MarkdownDescription: "The owning project ID."},
			"tags":        schema.SetAttribute{Computed: true, ElementType: types.StringType, MarkdownDescription: "Tags applied to the policy."},
			"region":      schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "The region. Defaults to the provider's region."},
		},
	}
}

func (d *qosPolicyDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	d.config = configureClient(req.ProviderData, &resp.Diagnostics)
}

func (d *qosPolicyDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var data qosPolicyDataSourceModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := d.config.NetworkV2Client()
	if err != nil {
		resp.Diagnostics.AddError("networking: building v2 client", err.Error())
		return
	}

	var policy *policies.Policy
	if id := data.PolicyID.ValueString(); id != "" {
		policy, err = policies.Get(ctx, client, id).Extract()
		if err != nil {
			resp.Diagnostics.AddError("networking: getting qos policy", err.Error())
			return
		}
	} else {
		pages, err := policies.List(client, policies.ListOpts{
			Name:     data.Name.ValueString(),
			TenantID: data.TenantID.ValueString(),
		}).AllPages(ctx)
		if err != nil {
			resp.Diagnostics.AddError("networking: listing qos policies", err.Error())
			return
		}
		all, err := policies.ExtractPolicies(pages)
		if err != nil {
			resp.Diagnostics.AddError("networking: extracting qos policies", err.Error())
			return
		}
		switch len(all) {
		case 0:
			resp.Diagnostics.AddError("No QoS policy found", "No QoS policy matched the given criteria.")
			return
		case 1:
			policy = &all[0]
		default:
			resp.Diagnostics.AddError("Multiple QoS policies found",
				fmt.Sprintf("%d policies matched; refine name/tenant_id.", len(all)))
			return
		}
	}

	data.ID = types.StringValue(policy.ID)
	data.PolicyID = types.StringValue(policy.ID)
	data.Name = types.StringValue(policy.Name)
	data.Description = types.StringValue(policy.Description)
	data.Shared = types.BoolValue(policy.Shared)
	data.IsDefault = types.BoolValue(policy.IsDefault)
	data.TenantID = types.StringValue(policy.TenantID)
	data.ProjectID = types.StringValue(policy.ProjectID)

	tagVals := policy.Tags
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
