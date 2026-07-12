// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0
//
// Ported from terraform-provider-openstack v3.4.0
// (openstack/resource_openstack_networking_qos_bandwidth_limit_rule_v2.go),
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
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/int64planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/platform9/terraform-provider-pcd/internal/clients"
)

var (
	_ resource.Resource                = (*qosBandwidthLimitRuleResource)(nil)
	_ resource.ResourceWithConfigure   = (*qosBandwidthLimitRuleResource)(nil)
	_ resource.ResourceWithImportState = (*qosBandwidthLimitRuleResource)(nil)
)

// NewQoSBandwidthLimitRuleResource is the factory registered with the provider.
func NewQoSBandwidthLimitRuleResource() resource.Resource {
	return &qosBandwidthLimitRuleResource{}
}

type qosBandwidthLimitRuleResource struct {
	config *clients.Config
}

type qosBandwidthLimitRuleModel struct {
	ID           types.String `tfsdk:"id"`
	QoSPolicyID  types.String `tfsdk:"qos_policy_id"`
	MaxKBps      types.Int64  `tfsdk:"max_kbps"`
	MaxBurstKBps types.Int64  `tfsdk:"max_burst_kbps"`
	Direction    types.String `tfsdk:"direction"`
	Region       types.String `tfsdk:"region"`
}

func (r *qosBandwidthLimitRuleResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_networking_qos_bandwidth_limit_rule"
}

func (r *qosBandwidthLimitRuleResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	useState := []planmodifier.String{stringplanmodifier.UseStateForUnknown()}
	resp.Schema = schema.Schema{
		MarkdownDescription: "Manages a bandwidth-limit rule on a Neutron QoS policy in PCD.",
		Attributes: map[string]schema.Attribute{
			"id":             schema.StringAttribute{Computed: true, MarkdownDescription: "The rule ID.", PlanModifiers: useState},
			"qos_policy_id":  schema.StringAttribute{Required: true, MarkdownDescription: "The QoS policy this rule belongs to. Changing this forces a new resource.", PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()}},
			"max_kbps":       schema.Int64Attribute{Required: true, MarkdownDescription: "The maximum rate in kbps."},
			"max_burst_kbps": schema.Int64Attribute{Optional: true, Computed: true, MarkdownDescription: "The maximum burst size in kilobits.", PlanModifiers: []planmodifier.Int64{int64planmodifier.UseStateForUnknown()}},
			"direction":      schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "The traffic direction: egress (default) or ingress.", PlanModifiers: useState},
			"region":         schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "The region. Defaults to the provider's region.", PlanModifiers: useState},
		},
	}
}

func (r *qosBandwidthLimitRuleResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.config = configureClient(req.ProviderData, &resp.Diagnostics)
}

func (r *qosBandwidthLimitRuleResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan qosBandwidthLimitRuleModel
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
	createOpts := rules.CreateBandwidthLimitRuleOpts{
		MaxKBps:      int(plan.MaxKBps.ValueInt64()),
		MaxBurstKBps: int(plan.MaxBurstKBps.ValueInt64()),
		Direction:    plan.Direction.ValueString(),
	}

	rule, err := rules.CreateBandwidthLimitRule(ctx, client, policyID, createOpts).ExtractBandwidthLimitRule()
	if err != nil {
		resp.Diagnostics.AddError("networking: creating bandwidth-limit rule", err.Error())
		return
	}

	notFound, readDiags := r.readInto(ctx, client, policyID, rule.ID, &plan)
	resp.Diagnostics.Append(readDiags...)
	if notFound {
		resp.Diagnostics.AddError("networking: bandwidth-limit rule not found after create",
			fmt.Sprintf("Rule %s was not found immediately after creation.", rule.ID))
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *qosBandwidthLimitRuleResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state qosBandwidthLimitRuleModel
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
		resp.Diagnostics.AddWarning("Bandwidth-limit rule not found",
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

func (r *qosBandwidthLimitRuleResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state qosBandwidthLimitRuleModel
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
	updateOpts := rules.UpdateBandwidthLimitRuleOpts{}
	if !plan.MaxKBps.Equal(state.MaxKBps) {
		v := int(plan.MaxKBps.ValueInt64())
		updateOpts.MaxKBps = &v
	}
	if !plan.MaxBurstKBps.Equal(state.MaxBurstKBps) {
		v := int(plan.MaxBurstKBps.ValueInt64())
		updateOpts.MaxBurstKBps = &v
	}
	if !plan.Direction.Equal(state.Direction) {
		updateOpts.Direction = plan.Direction.ValueString()
	}

	if _, err := rules.UpdateBandwidthLimitRule(ctx, client, policyID, plan.ID.ValueString(), updateOpts).ExtractBandwidthLimitRule(); err != nil {
		resp.Diagnostics.AddError("networking: updating bandwidth-limit rule", err.Error())
		return
	}

	notFound, readDiags := r.readInto(ctx, client, policyID, plan.ID.ValueString(), &plan)
	resp.Diagnostics.Append(readDiags...)
	if notFound {
		resp.Diagnostics.AddError("networking: bandwidth-limit rule not found after update",
			fmt.Sprintf("Rule %s was not found immediately after update.", plan.ID.ValueString()))
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *qosBandwidthLimitRuleResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state qosBandwidthLimitRuleModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := r.config.NetworkV2Client()
	if err != nil {
		resp.Diagnostics.AddError("networking: building v2 client", err.Error())
		return
	}

	if err := rules.DeleteBandwidthLimitRule(ctx, client, state.QoSPolicyID.ValueString(), state.ID.ValueString()).ExtractErr(); err != nil {
		if gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
			return
		}
		resp.Diagnostics.AddError("networking: deleting bandwidth-limit rule", err.Error())
	}
}

func (r *qosBandwidthLimitRuleResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	policyID, ruleID, err := splitQoSRuleID(req.ID)
	if err != nil {
		resp.Diagnostics.AddError("Invalid import ID", err.Error())
		return
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), ruleID)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("qos_policy_id"), policyID)...)
}

func (r *qosBandwidthLimitRuleResource) readInto(ctx context.Context, client *gophercloud.ServiceClient, policyID, ruleID string, m *qosBandwidthLimitRuleModel) (notFound bool, diags diag.Diagnostics) {
	rule, err := rules.GetBandwidthLimitRule(ctx, client, policyID, ruleID).ExtractBandwidthLimitRule()
	if err != nil {
		if gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
			return true, diags
		}
		diags.AddError("networking: reading bandwidth-limit rule", err.Error())
		return false, diags
	}

	m.ID = types.StringValue(rule.ID)
	m.QoSPolicyID = types.StringValue(policyID)
	m.MaxKBps = types.Int64Value(int64(rule.MaxKBps))
	m.MaxBurstKBps = types.Int64Value(int64(rule.MaxBurstKBps))
	m.Direction = types.StringValue(rule.Direction)
	if m.Region.IsNull() || m.Region.IsUnknown() {
		m.Region = types.StringValue(r.config.Region)
	}
	return false, diags
}
