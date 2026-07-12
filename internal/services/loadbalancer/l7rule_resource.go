// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0
//
// Ported from terraform-provider-openstack v3.4.0
// (openstack/resource_openstack_lb_l7rule_v2.go), adapted for the
// terraform-plugin-framework and PCD.

package loadbalancer

import (
	"context"
	"fmt"
	"net/http"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/loadbalancer/v2/l7policies"
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
	_ resource.Resource                = (*l7RuleResource)(nil)
	_ resource.ResourceWithConfigure   = (*l7RuleResource)(nil)
	_ resource.ResourceWithImportState = (*l7RuleResource)(nil)
)

// NewL7RuleResource is the factory registered with the provider.
func NewL7RuleResource() resource.Resource {
	return &l7RuleResource{}
}

type l7RuleResource struct {
	config *clients.Config
}

type l7RuleModel struct {
	ID                 types.String `tfsdk:"id"`
	L7PolicyID         types.String `tfsdk:"l7policy_id"`
	Type               types.String `tfsdk:"type"`
	CompareType        types.String `tfsdk:"compare_type"`
	Value              types.String `tfsdk:"value"`
	Key                types.String `tfsdk:"key"`
	Invert             types.Bool   `tfsdk:"invert"`
	AdminStateUp       types.Bool   `tfsdk:"admin_state_up"`
	Tags               types.Set    `tfsdk:"tags"`
	ProvisioningStatus types.String `tfsdk:"provisioning_status"`
	OperatingStatus    types.String `tfsdk:"operating_status"`
	Region             types.String `tfsdk:"region"`
}

func (r *l7RuleResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_lb_l7rule"
}

func (r *l7RuleResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	useState := []planmodifier.String{stringplanmodifier.UseStateForUnknown()}
	forceNew := []planmodifier.String{stringplanmodifier.RequiresReplace()}
	resp.Schema = schema.Schema{
		MarkdownDescription: "Manages a rule within an Octavia L7 policy in PCD.",
		Attributes: map[string]schema.Attribute{
			"id":                  schema.StringAttribute{Computed: true, MarkdownDescription: "The L7 rule ID.", PlanModifiers: useState},
			"l7policy_id":         schema.StringAttribute{Required: true, MarkdownDescription: "The L7 policy this rule belongs to. Changing this forces a new resource.", PlanModifiers: forceNew},
			"type":                schema.StringAttribute{Required: true, MarkdownDescription: "The rule type: COOKIE, FILE_TYPE, HEADER, HOST_NAME, PATH, SSL_CONN_HAS_CERT, SSL_VERIFY_RESULT, or SSL_DN_FIELD."},
			"compare_type":        schema.StringAttribute{Required: true, MarkdownDescription: "The comparison: CONTAINS, ENDS_WITH, EQUAL_TO, REGEX, or STARTS_WITH."},
			"value":               schema.StringAttribute{Required: true, MarkdownDescription: "The value to compare against."},
			"key":                 schema.StringAttribute{Optional: true, MarkdownDescription: "The key to match on (for COOKIE/HEADER rule types)."},
			"invert":              schema.BoolAttribute{Optional: true, Computed: true, Default: booldefault.StaticBool(false), MarkdownDescription: "Invert the match result."},
			"admin_state_up":      schema.BoolAttribute{Optional: true, Computed: true, Default: booldefault.StaticBool(true), MarkdownDescription: "The administrative state of the rule."},
			"tags":                schema.SetAttribute{Optional: true, Computed: true, ElementType: types.StringType, MarkdownDescription: "Tags applied to the rule.", PlanModifiers: []planmodifier.Set{setplanmodifier.UseStateForUnknown()}},
			"provisioning_status": schema.StringAttribute{Computed: true, MarkdownDescription: "The provisioning status.", PlanModifiers: useState},
			"operating_status":    schema.StringAttribute{Computed: true, MarkdownDescription: "The operating status.", PlanModifiers: useState},
			"region":              schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "The region. Defaults to the provider's region.", PlanModifiers: useState},
		},
	}
}

