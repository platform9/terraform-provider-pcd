// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0
//
// Ported from terraform-provider-openstack v3.4.0
// (openstack/resource_openstack_lb_monitor_v2.go), adapted for the
// terraform-plugin-framework and PCD.

package loadbalancer

import (
	"context"
	"fmt"
	"net/http"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/loadbalancer/v2/monitors"
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
	_ resource.Resource                = (*monitorResource)(nil)
	_ resource.ResourceWithConfigure   = (*monitorResource)(nil)
	_ resource.ResourceWithImportState = (*monitorResource)(nil)
)

// NewMonitorResource is the factory registered with the provider.
func NewMonitorResource() resource.Resource {
	return &monitorResource{}
}

type monitorResource struct {
	config *clients.Config
}

type monitorModel struct {
	ID                 types.String `tfsdk:"id"`
	PoolID             types.String `tfsdk:"pool_id"`
	Type               types.String `tfsdk:"type"`
	Delay              types.Int64  `tfsdk:"delay"`
	Timeout            types.Int64  `tfsdk:"timeout"`
	MaxRetries         types.Int64  `tfsdk:"max_retries"`
	MaxRetriesDown     types.Int64  `tfsdk:"max_retries_down"`
	HTTPMethod         types.String `tfsdk:"http_method"`
	URLPath            types.String `tfsdk:"url_path"`
	ExpectedCodes      types.String `tfsdk:"expected_codes"`
	Name               types.String `tfsdk:"name"`
	AdminStateUp       types.Bool   `tfsdk:"admin_state_up"`
	Tags               types.Set    `tfsdk:"tags"`
	ProvisioningStatus types.String `tfsdk:"provisioning_status"`
	OperatingStatus    types.String `tfsdk:"operating_status"`
	Region             types.String `tfsdk:"region"`
}

func (r *monitorResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_lb_monitor"
}

func (r *monitorResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	useState := []planmodifier.String{stringplanmodifier.UseStateForUnknown()}
	forceNew := []planmodifier.String{stringplanmodifier.RequiresReplace()}
	resp.Schema = schema.Schema{
		MarkdownDescription: "Manages a health monitor on an Octavia load balancer pool in PCD.",
		Attributes: map[string]schema.Attribute{
			"id":                  schema.StringAttribute{Computed: true, MarkdownDescription: "The health monitor ID.", PlanModifiers: useState},
			"pool_id":             schema.StringAttribute{Required: true, MarkdownDescription: "The pool to monitor. Changing this forces a new resource.", PlanModifiers: forceNew},
			"type":                schema.StringAttribute{Required: true, MarkdownDescription: "The monitor type: HTTP, HTTPS, PING, TCP, TLS-HELLO, UDP-CONNECT, or SCTP. Changing this forces a new resource.", PlanModifiers: forceNew},
			"delay":               schema.Int64Attribute{Required: true, MarkdownDescription: "Seconds between health checks."},
			"timeout":             schema.Int64Attribute{Required: true, MarkdownDescription: "Seconds to wait for a check to succeed (must be less than delay)."},
			"max_retries":         schema.Int64Attribute{Required: true, MarkdownDescription: "Successful checks before marking a member up (1-10)."},
			"max_retries_down":    schema.Int64Attribute{Optional: true, Computed: true, MarkdownDescription: "Failed checks before marking a member down.", PlanModifiers: []planmodifier.Int64{int64planmodifier.UseStateForUnknown()}},
			"http_method":         schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "HTTP method for HTTP(S) checks (default GET).", PlanModifiers: useState},
			"url_path":            schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "URL path for HTTP(S) checks (default /).", PlanModifiers: useState},
			"expected_codes":      schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "Expected HTTP status codes for HTTP(S) checks (default 200).", PlanModifiers: useState},
			"name":                schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "The name of the monitor.", PlanModifiers: useState},
			"admin_state_up":      schema.BoolAttribute{Optional: true, Computed: true, Default: booldefault.StaticBool(true), MarkdownDescription: "The administrative state of the monitor."},
			"tags":                schema.SetAttribute{Optional: true, Computed: true, ElementType: types.StringType, MarkdownDescription: "Tags applied to the monitor.", PlanModifiers: []planmodifier.Set{setplanmodifier.UseStateForUnknown()}},
			"provisioning_status": schema.StringAttribute{Computed: true, MarkdownDescription: "The provisioning status.", PlanModifiers: useState},
			"operating_status":    schema.StringAttribute{Computed: true, MarkdownDescription: "The operating status.", PlanModifiers: useState},
			"region":              schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "The region. Defaults to the provider's region.", PlanModifiers: useState},
		},
	}
}

