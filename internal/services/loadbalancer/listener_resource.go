// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0
//
// Ported from terraform-provider-openstack v3.4.0
// (openstack/resource_openstack_lb_listener_v2.go), adapted for the
// terraform-plugin-framework and PCD.

package loadbalancer

import (
	"context"
	"fmt"
	"net/http"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/loadbalancer/v2/listeners"
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
	_ resource.Resource                = (*listenerResource)(nil)
	_ resource.ResourceWithConfigure   = (*listenerResource)(nil)
	_ resource.ResourceWithImportState = (*listenerResource)(nil)
)

// NewListenerResource is the factory registered with the provider.
func NewListenerResource() resource.Resource {
	return &listenerResource{}
}

type listenerResource struct {
	config *clients.Config
}

type listenerModel struct {
	ID                     types.String `tfsdk:"id"`
	LoadbalancerID         types.String `tfsdk:"loadbalancer_id"`
	Protocol               types.String `tfsdk:"protocol"`
	ProtocolPort           types.Int64  `tfsdk:"protocol_port"`
	Name                   types.String `tfsdk:"name"`
	Description            types.String `tfsdk:"description"`
	DefaultPoolID          types.String `tfsdk:"default_pool_id"`
	ConnectionLimit        types.Int64  `tfsdk:"connection_limit"`
	DefaultTLSContainerRef types.String `tfsdk:"default_tls_container_ref"`
	SNIContainerRefs       types.List   `tfsdk:"sni_container_refs"`
	AdminStateUp           types.Bool   `tfsdk:"admin_state_up"`
	TimeoutClientData      types.Int64  `tfsdk:"timeout_client_data"`
	TimeoutMemberConnect   types.Int64  `tfsdk:"timeout_member_connect"`
	TimeoutMemberData      types.Int64  `tfsdk:"timeout_member_data"`
	TimeoutTCPInspect      types.Int64  `tfsdk:"timeout_tcp_inspect"`
	InsertHeaders          types.Map    `tfsdk:"insert_headers"`
	AllowedCIDRs           types.List   `tfsdk:"allowed_cidrs"`
	Tags                   types.Set    `tfsdk:"tags"`
	ProvisioningStatus     types.String `tfsdk:"provisioning_status"`
	OperatingStatus        types.String `tfsdk:"operating_status"`
	Region                 types.String `tfsdk:"region"`
}

func (r *listenerResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_lb_listener"
}

