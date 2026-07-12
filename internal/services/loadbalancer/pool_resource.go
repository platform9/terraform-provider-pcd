// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0
//
// Ported from terraform-provider-openstack v3.4.0
// (openstack/resource_openstack_lb_pool_v2.go), adapted for the
// terraform-plugin-framework and PCD.

package loadbalancer

import (
	"context"
	"fmt"
	"net/http"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/loadbalancer/v2/pools"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/setplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-framework/types/basetypes"

	"github.com/platform9/terraform-provider-pcd/internal/clients"
)

var (
	_ resource.Resource                = (*poolResource)(nil)
	_ resource.ResourceWithConfigure   = (*poolResource)(nil)
	_ resource.ResourceWithImportState = (*poolResource)(nil)
)

// poolPersistenceAttrTypes is the object type of the persistence attribute.
var poolPersistenceAttrTypes = map[string]attr.Type{
	"type":        types.StringType,
	"cookie_name": types.StringType,
}

// NewPoolResource is the factory registered with the provider.
func NewPoolResource() resource.Resource {
	return &poolResource{}
}

type poolResource struct {
	config *clients.Config
}

type poolModel struct {
	ID                 types.String `tfsdk:"id"`
	Name               types.String `tfsdk:"name"`
	Description        types.String `tfsdk:"description"`
	Protocol           types.String `tfsdk:"protocol"`
	LBMethod           types.String `tfsdk:"lb_method"`
	LoadbalancerID     types.String `tfsdk:"loadbalancer_id"`
	ListenerID         types.String `tfsdk:"listener_id"`
	AdminStateUp       types.Bool   `tfsdk:"admin_state_up"`
	Persistence        types.Object `tfsdk:"persistence"`
	Tags               types.Set    `tfsdk:"tags"`
	ProjectID          types.String `tfsdk:"project_id"`
	MonitorID          types.String `tfsdk:"monitor_id"`
	ProvisioningStatus types.String `tfsdk:"provisioning_status"`
	OperatingStatus    types.String `tfsdk:"operating_status"`
	Region             types.String `tfsdk:"region"`
}

type poolPersistenceModel struct {
	Type       types.String `tfsdk:"type"`
	CookieName types.String `tfsdk:"cookie_name"`
}

func (r *poolResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_lb_pool"
}

func (r *poolResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	useState := []planmodifier.String{stringplanmodifier.UseStateForUnknown()}
	forceNew := []planmodifier.String{stringplanmodifier.RequiresReplace()}
	forceNewC := []planmodifier.String{stringplanmodifier.RequiresReplace(), stringplanmodifier.UseStateForUnknown()}
	resp.Schema = schema.Schema{
		MarkdownDescription: "Manages a pool on an Octavia load balancer in PCD. Exactly one of `loadbalancer_id` or " +
			"`listener_id` must be set.",
		Attributes: map[string]schema.Attribute{
			"id":              schema.StringAttribute{Computed: true, MarkdownDescription: "The pool ID.", PlanModifiers: useState},
			"name":            schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "The name of the pool.", PlanModifiers: useState},
			"description":     schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "A description of the pool.", PlanModifiers: useState},
			"protocol":        schema.StringAttribute{Required: true, MarkdownDescription: "The protocol: TCP, UDP, PROXY, PROXYV2, HTTP, HTTPS, or SCTP. Changing this forces a new resource.", PlanModifiers: forceNew},
			"lb_method":       schema.StringAttribute{Required: true, MarkdownDescription: "The load-balancing algorithm: ROUND_ROBIN, LEAST_CONNECTIONS, SOURCE_IP, or SOURCE_IP_PORT."},
			"loadbalancer_id": schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "The load balancer to attach the pool to (mutually exclusive with listener_id). Changing this forces a new resource.", PlanModifiers: forceNewC},
			"listener_id":     schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "The listener to attach the pool to (mutually exclusive with loadbalancer_id). Changing this forces a new resource.", PlanModifiers: forceNewC},
			"admin_state_up":  schema.BoolAttribute{Optional: true, Computed: true, Default: booldefault.StaticBool(true), MarkdownDescription: "The administrative state of the pool."},
			"persistence": schema.SingleNestedAttribute{
				Optional:            true,
				MarkdownDescription: "Session persistence for the pool.",
				Attributes: map[string]schema.Attribute{
					"type":        schema.StringAttribute{Required: true, MarkdownDescription: "Persistence type: SOURCE_IP, HTTP_COOKIE, or APP_COOKIE."},
					"cookie_name": schema.StringAttribute{Optional: true, MarkdownDescription: "The cookie name (required for and only valid with APP_COOKIE)."},
				},
			},
			"tags":                schema.SetAttribute{Optional: true, Computed: true, ElementType: types.StringType, MarkdownDescription: "Tags applied to the pool.", PlanModifiers: []planmodifier.Set{setplanmodifier.UseStateForUnknown()}},
			"project_id":          schema.StringAttribute{Computed: true, MarkdownDescription: "The owning project.", PlanModifiers: useState},
			"monitor_id":          schema.StringAttribute{Computed: true, MarkdownDescription: "The health monitor attached to the pool, if any.", PlanModifiers: useState},
			"provisioning_status": schema.StringAttribute{Computed: true, MarkdownDescription: "The provisioning status.", PlanModifiers: useState},
			"operating_status":    schema.StringAttribute{Computed: true, MarkdownDescription: "The operating status.", PlanModifiers: useState},
			"region":              schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "The region. Defaults to the provider's region.", PlanModifiers: useState},
		},
	}
}

