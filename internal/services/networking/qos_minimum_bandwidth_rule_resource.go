// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0
//
// Ported from terraform-provider-openstack v3.4.0
// (openstack/resource_openstack_networking_qos_minimum_bandwidth_rule_v2.go),
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
	_ resource.Resource                = (*qosMinimumBandwidthRuleResource)(nil)
	_ resource.ResourceWithConfigure   = (*qosMinimumBandwidthRuleResource)(nil)
	_ resource.ResourceWithImportState = (*qosMinimumBandwidthRuleResource)(nil)
)

// NewQoSMinimumBandwidthRuleResource is the factory registered with the provider.
func NewQoSMinimumBandwidthRuleResource() resource.Resource {
	return &qosMinimumBandwidthRuleResource{}
}

type qosMinimumBandwidthRuleResource struct {
	config *clients.Config
}

type qosMinimumBandwidthRuleModel struct {
	ID          types.String `tfsdk:"id"`
	QoSPolicyID types.String `tfsdk:"qos_policy_id"`
	MinKBps     types.Int64  `tfsdk:"min_kbps"`
	Direction   types.String `tfsdk:"direction"`
	Region      types.String `tfsdk:"region"`
}

func (r *qosMinimumBandwidthRuleResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_networking_qos_minimum_bandwidth_rule"
}

func (r *qosMinimumBandwidthRuleResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	useState := []planmodifier.String{stringplanmodifier.UseStateForUnknown()}
	resp.Schema = schema.Schema{
		MarkdownDescription: "Manages a minimum-bandwidth (guaranteed) rule on a Neutron QoS policy in PCD.",
		Attributes: map[string]schema.Attribute{
			"id":            schema.StringAttribute{Computed: true, MarkdownDescription: "The rule ID.", PlanModifiers: useState},
			"qos_policy_id": schema.StringAttribute{Required: true, MarkdownDescription: "The QoS policy this rule belongs to. Changing this forces a new resource.", PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()}},
			"min_kbps":      schema.Int64Attribute{Required: true, MarkdownDescription: "The minimum guaranteed bandwidth in kbps."},
			"direction":     schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "The traffic direction: egress (default) or ingress.", PlanModifiers: useState},
			"region":        schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "The region. Defaults to the provider's region.", PlanModifiers: useState},
		},
	}
}

func (r *qosMinimumBandwidthRuleResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.config = configureClient(req.ProviderData, &resp.Diagnostics)
}

func (r *qosMinimumBandwidthRuleResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan qosMinimumBandwidthRuleModel
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
	rule, err := rules.CreateMinimumBandwidthRule(ctx, client, policyID, rules.CreateMinimumBandwidthRuleOpts{
		MinKBps:   int(plan.MinKBps.ValueInt64()),
		Direction: plan.Direction.ValueString(),
	}).ExtractMinimumBandwidthRule()
	if err != nil {
		resp.Diagnostics.AddError("networking: creating minimum-bandwidth rule", err.Error())
		return
	}

	notFound, readDiags := r.readInto(ctx, client, policyID, rule.ID, &plan)
	resp.Diagnostics.Append(readDiags...)
	if notFound {
		resp.Diagnostics.AddError("networking: minimum-bandwidth rule not found after create",
			fmt.Sprintf("Rule %s was not found immediately after creation.", rule.ID))
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *qosMinimumBandwidthRuleResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state qosMinimumBandwidthRuleModel
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
		resp.Diagnostics.AddWarning("Minimum-bandwidth rule not found",
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

func (r *qosMinimumBandwidthRuleResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state qosMinimumBandwidthRuleModel
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
	updateOpts := rules.UpdateMinimumBandwidthRuleOpts{}
	if !plan.MinKBps.Equal(state.MinKBps) {
		v := int(plan.MinKBps.ValueInt64())
		updateOpts.MinKBps = &v
	}
	if !plan.Direction.Equal(state.Direction) {
		updateOpts.Direction = plan.Direction.ValueString()
	}

	if _, err := rules.UpdateMinimumBandwidthRule(ctx, client, policyID, plan.ID.ValueString(), updateOpts).ExtractMinimumBandwidthRule(); err != nil {
		resp.Diagnostics.AddError("networking: updating minimum-bandwidth rule", err.Error())
		return
	}

	notFound, readDiags := r.readInto(ctx, client, policyID, plan.ID.ValueString(), &plan)
	resp.Diagnostics.Append(readDiags...)
	if notFound {
		resp.Diagnostics.AddError("networking: minimum-bandwidth rule not found after update",
			fmt.Sprintf("Rule %s was not found immediately after update.", plan.ID.ValueString()))
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *qosMinimumBandwidthRuleResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state qosMinimumBandwidthRuleModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := r.config.NetworkV2Client()
	if err != nil {
		resp.Diagnostics.AddError("networking: building v2 client", err.Error())
		return
	}

	if err := rules.DeleteMinimumBandwidthRule(ctx, client, state.QoSPolicyID.ValueString(), state.ID.ValueString()).ExtractErr(); err != nil {
		if gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
			return
		}
		resp.Diagnostics.AddError("networking: deleting minimum-bandwidth rule", err.Error())
	}
}

func (r *qosMinimumBandwidthRuleResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	policyID, ruleID, err := splitQoSRuleID(req.ID)
	if err != nil {
		resp.Diagnostics.AddError("Invalid import ID", err.Error())
		return
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), ruleID)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("qos_policy_id"), policyID)...)
}

func (r *qosMinimumBandwidthRuleResource) readInto(ctx context.Context, client *gophercloud.ServiceClient, policyID, ruleID string, m *qosMinimumBandwidthRuleModel) (notFound bool, diags diag.Diagnostics) {
	rule, err := rules.GetMinimumBandwidthRule(ctx, client, policyID, ruleID).ExtractMinimumBandwidthRule()
	if err != nil {
		if gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
			return true, diags
		}
		diags.AddError("networking: reading minimum-bandwidth rule", err.Error())
		return false, diags
	}

	m.ID = types.StringValue(rule.ID)
	m.QoSPolicyID = types.StringValue(policyID)
	m.MinKBps = types.Int64Value(int64(rule.MinKBps))
	m.Direction = types.StringValue(rule.Direction)
	if m.Region.IsNull() || m.Region.IsUnknown() {
		m.Region = types.StringValue(r.config.Region)
	}
	return false, diags
}