func (r *listenerResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	useState := []planmodifier.String{stringplanmodifier.UseStateForUnknown()}
	forceNew := []planmodifier.String{stringplanmodifier.RequiresReplace()}
	tinms := " (in milliseconds)."
	resp.Schema = schema.Schema{
		MarkdownDescription: "Manages a listener on an Octavia load balancer in PCD.",
		Attributes: map[string]schema.Attribute{
			"id":              schema.StringAttribute{Computed: true, MarkdownDescription: "The listener ID.", PlanModifiers: useState},
			"loadbalancer_id": schema.StringAttribute{Required: true, MarkdownDescription: "The load balancer this listener belongs to. Changing this forces a new resource.", PlanModifiers: forceNew},
			"protocol":        schema.StringAttribute{Required: true, MarkdownDescription: "The protocol: HTTP, HTTPS, TCP, TERMINATED_HTTPS, or UDP. Changing this forces a new resource.", PlanModifiers: forceNew},
			"protocol_port":   schema.Int64Attribute{Required: true, MarkdownDescription: "The port to listen on. Changing this forces a new resource.", PlanModifiers: []planmodifier.Int64{int64planmodifier.RequiresReplace()}},
			"name":            schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "The name of the listener.", PlanModifiers: useState},
			"description":     schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "A description of the listener.", PlanModifiers: useState},
			"default_pool_id": schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "The default pool for the listener.", PlanModifiers: useState},
			"connection_limit": schema.Int64Attribute{Optional: true, Computed: true, MarkdownDescription: "Maximum connections (-1 for unlimited).",
				PlanModifiers: []planmodifier.Int64{int64planmodifier.UseStateForUnknown()}},
			"default_tls_container_ref": schema.StringAttribute{Optional: true, MarkdownDescription: "Barbican secret ref for the default TLS certificate (TERMINATED_HTTPS)."},
			"sni_container_refs":        schema.ListAttribute{Optional: true, ElementType: types.StringType, MarkdownDescription: "Barbican secret refs for SNI certificates."},
			"admin_state_up":            schema.BoolAttribute{Optional: true, Computed: true, Default: booldefault.StaticBool(true), MarkdownDescription: "The administrative state of the listener."},
			"timeout_client_data":       schema.Int64Attribute{Optional: true, Computed: true, MarkdownDescription: "Frontend client inactivity timeout" + tinms, PlanModifiers: []planmodifier.Int64{int64planmodifier.UseStateForUnknown()}},
			"timeout_member_connect":    schema.Int64Attribute{Optional: true, Computed: true, MarkdownDescription: "Backend member connection timeout" + tinms, PlanModifiers: []planmodifier.Int64{int64planmodifier.UseStateForUnknown()}},
			"timeout_member_data":       schema.Int64Attribute{Optional: true, Computed: true, MarkdownDescription: "Backend member inactivity timeout" + tinms, PlanModifiers: []planmodifier.Int64{int64planmodifier.UseStateForUnknown()}},
			"timeout_tcp_inspect":       schema.Int64Attribute{Optional: true, Computed: true, MarkdownDescription: "TCP inspection timeout" + tinms, PlanModifiers: []planmodifier.Int64{int64planmodifier.UseStateForUnknown()}},
			"insert_headers":            schema.MapAttribute{Optional: true, ElementType: types.StringType, MarkdownDescription: "Headers to insert into requests before forwarding (e.g. X-Forwarded-For)."},
			"allowed_cidrs":             schema.ListAttribute{Optional: true, ElementType: types.StringType, MarkdownDescription: "CIDRs allowed to connect to the listener."},
			"tags":                      schema.SetAttribute{Optional: true, Computed: true, ElementType: types.StringType, MarkdownDescription: "Tags applied to the listener.", PlanModifiers: []planmodifier.Set{setplanmodifier.UseStateForUnknown()}},
			"provisioning_status":       schema.StringAttribute{Computed: true, MarkdownDescription: "The provisioning status.", PlanModifiers: useState},
			"operating_status":          schema.StringAttribute{Computed: true, MarkdownDescription: "The operating status.", PlanModifiers: useState},
			"region":                    schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "The region. Defaults to the provider's region.", PlanModifiers: useState},
		},
	}
}

func (r *listenerResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.config = configureClient(req.ProviderData, &resp.Diagnostics)
}