func (r *l7RuleResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.config = configureClient(req.ProviderData, &resp.Diagnostics)
}

func (r *l7RuleResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan l7RuleModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := r.config.LoadBalancerV2Client()
	if err != nil {
		resp.Diagnostics.AddError("loadbalancer: building v2 client", err.Error())
		return
	}

	policyID := plan.L7PolicyID.ValueString()
	rootLB, err := rootLBIDFromL7Policy(ctx, client, policyID)
	if err != nil {
		resp.Diagnostics.AddError("loadbalancer: resolving root load balancer", err.Error())
		return
	}
	if err := waitForLoadBalancerActive(ctx, client, rootLB, defaultLBTimeout); err != nil {
		resp.Diagnostics.AddError("loadbalancer: waiting before l7 rule create", err.Error())
		return
	}

	adminUp := plan.AdminStateUp.ValueBool()
	createOpts := l7policies.CreateRuleOpts{
		RuleType:     l7policies.RuleType(plan.Type.ValueString()),
		CompareType:  l7policies.CompareType(plan.CompareType.ValueString()),
		Value:        plan.Value.ValueString(),
		Key:          plan.Key.ValueString(),
		Invert:       plan.Invert.ValueBool(),
		AdminStateUp: &adminUp,
	}
	if !plan.Tags.IsNull() && !plan.Tags.IsUnknown() {
		resp.Diagnostics.Append(plan.Tags.ElementsAs(ctx, &createOpts.Tags, false)...)
		if resp.Diagnostics.HasError() {
			return
		}
	}

	rule, err := l7policies.CreateRule(ctx, client, policyID, createOpts).Extract()
	if err != nil {
		resp.Diagnostics.AddError("loadbalancer: creating l7 rule", err.Error())
		return
	}
	if err := waitForLoadBalancerActive(ctx, client, rootLB, defaultLBTimeout); err != nil {
		resp.Diagnostics.AddError("loadbalancer: waiting after l7 rule create", err.Error())
		return
	}

	_, readDiags := r.readInto(ctx, client, policyID, rule.ID, &plan)
	resp.Diagnostics.Append(readDiags...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *l7RuleResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state l7RuleModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := r.config.LoadBalancerV2Client()
	if err != nil {
		resp.Diagnostics.AddError("loadbalancer: building v2 client", err.Error())
		return
	}

	notFound, diags := r.readInto(ctx, client, state.L7PolicyID.ValueString(), state.ID.ValueString(), &state)
	if notFound {
		resp.Diagnostics.AddWarning("L7 rule not found",
			fmt.Sprintf("L7 rule %s no longer exists and was removed from state.", state.ID.ValueString()))
		resp.State.RemoveResource(ctx)
		return
	}
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *l7RuleResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state l7RuleModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := r.config.LoadBalancerV2Client()
	if err != nil {
		resp.Diagnostics.AddError("loadbalancer: building v2 client", err.Error())
		return
	}

	policyID := state.L7PolicyID.ValueString()
	rootLB, err := rootLBIDFromL7Policy(ctx, client, policyID)
	if err != nil {
		resp.Diagnostics.AddError("loadbalancer: resolving root load balancer", err.Error())
		return
	}

	updateOpts := l7policies.UpdateRuleOpts{
		RuleType:    l7policies.RuleType(plan.Type.ValueString()),
		CompareType: l7policies.CompareType(plan.CompareType.ValueString()),
		Value:       plan.Value.ValueString(),
	}
	if !plan.Key.Equal(state.Key) {
		v := plan.Key.ValueString()
		updateOpts.Key = &v
	}
	if !plan.Invert.Equal(state.Invert) {
		v := plan.Invert.ValueBool()
		updateOpts.Invert = &v
	}
	if !plan.AdminStateUp.Equal(state.AdminStateUp) {
		v := plan.AdminStateUp.ValueBool()
		updateOpts.AdminStateUp = &v
	}
	if !plan.Tags.Equal(state.Tags) {
		tags := []string{}
		if !plan.Tags.IsNull() && !plan.Tags.IsUnknown() {
			resp.Diagnostics.Append(plan.Tags.ElementsAs(ctx, &tags, false)...)
			if resp.Diagnostics.HasError() {
				return
			}
		}
		updateOpts.Tags = &tags
	}

	if err := waitForLoadBalancerActive(ctx, client, rootLB, defaultLBTimeout); err != nil {
		resp.Diagnostics.AddError("loadbalancer: waiting before l7 rule update", err.Error())
		return
	}
	if _, err := l7policies.UpdateRule(ctx, client, policyID, plan.ID.ValueString(), updateOpts).Extract(); err != nil {
		resp.Diagnostics.AddError("loadbalancer: updating l7 rule", err.Error())
		return
	}
	if err := waitForLoadBalancerActive(ctx, client, rootLB, defaultLBTimeout); err != nil {
		resp.Diagnostics.AddError("loadbalancer: waiting after l7 rule update", err.Error())
		return
	}

	_, readDiags := r.readInto(ctx, client, policyID, plan.ID.ValueString(), &plan)
	resp.Diagnostics.Append(readDiags...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *l7RuleResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state l7RuleModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := r.config.LoadBalancerV2Client()
	if err != nil {
		resp.Diagnostics.AddError("loadbalancer: building v2 client", err.Error())
		return
	}

	policyID := state.L7PolicyID.ValueString()
	rootLB, err := rootLBIDFromL7Policy(ctx, client, policyID)
	if err != nil {
		if gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
			return // the policy (and thus the rule) is already gone
		}
		resp.Diagnostics.AddError("loadbalancer: resolving root load balancer", err.Error())
		return
	}
	if err := waitForLoadBalancerActive(ctx, client, rootLB, defaultLBTimeout); err != nil {
		resp.Diagnostics.AddError("loadbalancer: waiting before l7 rule delete", err.Error())
		return
	}
	if err := l7policies.DeleteRule(ctx, client, policyID, state.ID.ValueString()).ExtractErr(); err != nil {
		if gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
			return
		}
		resp.Diagnostics.AddError("loadbalancer: deleting l7 rule", err.Error())
		return
	}
	if err := waitForLoadBalancerActive(ctx, client, rootLB, defaultLBTimeout); err != nil {
		resp.Diagnostics.AddError("loadbalancer: waiting after l7 rule delete", err.Error())
	}
}

