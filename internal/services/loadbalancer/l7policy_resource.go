// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0
//
// Ported from terraform-provider-openstack v3.4.0
// (openstack/resource_openstack_lb_l7policy_v2.go), adapted for the
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
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/int64planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/setplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/platform9/terraform-provider-pcd/internal/clients"
)

var (
	_ resource.Resource                = (*l7PolicyResource)(nil)
	_ resource.ResourceWithConfigure   = (*l7PolicyResource)(nil)
	_ resource.ResourceWithImportState = (*l7PolicyResource)(nil)
)

// NewL7PolicyResource is the factory registered with the provider.
func NewL7PolicyResource() resource.Resource {
	return &l7PolicyResource{}
}

type l7PolicyResource struct {
	config *clients.Config
}

type l7PolicyModel struct {
	ID                 types.String `tfsdk:"id"`
	ListenerID         types.String `tfsdk:"listener_id"`
	Action             types.String `tfsdk:"action"`
	Name               types.String `tfsdk:"name"`
	Description        types.String `tfsdk:"description"`
	Position           types.Int64  `tfsdk:"position"`
	RedirectPoolID     types.String `tfsdk:"redirect_pool_id"`
	RedirectURL        types.String `tfsdk:"redirect_url"`
	RedirectPrefix     types.String `tfsdk:"redirect_prefix"`
	RedirectHTTPCode   types.Int64  `tfsdk:"redirect_http_code"`
	AdminStateUp       types.Bool   `tfsdk:"admin_state_up"`
	Tags               types.Set    `tfsdk:"tags"`
	ProvisioningStatus types.String `tfsdk:"provisioning_status"`
	OperatingStatus    types.String `tfsdk:"operating_status"`
	Region             types.String `tfsdk:"region"`
}

func (r *l7PolicyResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_lb_l7policy"
}

func (r *l7PolicyResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	useState := []planmodifier.String{stringplanmodifier.UseStateForUnknown()}
	forceNew := []planmodifier.String{stringplanmodifier.RequiresReplace()}
	resp.Schema = schema.Schema{
		MarkdownDescription: "Manages an L7 policy on an Octavia listener in PCD. The policy routes matching requests " +
			"based on its `action`.",
		Attributes: map[string]schema.Attribute{
			"id":                  schema.StringAttribute{Computed: true, MarkdownDescription: "The L7 policy ID.", PlanModifiers: useState},
			"listener_id":         schema.StringAttribute{Required: true, MarkdownDescription: "The listener this policy is attached to. Changing this forces a new resource.", PlanModifiers: forceNew},
			"action":              schema.StringAttribute{Required: true, MarkdownDescription: "The action: REDIRECT_PREFIX, REDIRECT_TO_POOL, REDIRECT_TO_URL, or REJECT."},
			"name":                schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "The name of the policy.", PlanModifiers: useState},
			"description":         schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "A description of the policy.", PlanModifiers: useState},
			"position":            schema.Int64Attribute{Optional: true, Computed: true, MarkdownDescription: "The evaluation position of the policy (Octavia renumbers as needed).", PlanModifiers: []planmodifier.Int64{int64planmodifier.UseStateForUnknown()}},
			"redirect_pool_id":    schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "Pool to redirect to (action REDIRECT_TO_POOL).", PlanModifiers: useState},
			"redirect_url":        schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "URL to redirect to (action REDIRECT_TO_URL).", PlanModifiers: useState},
			"redirect_prefix":     schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "Prefix URL to redirect to (action REDIRECT_PREFIX).", PlanModifiers: useState},
			"redirect_http_code":  schema.Int64Attribute{Optional: true, Computed: true, MarkdownDescription: "Redirect HTTP status code: 301, 302, 303, 307, or 308.", PlanModifiers: []planmodifier.Int64{int64planmodifier.UseStateForUnknown()}},
			"admin_state_up":      schema.BoolAttribute{Optional: true, Computed: true, Default: booldefault.StaticBool(true), MarkdownDescription: "The administrative state of the policy."},
			"tags":                schema.SetAttribute{Optional: true, Computed: true, ElementType: types.StringType, MarkdownDescription: "Tags applied to the policy.", PlanModifiers: []planmodifier.Set{setplanmodifier.UseStateForUnknown()}},
			"provisioning_status": schema.StringAttribute{Computed: true, MarkdownDescription: "The provisioning status.", PlanModifiers: useState},
			"operating_status":    schema.StringAttribute{Computed: true, MarkdownDescription: "The operating status.", PlanModifiers: useState},
			"region":              schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "The region. Defaults to the provider's region.", PlanModifiers: useState},
		},
	}
}