func (r *monitorResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.config = configureClient(req.ProviderData, &resp.Diagnostics)
}

func (r *monitorResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan monitorModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := r.config.LoadBalancerV2Client()
	if err != nil {
		resp.Diagnostics.AddError("loadbalancer: building v2 client", err.Error())
		return
	}

	poolID := plan.PoolID.ValueString()
	rootLB, err := rootLBIDFromPool(ctx, client, poolID)
	if err != nil {
		resp.Diagnostics.AddError("loadbalancer: resolving root load balancer", err.Error())
		return
	}
	if err := waitForLoadBalancerActive(ctx, client, rootLB, defaultLBTimeout); err != nil {
		resp.Diagnostics.AddError("loadbalancer: waiting before monitor create", err.Error())
		return
	}

	adminUp := plan.AdminStateUp.ValueBool()
	createOpts := monitors.CreateOpts{
		PoolID:       poolID,
		Type:         plan.Type.ValueString(),
		Delay:        int(plan.Delay.ValueInt64()),
		Timeout:      int(plan.Timeout.ValueInt64()),
		MaxRetries:   int(plan.MaxRetries.ValueInt64()),
		Name:         plan.Name.ValueString(),
		AdminStateUp: &adminUp,
	}
	if p := intPtrIfSet(plan.MaxRetriesDown); p != nil {
		createOpts.MaxRetriesDown = *p
	}
	if isHTTPMonitor(plan.Type.ValueString()) {
		createOpts.HTTPMethod = plan.HTTPMethod.ValueString()
		createOpts.URLPath = plan.URLPath.ValueString()
		createOpts.ExpectedCodes = plan.ExpectedCodes.ValueString()
	}
	if !plan.Tags.IsNull() && !plan.Tags.IsUnknown() {
		resp.Diagnostics.Append(plan.Tags.ElementsAs(ctx, &createOpts.Tags, false)...)
		if resp.Diagnostics.HasError() {
			return
		}
	}

	mon, err := monitors.Create(ctx, client, createOpts).Extract()
	if err != nil {
		resp.Diagnostics.AddError("loadbalancer: creating monitor", err.Error())
		return
	}
	if err := waitForLoadBalancerActive(ctx, client, rootLB, defaultLBTimeout); err != nil {
		resp.Diagnostics.AddError("loadbalancer: waiting after monitor create", err.Error())
		return
	}

	_, readDiags := r.readInto(ctx, client, mon.ID, &plan)
	resp.Diagnostics.Append(readDiags...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *monitorResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state monitorModel
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
		resp.Diagnostics.AddWarning("Monitor not found",
			fmt.Sprintf("Monitor %s no longer exists and was removed from state.", state.ID.ValueString()))
		resp.State.RemoveResource(ctx)
		return
	}
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *monitorResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state monitorModel
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

	poolID := state.PoolID.ValueString()
	rootLB, err := rootLBIDFromPool(ctx, client, poolID)
	if err != nil {
		resp.Diagnostics.AddError("loadbalancer: resolving root load balancer", err.Error())
		return
	}

	updateOpts := monitors.UpdateOpts{
		Delay:      int(plan.Delay.ValueInt64()),
		Timeout:    int(plan.Timeout.ValueInt64()),
		MaxRetries: int(plan.MaxRetries.ValueInt64()),
	}
	if p := intPtrIfSet(plan.MaxRetriesDown); p != nil {
		updateOpts.MaxRetriesDown = *p
	}
	if !plan.Name.Equal(state.Name) {
		v := plan.Name.ValueString()
		updateOpts.Name = &v
	}
	if !plan.AdminStateUp.Equal(state.AdminStateUp) {
		v := plan.AdminStateUp.ValueBool()
		updateOpts.AdminStateUp = &v
	}
	if isHTTPMonitor(plan.Type.ValueString()) {
		updateOpts.HTTPMethod = plan.HTTPMethod.ValueString()
		updateOpts.URLPath = plan.URLPath.ValueString()
		updateOpts.ExpectedCodes = plan.ExpectedCodes.ValueString()
	}
	if !plan.Tags.Equal(state.Tags) {
		tags := []string{}
		if !plan.Tags.IsNull() && !plan.Tags.IsUnknown() {
			resp.Diagnostics.Append(plan.Tags.ElementsAs(ctx, &tags, false)...)
			if resp.Diagnostics.HasError() {
				return
			}
		}
		updateOpts.Tags = tags
	}

	if err := waitForLoadBalancerActive(ctx, client, rootLB, defaultLBTimeout); err != nil {
		resp.Diagnostics.AddError("loadbalancer: waiting before monitor update", err.Error())
		return
	}
	if _, err := monitors.Update(ctx, client, plan.ID.ValueString(), updateOpts).Extract(); err != nil {
		resp.Diagnostics.AddError("loadbalancer: updating monitor", err.Error())
		return
	}
	if err := waitForLoadBalancerActive(ctx, client, rootLB, defaultLBTimeout); err != nil {
		resp.Diagnostics.AddError("loadbalancer: waiting after monitor update", err.Error())
		return
	}

	_, readDiags := r.readInto(ctx, client, plan.ID.ValueString(), &plan)
	resp.Diagnostics.Append(readDiags...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *monitorResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state monitorModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := r.config.LoadBalancerV2Client()
	if err != nil {
		resp.Diagnostics.AddError("loadbalancer: building v2 client", err.Error())
		return
	}

	poolID := state.PoolID.ValueString()
	rootLB, err := rootLBIDFromPool(ctx, client, poolID)
	if err != nil {
		resp.Diagnostics.AddError("loadbalancer: resolving root load balancer", err.Error())
		return
	}
	if err := waitForLoadBalancerActive(ctx, client, rootLB, defaultLBTimeout); err != nil {
		resp.Diagnostics.AddError("loadbalancer: waiting before monitor delete", err.Error())
		return
	}
	if err := monitors.Delete(ctx, client, state.ID.ValueString()).ExtractErr(); err != nil {
		if gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
			return
		}
		resp.Diagnostics.AddError("loadbalancer: deleting monitor", err.Error())
		return
	}
	if err := waitForLoadBalancerActive(ctx, client, rootLB, defaultLBTimeout); err != nil {
		resp.Diagnostics.AddError("loadbalancer: waiting after monitor delete", err.Error())
	}
}