func (r *listenerResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan listenerModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := r.config.LoadBalancerV2Client()
	if err != nil {
		resp.Diagnostics.AddError("loadbalancer: building v2 client", err.Error())
		return
	}

	lbID := plan.LoadbalancerID.ValueString()
	if err := waitForLoadBalancerActive(ctx, client, lbID, defaultLBTimeout); err != nil {
		resp.Diagnostics.AddError("loadbalancer: waiting before listener create", err.Error())
		return
	}

	adminUp := plan.AdminStateUp.ValueBool()
	createOpts := listeners.CreateOpts{
		LoadbalancerID:         lbID,
		Protocol:               listeners.Protocol(plan.Protocol.ValueString()),
		ProtocolPort:           int(plan.ProtocolPort.ValueInt64()),
		Name:                   plan.Name.ValueString(),
		Description:            plan.Description.ValueString(),
		DefaultPoolID:          plan.DefaultPoolID.ValueString(),
		DefaultTlsContainerRef: plan.DefaultTLSContainerRef.ValueString(),
		AdminStateUp:           &adminUp,
		ConnLimit:              intPtrIfSet(plan.ConnectionLimit),
		TimeoutClientData:      intPtrIfSet(plan.TimeoutClientData),
		TimeoutMemberConnect:   intPtrIfSet(plan.TimeoutMemberConnect),
		TimeoutMemberData:      intPtrIfSet(plan.TimeoutMemberData),
		TimeoutTCPInspect:      intPtrIfSet(plan.TimeoutTCPInspect),
		SniContainerRefs:       listToStrings(ctx, plan.SNIContainerRefs, &resp.Diagnostics),
		AllowedCIDRs:           listToStrings(ctx, plan.AllowedCIDRs, &resp.Diagnostics),
		InsertHeaders:          mapToStrings(ctx, plan.InsertHeaders, &resp.Diagnostics),
	}
	if !plan.Tags.IsNull() && !plan.Tags.IsUnknown() {
		resp.Diagnostics.Append(plan.Tags.ElementsAs(ctx, &createOpts.Tags, false)...)
	}
	if resp.Diagnostics.HasError() {
		return
	}

	listener, err := listeners.Create(ctx, client, createOpts).Extract()
	if err != nil {
		resp.Diagnostics.AddError("loadbalancer: creating listener", err.Error())
		return
	}
	if err := waitForLoadBalancerActive(ctx, client, lbID, defaultLBTimeout); err != nil {
		resp.Diagnostics.AddError("loadbalancer: waiting after listener create", err.Error())
		return
	}

	_, readDiags := r.readInto(ctx, client, listener.ID, &plan)
	resp.Diagnostics.Append(readDiags...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *listenerResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state listenerModel
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
		resp.Diagnostics.AddWarning("Listener not found",
			fmt.Sprintf("Listener %s no longer exists and was removed from state.", state.ID.ValueString()))
		resp.State.RemoveResource(ctx)
		return
	}
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *listenerResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state listenerModel
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

	lbID := state.LoadbalancerID.ValueString()
	updateOpts := listeners.UpdateOpts{}
	if !plan.Name.Equal(state.Name) {
		v := plan.Name.ValueString()
		updateOpts.Name = &v
	}
	if !plan.Description.Equal(state.Description) {
		v := plan.Description.ValueString()
		updateOpts.Description = &v
	}
	if !plan.DefaultPoolID.Equal(state.DefaultPoolID) {
		v := plan.DefaultPoolID.ValueString()
		updateOpts.DefaultPoolID = &v
	}
	if !plan.DefaultTLSContainerRef.Equal(state.DefaultTLSContainerRef) {
		v := plan.DefaultTLSContainerRef.ValueString()
		updateOpts.DefaultTlsContainerRef = &v
	}
	if !plan.AdminStateUp.Equal(state.AdminStateUp) {
		v := plan.AdminStateUp.ValueBool()
		updateOpts.AdminStateUp = &v
	}
	if !plan.ConnectionLimit.Equal(state.ConnectionLimit) {
		updateOpts.ConnLimit = intPtrIfSet(plan.ConnectionLimit)
	}
	if !plan.TimeoutClientData.Equal(state.TimeoutClientData) {
		updateOpts.TimeoutClientData = intPtrIfSet(plan.TimeoutClientData)
	}
	if !plan.TimeoutMemberConnect.Equal(state.TimeoutMemberConnect) {
		updateOpts.TimeoutMemberConnect = intPtrIfSet(plan.TimeoutMemberConnect)
	}
	if !plan.TimeoutMemberData.Equal(state.TimeoutMemberData) {
		updateOpts.TimeoutMemberData = intPtrIfSet(plan.TimeoutMemberData)
	}
	if !plan.TimeoutTCPInspect.Equal(state.TimeoutTCPInspect) {
		updateOpts.TimeoutTCPInspect = intPtrIfSet(plan.TimeoutTCPInspect)
	}
	if !plan.SNIContainerRefs.Equal(state.SNIContainerRefs) {
		v := listToStrings(ctx, plan.SNIContainerRefs, &resp.Diagnostics)
		updateOpts.SniContainerRefs = &v
	}
	if !plan.AllowedCIDRs.Equal(state.AllowedCIDRs) {
		v := listToStrings(ctx, plan.AllowedCIDRs, &resp.Diagnostics)
		updateOpts.AllowedCIDRs = &v
	}
	if !plan.InsertHeaders.Equal(state.InsertHeaders) {
		v := mapToStrings(ctx, plan.InsertHeaders, &resp.Diagnostics)
		updateOpts.InsertHeaders = &v
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

	if err := waitForLoadBalancerActive(ctx, client, lbID, defaultLBTimeout); err != nil {
		resp.Diagnostics.AddError("loadbalancer: waiting before listener update", err.Error())
		return
	}
	if _, err := listeners.Update(ctx, client, plan.ID.ValueString(), updateOpts).Extract(); err != nil {
		resp.Diagnostics.AddError("loadbalancer: updating listener", err.Error())
		return
	}
	if err := waitForLoadBalancerActive(ctx, client, lbID, defaultLBTimeout); err != nil {
		resp.Diagnostics.AddError("loadbalancer: waiting after listener update", err.Error())
		return
	}

	_, readDiags := r.readInto(ctx, client, plan.ID.ValueString(), &plan)
	resp.Diagnostics.Append(readDiags...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *listenerResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state listenerModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := r.config.LoadBalancerV2Client()
	if err != nil {
		resp.Diagnostics.AddError("loadbalancer: building v2 client", err.Error())
		return
	}

	lbID := state.LoadbalancerID.ValueString()
	if err := waitForLoadBalancerActive(ctx, client, lbID, defaultLBTimeout); err != nil {
		resp.Diagnostics.AddError("loadbalancer: waiting before listener delete", err.Error())
		return
	}
	if err := listeners.Delete(ctx, client, state.ID.ValueString()).ExtractErr(); err != nil {
		if gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
			return
		}
		resp.Diagnostics.AddError("loadbalancer: deleting listener", err.Error())
		return
	}
	if err := waitForLoadBalancerActive(ctx, client, lbID, defaultLBTimeout); err != nil {
		resp.Diagnostics.AddError("loadbalancer: waiting after listener delete", err.Error())
	}
}

func (r *listenerResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, path.Root("id"), req, resp)
}