func (r *l7RuleResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	policyID, ruleID, err := splitParentChildID(req.ID)
	if err != nil {
		resp.Diagnostics.AddError("Invalid import ID", err.Error())
		return
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), ruleID)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("l7policy_id"), policyID)...)
}

func (r *l7RuleResource) readInto(ctx context.Context, client *gophercloud.ServiceClient, policyID, ruleID string, m *l7RuleModel) (notFound bool, diags diag.Diagnostics) {
	rule, err := l7policies.GetRule(ctx, client, policyID, ruleID).Extract()
	if err != nil {
		if gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
			return true, diags
		}
		diags.AddError("loadbalancer: reading l7 rule", err.Error())
		return false, diags
	}

	m.ID = types.StringValue(rule.ID)
	m.L7PolicyID = types.StringValue(policyID)
	m.Type = types.StringValue(rule.RuleType)
	m.CompareType = types.StringValue(rule.CompareType)
	m.Value = types.StringValue(rule.Value)
	m.Key = optionalString(rule.Key)
	m.Invert = types.BoolValue(rule.Invert)
	m.AdminStateUp = types.BoolValue(rule.AdminStateUp)
	m.ProvisioningStatus = types.StringValue(rule.ProvisioningStatus)
	m.OperatingStatus = types.StringValue(rule.OperatingStatus)

	tagVals := rule.Tags
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