func (r *monitorResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, path.Root("id"), req, resp)
}

func (r *monitorResource) readInto(ctx context.Context, client *gophercloud.ServiceClient, id string, m *monitorModel) (notFound bool, diags diag.Diagnostics) {
	mon, err := monitors.Get(ctx, client, id).Extract()
	if err != nil {
		if gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
			return true, diags
		}
		diags.AddError("loadbalancer: reading monitor", err.Error())
		return false, diags
	}

	m.ID = types.StringValue(mon.ID)
	if len(mon.Pools) > 0 && (m.PoolID.IsNull() || m.PoolID.IsUnknown()) {
		m.PoolID = types.StringValue(mon.Pools[0].ID)
	}
	m.Type = types.StringValue(mon.Type)
	m.Delay = types.Int64Value(int64(mon.Delay))
	m.Timeout = types.Int64Value(int64(mon.Timeout))
	m.MaxRetries = types.Int64Value(int64(mon.MaxRetries))
	m.MaxRetriesDown = types.Int64Value(int64(mon.MaxRetriesDown))
	m.HTTPMethod = types.StringValue(mon.HTTPMethod)
	m.URLPath = types.StringValue(mon.URLPath)
	m.ExpectedCodes = types.StringValue(mon.ExpectedCodes)
	m.Name = types.StringValue(mon.Name)
	m.AdminStateUp = types.BoolValue(mon.AdminStateUp)
	m.ProvisioningStatus = types.StringValue(mon.ProvisioningStatus)
	m.OperatingStatus = types.StringValue(mon.OperatingStatus)

	tagVals := mon.Tags
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

// isHTTPMonitor reports whether a monitor type uses the HTTP probe fields.
func isHTTPMonitor(t string) bool {
	return t == "HTTP" || t == "HTTPS"
}
