// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0
//
// Ported from terraform-provider-openstack v3.4.0
// (openstack/resource_openstack_networking_qos_dscp_marking_rule_v2.go),
// adapted for the terraform-plugin-framework and PCD.

package networking

import (
	"context"
	"fmt"
	"net/http"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/extensions/qos/rules"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/platform9/terraform-provider-pcd/internal/clients"
)

var (
	_ resource.Resource                = (*qosDSCPMarkingRuleResource)(nil)
	_ resource.ResourceWithConfigure   = (*qosDSCPMarkingRuleResource)(nil)
	_ resource.ResourceWithImportState = (*qosDSCPMarkingRuleResource)(nil)
)

// NewQoSDSCPMarkingRuleResource is the factory registered with the provider.
func NewQoSDSCPMarkingRuleResource() resource.Resource {
	return &qosDSCPMarkingRuleResource{}
}

type qosDSCPMarkingRuleResource struct {
	config *clients.Config
}

type qosDSCPMarkingRuleModel struct {
	ID          types.String `tfsdk:"id"`
	QoSPolicyID types.String `tfsdk:"qos_policy_id"`
	DSCPMark    types.Int64  `tfsdk:"dscp_mark"`
	Region      types.String `tfsdk:"region"`
}

func (r *qosDSCPMarkingRuleResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_networking_qos_dscp_marking_rule"
}

func (r *qosDSCPMarkingRuleResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	useState := []planmodifier.String{stringplanmodifier.UseStateForUnknown()}
	resp.Schema = schema.Schema{
		MarkdownDescription: "Manages a DSCP-marking rule on a Neutron QoS policy in PCD.",
		Attributes: map[string]schema.Attribute{
			"id":            schema.StringAttribute{Computed: true, MarkdownDescription: "The rule ID.", PlanModifiers: useState},
			"qos_policy_id": schema.StringAttribute{Required: true, MarkdownDescription: "The QoS policy this rule belongs to. Changing this forces a new resource.", PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()}},
			"dscp_mark":     schema.Int64Attribute{Required: true, MarkdownDescription: "The DSCP mark value (0, 8-56 in valid increments)."},
			"region":        schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "The region. Defaults to the provider's region.", PlanModifiers: useState},
		},
	}
}

func (r *qosDSCPMarkingRuleResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.config = configureClient(req.ProviderData, &resp.Diagnostics)
}

func (r *qosDSCPMarkingRuleResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan qosDSCPMarkingRuleModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := r.config.NetworkV2Client()
	if err != nil {
		resp.Diagnostics.AddError("networking: building v2 client", err.Error())
		return
	}

	policyID := plan.QoSPolicyID.ValueString()
	rule, err := rules.CreateDSCPMarkingRule(ctx, client, policyID, rules.CreateDSCPMarkingRuleOpts{
		DSCPMark: int(plan.DSCPMark.ValueInt64()),
	}).ExtractDSCPMarkingRule()
	if err != nil {
		resp.Diagnostics.AddError("networking: creating dscp-marking rule", err.Error())
		return
	}

	notFound, readDiags := r.readInto(ctx, client, policyID, rule.ID, &plan)
	resp.Diagnostics.Append(readDiags...)
	if notFound {
		resp.Diagnostics.AddError("networking: dscp-marking rule not found after create",
			fmt.Sprintf("Rule %s was not found immediately after creation.", rule.ID))
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *qosDSCPMarkingRuleResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state qosDSCPMarkingRuleModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := r.config.NetworkV2Client()
	if err != nil {
		resp.Diagnostics.AddError("networking: building v2 client", err.Error())
		return
	}

	notFound, diags := r.readInto(ctx, client, state.QoSPolicyID.ValueString(), state.ID.ValueString(), &state)
	if notFound {
		resp.Diagnostics.AddWarning("DSCP-marking rule not found",
			fmt.Sprintf("Rule %s no longer exists and was removed from state.", state.ID.ValueString()))
		resp.State.RemoveResource(ctx)
		return
	}
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *qosDSCPMarkingRuleResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state qosDSCPMarkingRuleModel
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

	policyID := state.QoSPolicyID.ValueString()
	mark := int(plan.DSCPMark.ValueInt64())
	if _, err := rules.UpdateDSCPMarkingRule(ctx, client, policyID, plan.ID.ValueString(), rules.UpdateDSCPMarkingRuleOpts{
		DSCPMark: &mark,
	}).ExtractDSCPMarkingRule(); err != nil {
		resp.Diagnostics.AddError("networking: updating dscp-marking rule", err.Error())
		return
	}

	notFound, readDiags := r.readInto(ctx, client, policyID, plan.ID.ValueString(), &plan)
	resp.Diagnostics.Append(readDiags...)
	if notFound {
		resp.Diagnostics.AddError("networking: dscp-marking rule not found after update",
			fmt.Sprintf("Rule %s was not found immediately after update.", plan.ID.ValueString()))
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *qosDSCPMarkingRuleResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state qosDSCPMarkingRuleModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := r.config.NetworkV2Client()
	if err != nil {
		resp.Diagnostics.AddError("networking: building v2 client", err.Error())
		return
	}

	if err := rules.DeleteDSCPMarkingRule(ctx, client, state.QoSPolicyID.ValueString(), state.ID.ValueString()).ExtractErr(); err != nil {
		if gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
			return
		}
		resp.Diagnostics.AddError("networking: deleting dscp-marking rule", err.Error())
	}
}

func (r *qosDSCPMarkingRuleResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	policyID, ruleID, err := splitQoSRuleID(req.ID)
	if err != nil {
		resp.Diagnostics.AddError("Invalid import ID", err.Error())
		return
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), ruleID)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("qos_policy_id"), policyID)...)
}

func (r *qosDSCPMarkingRuleResource) readInto(ctx context.Context, client *gophercloud.ServiceClient, policyID, ruleID string, m *qosDSCPMarkingRuleModel) (notFound bool, diags diag.Diagnostics) {
	rule, err := rules.GetDSCPMarkingRule(ctx, client, policyID, ruleID).ExtractDSCPMarkingRule()
	if err != nil {
		if gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
			return true, diags
		}
		diags.AddError("networking: reading dscp-marking rule", err.Error())
		return false, diags
	}

	m.ID = types.StringValue(rule.ID)
	m.QoSPolicyID = types.StringValue(policyID)
	m.DSCPMark = types.Int64Value(int64(rule.DSCPMark))
	if m.Region.IsNull() || m.Region.IsUnknown() {
		m.Region = types.StringValue(r.config.Region)
	}
	return false, diags
}