// readInto refreshes the server-managed listener attributes. The user-input
// collections (sni_container_refs, allowed_cidrs, insert_headers,
// default_tls_container_ref) are echo-only — Octavia's null/empty handling for
// them would otherwise churn the plan.
func (r *listenerResource) readInto(ctx context.Context, client *gophercloud.ServiceClient, id string, m *listenerModel) (notFound bool, diags diag.Diagnostics) {
	l, err := listeners.Get(ctx, client, id).Extract()
	if err != nil {
		if gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
			return true, diags
		}
		diags.AddError("loadbalancer: reading listener", err.Error())
		return false, diags
	}

	m.ID = types.StringValue(l.ID)
	// loadbalancer_id is ForceNew and not scalar in the result; keep the prior
	// value, and derive from the result only on import (no prior state).
	if m.LoadbalancerID.IsNull() || m.LoadbalancerID.IsUnknown() {
		if len(l.Loadbalancers) > 0 {
			m.LoadbalancerID = types.StringValue(l.Loadbalancers[0].ID)
		}
	}
	m.Protocol = types.StringValue(l.Protocol)
	m.ProtocolPort = types.Int64Value(int64(l.ProtocolPort))
	m.Name = types.StringValue(l.Name)
	m.Description = types.StringValue(l.Description)
	m.DefaultPoolID = types.StringValue(l.DefaultPoolID)
	m.ConnectionLimit = types.Int64Value(int64(l.ConnLimit))
	m.AdminStateUp = types.BoolValue(l.AdminStateUp)
	m.TimeoutClientData = types.Int64Value(int64(l.TimeoutClientData))
	m.TimeoutMemberConnect = types.Int64Value(int64(l.TimeoutMemberConnect))
	m.TimeoutMemberData = types.Int64Value(int64(l.TimeoutMemberData))
	m.TimeoutTCPInspect = types.Int64Value(int64(l.TimeoutTCPInspect))
	m.ProvisioningStatus = types.StringValue(l.ProvisioningStatus)
	m.OperatingStatus = types.StringValue(l.OperatingStatus)

	tagVals := l.Tags
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
