// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0
//
// Ported from terraform-provider-openstack v3.4.0
// (openstack/resource_openstack_networking_qos_policy_v2.go), adapted for the
// terraform-plugin-framework and PCD.

package networking

import (
	"context"
	"fmt"
	"net/http"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/extensions/qos/policies"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/setplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/platform9/terraform-provider-pcd/internal/clients"
)

var (
	_ resource.Resource                = (*qosPolicyResource)(nil)
	_ resource.ResourceWithConfigure   = (*qosPolicyResource)(nil)
	_ resource.ResourceWithImportState = (*qosPolicyResource)(nil)
)

// NewQoSPolicyResource is the factory registered with the provider.
func NewQoSPolicyResource() resource.Resource {
	return &qosPolicyResource{}
}

type qosPolicyResource struct {
	config *clients.Config
}

type qosPolicyModel struct {
	ID          types.String `tfsdk:"id"`
	Name        types.String `tfsdk:"name"`
	Description types.String `tfsdk:"description"`
	Shared      types.Bool   `tfsdk:"shared"`
	IsDefault   types.Bool   `tfsdk:"is_default"`
	TenantID    types.String `tfsdk:"tenant_id"`
	Tags        types.Set    `tfsdk:"tags"`
	Region      types.String `tfsdk:"region"`
}

func (r *qosPolicyResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_networking_qos_policy"
}

func (r *qosPolicyResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	useState := []planmodifier.String{stringplanmodifier.UseStateForUnknown()}
	resp.Schema = schema.Schema{
		MarkdownDescription: "Manages a Neutron QoS policy in PCD. Attach it to networks or ports and add rules with " +
			"`pcd_networking_qos_bandwidth_limit_rule`, `_dscp_marking_rule`, or `_minimum_bandwidth_rule`.",
		Attributes: map[string]schema.Attribute{
			"id":          schema.StringAttribute{Computed: true, MarkdownDescription: "The QoS policy ID.", PlanModifiers: useState},
			"name":        schema.StringAttribute{Required: true, MarkdownDescription: "The name of the QoS policy."},
			"description": schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "A description of the QoS policy.", PlanModifiers: useState},
			"shared":      schema.BoolAttribute{Optional: true, Computed: true, Default: booldefault.StaticBool(false), MarkdownDescription: "Whether the policy is shared across all projects."},
			"is_default":  schema.BoolAttribute{Optional: true, Computed: true, Default: booldefault.StaticBool(false), MarkdownDescription: "Whether this is the default policy for the project."},
			"tenant_id":   schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "The owning project. Changing this forces a new resource.", PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace(), stringplanmodifier.UseStateForUnknown()}},
			"tags":        schema.SetAttribute{Optional: true, Computed: true, ElementType: types.StringType, MarkdownDescription: "Tags applied to the policy.", PlanModifiers: []planmodifier.Set{setplanmodifier.UseStateForUnknown()}},
			"region":      schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "The region. Defaults to the provider's region.", PlanModifiers: useState},
		},
	}
}

func (r *qosPolicyResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.config = configureClient(req.ProviderData, &resp.Diagnostics)
}