func (r *l7PolicyResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.config = configureClient(req.ProviderData, &resp.Diagnostics)
}

func (r *l7PolicyResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan l7PolicyModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := r.config.LoadBalancerV2Client()
	if err != nil {
		resp.Diagnostics.AddError("loadbalancer: building v2 client", err.Error())
		return
	}

	rootLB, err := rootLBIDFromListener(ctx, client, plan.ListenerID.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("loadbalancer: resolving root load balancer", err.Error())
		return
	}
	if err := waitForLoadBalancerActive(ctx, client, rootLB, defaultLBTimeout); err != nil {
		resp.Diagnostics.AddError("loadbalancer: waiting before l7 policy create", err.Error())
		return
	}

	adminUp := plan.AdminStateUp.ValueBool()
	createOpts := l7policies.CreateOpts{
		ListenerID:       plan.ListenerID.ValueString(),
		Action:           l7policies.Action(plan.Action.ValueString()),
		Name:             plan.Name.ValueString(),
		Description:      plan.Description.ValueString(),
		Position:         int32(plan.Position.ValueInt64()),
		RedirectPoolID:   plan.RedirectPoolID.ValueString(),
		RedirectURL:      plan.RedirectURL.ValueString(),
		RedirectPrefix:   plan.RedirectPrefix.ValueString(),
		RedirectHttpCode: int32(plan.RedirectHTTPCode.ValueInt64()),
		AdminStateUp:     &adminUp,
	}
	if !plan.Tags.IsNull() && !plan.Tags.IsUnknown() {
		resp.Diagnostics.Append(plan.Tags.ElementsAs(ctx, &createOpts.Tags, false)...)
		if resp.Diagnostics.HasError() {
			return
		}
	}

	policy, err := l7policies.Create(ctx, client, createOpts).Extract()
	if err != nil {
		resp.Diagnostics.AddError("loadbalancer: creating l7 policy", err.Error())
		return
	}
	if err := waitForLoadBalancerActive(ctx, client, rootLB, defaultLBTimeout); err != nil {
		resp.Diagnostics.AddError("loadbalancer: waiting after l7 policy create", err.Error())
		return
	}

	_, readDiags := r.readInto(ctx, client, policy.ID, &plan)
	resp.Diagnostics.Append(readDiags...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *l7PolicyResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state l7PolicyModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := r.config.LoadBalancerV2Client()
	if err != nil {
		resp.Diagnostics.AddError("loadbalancer: building v2 client", err.Error())
		return
	}

	notFound, diags := r.readInto(ctx, client, state.ID.ValueString(), &state)
	if notFound {
		resp.Diagnostics.AddWarning("L7 policy not found",
			fmt.Sprintf("L7 policy %s no longer exists and was removed from state.", state.ID.ValueString()))
		resp.State.RemoveResource(ctx)
		return
	}
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *l7PolicyResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state l7PolicyModel
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

	rootLB, err := rootLBIDFromListener(ctx, client, state.ListenerID.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("loadbalancer: resolving root load balancer", err.Error())
		return
	}

	updateOpts := l7policies.UpdateOpts{}
	if !plan.Action.Equal(state.Action) {
		updateOpts.Action = l7policies.Action(plan.Action.ValueString())
	}
	if !plan.Name.Equal(state.Name) {
		v := plan.Name.ValueString()
		updateOpts.Name = &v
	}
	if !plan.Description.Equal(state.Description) {
		v := plan.Description.ValueString()
		updateOpts.Description = &v
	}
	if !plan.Position.Equal(state.Position) {
		updateOpts.Position = int32(plan.Position.ValueInt64())
	}
	if !plan.RedirectPoolID.Equal(state.RedirectPoolID) {
		v := plan.RedirectPoolID.ValueString()
		updateOpts.RedirectPoolID = &v
	}
	if !plan.RedirectURL.Equal(state.RedirectURL) {
		v := plan.RedirectURL.ValueString()
		updateOpts.RedirectURL = &v
	}
	if !plan.RedirectPrefix.Equal(state.RedirectPrefix) {
		v := plan.RedirectPrefix.ValueString()
		updateOpts.RedirectPrefix = &v
	}
	if !plan.RedirectHTTPCode.Equal(state.RedirectHTTPCode) {
		updateOpts.RedirectHttpCode = int32(plan.RedirectHTTPCode.ValueInt64())
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
		resp.Diagnostics.AddError("loadbalancer: waiting before l7 policy update", err.Error())
		return
	}
	if _, err := l7policies.Update(ctx, client, plan.ID.ValueString(), updateOpts).Extract(); err != nil {
		resp.Diagnostics.AddError("loadbalancer: updating l7 policy", err.Error())
		return
	}
	if err := waitForLoadBalancerActive(ctx, client, rootLB, defaultLBTimeout); err != nil {
		resp.Diagnostics.AddError("loadbalancer: waiting after l7 policy update", err.Error())
		return
	}

	_, readDiags := r.readInto(ctx, client, plan.ID.ValueString(), &plan)
	resp.Diagnostics.Append(readDiags...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *l7PolicyResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state l7PolicyModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := r.config.LoadBalancerV2Client()
	if err != nil {
		resp.Diagnostics.AddError("loadbalancer: building v2 client", err.Error())
		return
	}

	rootLB, err := rootLBIDFromListener(ctx, client, state.ListenerID.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("loadbalancer: resolving root load balancer", err.Error())
		return
	}
	if err := waitForLoadBalancerActive(ctx, client, rootLB, defaultLBTimeout); err != nil {
		resp.Diagnostics.AddError("loadbalancer: waiting before l7 policy delete", err.Error())
		return
	}
	if err := l7policies.Delete(ctx, client, state.ID.ValueString()).ExtractErr(); err != nil {
		if gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
			return
		}
		resp.Diagnostics.AddError("loadbalancer: deleting l7 policy", err.Error())
		return
	}
	if err := waitForLoadBalancerActive(ctx, client, rootLB, defaultLBTimeout); err != nil {
		resp.Diagnostics.AddError("loadbalancer: waiting after l7 policy delete", err.Error())
	}
}

func (r *l7PolicyResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, path.Root("id"), req, resp)
}