func (r *poolResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.config = configureClient(req.ProviderData, &resp.Diagnostics)
}

func (r *poolResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan poolModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	lbSet := isSet(plan.LoadbalancerID)
	listenerSet := isSet(plan.ListenerID)
	if lbSet == listenerSet {
		resp.Diagnostics.AddError("Invalid pool", "Exactly one of loadbalancer_id or listener_id must be set.")
		return
	}

	client, err := r.config.LoadBalancerV2Client()
	if err != nil {
		resp.Diagnostics.AddError("loadbalancer: building v2 client", err.Error())
		return
	}

	rootLB, err := r.rootLBID(ctx, client, &plan)
	if err != nil {
		resp.Diagnostics.AddError("loadbalancer: resolving root load balancer", err.Error())
		return
	}
	if err := waitForLoadBalancerActive(ctx, client, rootLB, defaultLBTimeout); err != nil {
		resp.Diagnostics.AddError("loadbalancer: waiting before pool create", err.Error())
		return
	}

	adminUp := plan.AdminStateUp.ValueBool()
	createOpts := pools.CreateOpts{
		LBMethod:       pools.LBMethod(plan.LBMethod.ValueString()),
		Protocol:       pools.Protocol(plan.Protocol.ValueString()),
		LoadbalancerID: plan.LoadbalancerID.ValueString(),
		ListenerID:     plan.ListenerID.ValueString(),
		Name:           plan.Name.ValueString(),
		Description:    plan.Description.ValueString(),
		AdminStateUp:   &adminUp,
		Persistence:    persistenceFromObject(ctx, plan.Persistence, &resp.Diagnostics),
	}
	if !plan.Tags.IsNull() && !plan.Tags.IsUnknown() {
		resp.Diagnostics.Append(plan.Tags.ElementsAs(ctx, &createOpts.Tags, false)...)
	}
	if resp.Diagnostics.HasError() {
		return
	}

	pool, err := pools.Create(ctx, client, createOpts).Extract()
	if err != nil {
		resp.Diagnostics.AddError("loadbalancer: creating pool", err.Error())
		return
	}
	if err := waitForLoadBalancerActive(ctx, client, rootLB, defaultLBTimeout); err != nil {
		resp.Diagnostics.AddError("loadbalancer: waiting after pool create", err.Error())
		return
	}

	_, readDiags := r.readInto(ctx, client, pool.ID, &plan)
	resp.Diagnostics.Append(readDiags...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *poolResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state poolModel
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
		resp.Diagnostics.AddWarning("Pool not found",
			fmt.Sprintf("Pool %s no longer exists and was removed from state.", state.ID.ValueString()))
		resp.State.RemoveResource(ctx)
		return
	}
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *poolResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state poolModel
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

	rootLB, err := r.rootLBID(ctx, client, &state)
	if err != nil {
		resp.Diagnostics.AddError("loadbalancer: resolving root load balancer", err.Error())
		return
	}

	updateOpts := pools.UpdateOpts{LBMethod: pools.LBMethod(plan.LBMethod.ValueString())}
	if !plan.Name.Equal(state.Name) {
		v := plan.Name.ValueString()
		updateOpts.Name = &v
	}
	if !plan.Description.Equal(state.Description) {
		v := plan.Description.ValueString()
		updateOpts.Description = &v
	}
	if !plan.AdminStateUp.Equal(state.AdminStateUp) {
		v := plan.AdminStateUp.ValueBool()
		updateOpts.AdminStateUp = &v
	}
	if !plan.Persistence.Equal(state.Persistence) {
		if plan.Persistence.IsNull() {
			updateOpts.Persistence = &pools.SessionPersistence{} // clears (session_persistence: null)
		} else {
			updateOpts.Persistence = persistenceFromObject(ctx, plan.Persistence, &resp.Diagnostics)
		}
	}
	if !plan.Tags.Equal(state.Tags) {
		tags := []string{}
		if !plan.Tags.IsNull() && !plan.Tags.IsUnknown() {
			resp.Diagnostics.Append(plan.Tags.ElementsAs(ctx, &tags, false)...)
		}
		updateOpts.Tags = &tags
	}
	if resp.Diagnostics.HasError() {
		return
	}

	if err := waitForLoadBalancerActive(ctx, client, rootLB, defaultLBTimeout); err != nil {
		resp.Diagnostics.AddError("loadbalancer: waiting before pool update", err.Error())
		return
	}
	if _, err := pools.Update(ctx, client, plan.ID.ValueString(), updateOpts).Extract(); err != nil {
		resp.Diagnostics.AddError("loadbalancer: updating pool", err.Error())
		return
	}
	if err := waitForLoadBalancerActive(ctx, client, rootLB, defaultLBTimeout); err != nil {
		resp.Diagnostics.AddError("loadbalancer: waiting after pool update", err.Error())
		return
	}

	_, readDiags := r.readInto(ctx, client, plan.ID.ValueString(), &plan)
	resp.Diagnostics.Append(readDiags...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *poolResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state poolModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := r.config.LoadBalancerV2Client()
	if err != nil {
		resp.Diagnostics.AddError("loadbalancer: building v2 client", err.Error())
		return
	}

	rootLB, err := r.rootLBID(ctx, client, &state)
	if err != nil {
		resp.Diagnostics.AddError("loadbalancer: resolving root load balancer", err.Error())
		return
	}
	if err := waitForLoadBalancerActive(ctx, client, rootLB, defaultLBTimeout); err != nil {
		resp.Diagnostics.AddError("loadbalancer: waiting before pool delete", err.Error())
		return
	}
	if err := pools.Delete(ctx, client, state.ID.ValueString()).ExtractErr(); err != nil {
		if gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
			return
		}
		resp.Diagnostics.AddError("loadbalancer: deleting pool", err.Error())
		return
	}
	if err := waitForLoadBalancerActive(ctx, client, rootLB, defaultLBTimeout); err != nil {
		resp.Diagnostics.AddError("loadbalancer: waiting after pool delete", err.Error())
	}
}