func (r *qosPolicyResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan qosPolicyModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := r.config.NetworkV2Client()
	if err != nil {
		resp.Diagnostics.AddError("networking: building v2 client", err.Error())
		return
	}

	createOpts := policies.CreateOpts{
		Name:        plan.Name.ValueString(),
		Description: plan.Description.ValueString(),
		Shared:      plan.Shared.ValueBool(),
		IsDefault:   plan.IsDefault.ValueBool(),
		TenantID:    plan.TenantID.ValueString(),
	}

	policy, err := policies.Create(ctx, client, createOpts).Extract()
	if err != nil {
		resp.Diagnostics.AddError("networking: creating qos policy", err.Error())
		return
	}

	if !plan.Tags.IsNull() && !plan.Tags.IsUnknown() {
		var tags []string
		resp.Diagnostics.Append(plan.Tags.ElementsAs(ctx, &tags, false)...)
		if resp.Diagnostics.HasError() {
			return
		}
		if err := replaceTags(ctx, client, "qos/policies", policy.ID, tags); err != nil {
			resp.Diagnostics.AddError("networking: setting qos policy tags", err.Error())
			return
		}
	}

	notFound, readDiags := r.readInto(ctx, client, policy.ID, &plan)
	resp.Diagnostics.Append(readDiags...)
	if notFound {
		resp.Diagnostics.AddError("networking: qos policy not found after create",
			fmt.Sprintf("QoS policy %s was not found immediately after creation.", policy.ID))
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *qosPolicyResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state qosPolicyModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := r.config.NetworkV2Client()
	if err != nil {
		resp.Diagnostics.AddError("networking: building v2 client", err.Error())
		return
	}

	notFound, diags := r.readInto(ctx, client, state.ID.ValueString(), &state)
	if notFound {
		resp.Diagnostics.AddWarning("QoS policy not found",
			fmt.Sprintf("QoS policy %s no longer exists and was removed from state.", state.ID.ValueString()))
		resp.State.RemoveResource(ctx)
		return
	}
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *qosPolicyResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state qosPolicyModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := r.config.NetworkV2Client()
	if err != nil {
		resp.Diagnostics.AddError("networking: building v2 client", err.Error())
		return
	}

	updateOpts := policies.UpdateOpts{}
	if !plan.Name.Equal(state.Name) {
		updateOpts.Name = plan.Name.ValueString()
	}
	if !plan.Description.Equal(state.Description) {
		v := plan.Description.ValueString()
		updateOpts.Description = &v
	}
	if !plan.Shared.Equal(state.Shared) {
		v := plan.Shared.ValueBool()
		updateOpts.Shared = &v
	}
	if !plan.IsDefault.Equal(state.IsDefault) {
		v := plan.IsDefault.ValueBool()
		updateOpts.IsDefault = &v
	}

	id := plan.ID.ValueString()
	if _, err := policies.Update(ctx, client, id, updateOpts).Extract(); err != nil {
		resp.Diagnostics.AddError("networking: updating qos policy", err.Error())
		return
	}

	if !plan.Tags.Equal(state.Tags) {
		var tags []string
		if !plan.Tags.IsNull() && !plan.Tags.IsUnknown() {
			resp.Diagnostics.Append(plan.Tags.ElementsAs(ctx, &tags, false)...)
			if resp.Diagnostics.HasError() {
				return
			}
		}
		if err := replaceTags(ctx, client, "qos/policies", id, tags); err != nil {
			resp.Diagnostics.AddError("networking: updating qos policy tags", err.Error())
			return
		}
	}

	notFound, readDiags := r.readInto(ctx, client, id, &plan)
	resp.Diagnostics.Append(readDiags...)
	if notFound {
		resp.Diagnostics.AddError("networking: qos policy not found after update",
			fmt.Sprintf("QoS policy %s was not found immediately after update.", id))
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *qosPolicyResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state qosPolicyModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := r.config.NetworkV2Client()
	if err != nil {
		resp.Diagnostics.AddError("networking: building v2 client", err.Error())
		return
	}

	if err := policies.Delete(ctx, client, state.ID.ValueString()).ExtractErr(); err != nil {
		if gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
			return
		}
		resp.Diagnostics.AddError("networking: deleting qos policy", err.Error())
	}
}

func (r *qosPolicyResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, path.Root("id"), req, resp)
}

func (r *qosPolicyResource) readInto(ctx context.Context, client *gophercloud.ServiceClient, id string, m *qosPolicyModel) (notFound bool, diags diag.Diagnostics) {
	policy, err := policies.Get(ctx, client, id).Extract()
	if err != nil {
		if gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
			return true, diags
		}
		diags.AddError("networking: reading qos policy", err.Error())
		return false, diags
	}

	m.ID = types.StringValue(policy.ID)
	m.Name = types.StringValue(policy.Name)
	m.Description = types.StringValue(policy.Description)
	m.Shared = types.BoolValue(policy.Shared)
	m.IsDefault = types.BoolValue(policy.IsDefault)
	m.TenantID = types.StringValue(policy.TenantID)

	tagVals := policy.Tags
	if tagVals == nil {
		tagVals = []string{}
	}
	tags, d := types.SetValueFrom(ctx, types.StringType, tagVals)
	diags = append(diags, d...)
	m.Tags = tags

	if m.Region.IsNull() || m.Region.IsUnknown() {
		m.Region = types.StringValue(r.config.Region)
	}
	return false, diags
}