func (r *l7PolicyResource) readInto(ctx context.Context, client *gophercloud.ServiceClient, id string, m *l7PolicyModel) (notFound bool, diags diag.Diagnostics) {
	p, err := l7policies.Get(ctx, client, id).Extract()
	if err != nil {
		if gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
			return true, diags
		}
		diags.AddError("loadbalancer: reading l7 policy", err.Error())
		return false, diags
	}

	m.ID = types.StringValue(p.ID)
	m.ListenerID = types.StringValue(p.ListenerID)
	m.Action = types.StringValue(p.Action)
	m.Name = types.StringValue(p.Name)
	m.Description = types.StringValue(p.Description)
	m.Position = types.Int64Value(int64(p.Position))
	m.RedirectPoolID = types.StringValue(p.RedirectPoolID)
	m.RedirectURL = types.StringValue(p.RedirectURL)
	m.RedirectPrefix = types.StringValue(p.RedirectPrefix)
	m.RedirectHTTPCode = types.Int64Value(int64(p.RedirectHttpCode))
	m.AdminStateUp = types.BoolValue(p.AdminStateUp)
	m.ProvisioningStatus = types.StringValue(p.ProvisioningStatus)
	m.OperatingStatus = types.StringValue(p.OperatingStatus)

	tagVals := p.Tags
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