func (r *poolResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, path.Root("id"), req, resp)
}

// rootLBID resolves the root load balancer ID from the pool's loadbalancer_id or
// (failing that) its listener.
func (r *poolResource) rootLBID(ctx context.Context, client *gophercloud.ServiceClient, m *poolModel) (string, error) {
	if isSet(m.LoadbalancerID) {
		return m.LoadbalancerID.ValueString(), nil
	}
	if isSet(m.ListenerID) {
		return rootLBIDFromListener(ctx, client, m.ListenerID.ValueString())
	}
	return "", fmt.Errorf("pool has neither loadbalancer_id nor listener_id")
}

func (r *poolResource) readInto(ctx context.Context, client *gophercloud.ServiceClient, id string, m *poolModel) (notFound bool, diags diag.Diagnostics) {
	p, err := pools.Get(ctx, client, id).Extract()
	if err != nil {
		if gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
			return true, diags
		}
		diags.AddError("loadbalancer: reading pool", err.Error())
		return false, diags
	}

	m.ID = types.StringValue(p.ID)
	m.Name = types.StringValue(p.Name)
	m.Description = types.StringValue(p.Description)
	m.Protocol = types.StringValue(p.Protocol)
	m.LBMethod = types.StringValue(p.LBMethod)
	m.AdminStateUp = types.BoolValue(p.AdminStateUp)
	m.ProjectID = types.StringValue(p.ProjectID)
	m.MonitorID = types.StringValue(p.MonitorID)
	m.ProvisioningStatus = types.StringValue(p.ProvisioningStatus)
	m.OperatingStatus = types.StringValue(p.OperatingStatus)

	// loadbalancer_id / listener_id are ForceNew; keep prior values, and derive
	// from the result only on import (no prior state).
	if !isSet(m.LoadbalancerID) && !isSet(m.ListenerID) {
		if len(p.Loadbalancers) > 0 {
			m.LoadbalancerID = types.StringValue(p.Loadbalancers[0].ID)
		}
		if len(p.Listeners) > 0 {
			m.ListenerID = types.StringValue(p.Listeners[0].ID)
		}
	}

	if p.Persistence.Type == "" {
		m.Persistence = types.ObjectNull(poolPersistenceAttrTypes)
	} else {
		obj, d := types.ObjectValueFrom(ctx, poolPersistenceAttrTypes, poolPersistenceModel{
			Type:       types.StringValue(p.Persistence.Type),
			CookieName: optionalString(p.Persistence.CookieName),
		})
		diags = append(diags, d...)
		m.Persistence = obj
	}

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

func persistenceFromObject(ctx context.Context, obj types.Object, diags *diag.Diagnostics) *pools.SessionPersistence {
	if obj.IsNull() || obj.IsUnknown() {
		return nil
	}
	var pm poolPersistenceModel
	diags.Append(obj.As(ctx, &pm, basetypes.ObjectAsOptions{})...)
	return &pools.SessionPersistence{Type: pm.Type.ValueString(), CookieName: pm.CookieName.ValueString()}
}

// optionalString keeps an empty server value as null.
func optionalString(v string) types.String {
	if v == "" {
		return types.StringNull()
	}
	return types.StringValue(v)
}
