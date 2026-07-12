// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0
//
// Ported from terraform-provider-openstack v3.4.0
// (openstack/resource_openstack_lb_member_v2.go), adapted for the
// terraform-plugin-framework and PCD.

package loadbalancer

import (
	"context"
	"fmt"
	"net/http"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/loadbalancer/v2/pools"
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
	_ resource.Resource                = (*memberResource)(nil)
	_ resource.ResourceWithConfigure   = (*memberResource)(nil)
	_ resource.ResourceWithImportState = (*memberResource)(nil)
)

// NewMemberResource is the factory registered with the provider.
func NewMemberResource() resource.Resource {
	return &memberResource{}
}

type memberResource struct {
	config *clients.Config
}

type memberModel struct {
	ID                 types.String `tfsdk:"id"`
	PoolID             types.String `tfsdk:"pool_id"`
	Address            types.String `tfsdk:"address"`
	ProtocolPort       types.Int64  `tfsdk:"protocol_port"`
	Name               types.String `tfsdk:"name"`
	Weight             types.Int64  `tfsdk:"weight"`
	SubnetID           types.String `tfsdk:"subnet_id"`
	AdminStateUp       types.Bool   `tfsdk:"admin_state_up"`
	Backup             types.Bool   `tfsdk:"backup"`
	MonitorAddress     types.String `tfsdk:"monitor_address"`
	MonitorPort        types.Int64  `tfsdk:"monitor_port"`
	Tags               types.Set    `tfsdk:"tags"`
	ProvisioningStatus types.String `tfsdk:"provisioning_status"`
	OperatingStatus    types.String `tfsdk:"operating_status"`
	Region             types.String `tfsdk:"region"`
}

func (r *memberResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_lb_member"
}

func (r *memberResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	useState := []planmodifier.String{stringplanmodifier.UseStateForUnknown()}
	forceNew := []planmodifier.String{stringplanmodifier.RequiresReplace()}
	forceNewC := []planmodifier.String{stringplanmodifier.RequiresReplace(), stringplanmodifier.UseStateForUnknown()}
	resp.Schema = schema.Schema{
		MarkdownDescription: "Manages a member (backend) of an Octavia load balancer pool in PCD.",
		Attributes: map[string]schema.Attribute{
			"id":                  schema.StringAttribute{Computed: true, MarkdownDescription: "The member ID.", PlanModifiers: useState},
			"pool_id":             schema.StringAttribute{Required: true, MarkdownDescription: "The pool this member belongs to. Changing this forces a new resource.", PlanModifiers: forceNew},
			"address":             schema.StringAttribute{Required: true, MarkdownDescription: "The IP address of the backend member. Changing this forces a new resource.", PlanModifiers: forceNew},
			"protocol_port":       schema.Int64Attribute{Required: true, MarkdownDescription: "The port the backend member listens on. Changing this forces a new resource.", PlanModifiers: []planmodifier.Int64{int64planmodifier.RequiresReplace()}},
			"name":                schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "The name of the member.", PlanModifiers: useState},
			"weight":              schema.Int64Attribute{Optional: true, Computed: true, MarkdownDescription: "Relative share of traffic this member receives (0 disables).", PlanModifiers: []planmodifier.Int64{int64planmodifier.UseStateForUnknown()}},
			"subnet_id":           schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "The subnet the member address is on. Changing this forces a new resource.", PlanModifiers: forceNewC},
			"admin_state_up":      schema.BoolAttribute{Optional: true, Computed: true, Default: booldefault.StaticBool(true), MarkdownDescription: "The administrative state of the member."},
			"backup":              schema.BoolAttribute{Optional: true, Computed: true, MarkdownDescription: "Whether this is a backup member (only receives traffic when non-backup members are down)."},
			"monitor_address":     schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "Alternate IP address used for health monitoring.", PlanModifiers: useState},
			"monitor_port":        schema.Int64Attribute{Optional: true, Computed: true, MarkdownDescription: "Alternate port used for health monitoring.", PlanModifiers: []planmodifier.Int64{int64planmodifier.UseStateForUnknown()}},
			"tags":                schema.SetAttribute{Optional: true, Computed: true, ElementType: types.StringType, MarkdownDescription: "Tags applied to the member.", PlanModifiers: []planmodifier.Set{setplanmodifier.UseStateForUnknown()}},
			"provisioning_status": schema.StringAttribute{Computed: true, MarkdownDescription: "The provisioning status.", PlanModifiers: useState},
			"operating_status":    schema.StringAttribute{Computed: true, MarkdownDescription: "The operating status.", PlanModifiers: useState},
			"region":              schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "The region. Defaults to the provider's region.", PlanModifiers: useState},
		},
	}
}

func (r *memberResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.config = configureClient(req.ProviderData, &resp.Diagnostics)
}

func (r *memberResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan memberModel
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
		resp.Diagnostics.AddError("loadbalancer: waiting before member create", err.Error())
		return
	}

	adminUp := plan.AdminStateUp.ValueBool()
	createOpts := pools.CreateMemberOpts{
		Address:        plan.Address.ValueString(),
		ProtocolPort:   int(plan.ProtocolPort.ValueInt64()),
		Name:           plan.Name.ValueString(),
		SubnetID:       plan.SubnetID.ValueString(),
		MonitorAddress: plan.MonitorAddress.ValueString(),
		AdminStateUp:   &adminUp,
		Weight:         intPtrIfSet(plan.Weight),
		MonitorPort:    intPtrIfSet(plan.MonitorPort),
		Backup:         boolPtrIfSet(plan.Backup),
	}
	if !plan.Tags.IsNull() && !plan.Tags.IsUnknown() {
		resp.Diagnostics.Append(plan.Tags.ElementsAs(ctx, &createOpts.Tags, false)...)
		if resp.Diagnostics.HasError() {
			return
		}
	}

	member, err := pools.CreateMember(ctx, client, poolID, createOpts).Extract()
	if err != nil {
		resp.Diagnostics.AddError("loadbalancer: creating member", err.Error())
		return
	}
	if err := waitForLoadBalancerActive(ctx, client, rootLB, defaultLBTimeout); err != nil {
		resp.Diagnostics.AddError("loadbalancer: waiting after member create", err.Error())
		return
	}

	_, readDiags := r.readInto(ctx, client, poolID, member.ID, &plan)
	resp.Diagnostics.Append(readDiags...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *memberResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state memberModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := r.config.LoadBalancerV2Client()
	if err != nil {
		resp.Diagnostics.AddError("loadbalancer: building v2 client", err.Error())
		return
	}

	notFound, diags := r.readInto(ctx, client, state.PoolID.ValueString(), state.ID.ValueString(), &state)
	if notFound {
		resp.Diagnostics.AddWarning("Member not found",
			fmt.Sprintf("Member %s no longer exists and was removed from state.", state.ID.ValueString()))
		resp.State.RemoveResource(ctx)
		return
	}
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *memberResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state memberModel
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

	updateOpts := pools.UpdateMemberOpts{}
	if !plan.Name.Equal(state.Name) {
		v := plan.Name.ValueString()
		updateOpts.Name = &v
	}
	if !plan.Weight.Equal(state.Weight) {
		updateOpts.Weight = intPtrIfSet(plan.Weight)
	}
	if !plan.AdminStateUp.Equal(state.AdminStateUp) {
		v := plan.AdminStateUp.ValueBool()
		updateOpts.AdminStateUp = &v
	}
	if !plan.Backup.Equal(state.Backup) {
		updateOpts.Backup = boolPtrIfSet(plan.Backup)
	}
	if !plan.MonitorAddress.Equal(state.MonitorAddress) {
		v := plan.MonitorAddress.ValueString()
		updateOpts.MonitorAddress = &v
	}
	if !plan.MonitorPort.Equal(state.MonitorPort) {
		updateOpts.MonitorPort = intPtrIfSet(plan.MonitorPort)
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
		resp.Diagnostics.AddError("loadbalancer: waiting before member update", err.Error())
		return
	}
	if _, err := pools.UpdateMember(ctx, client, poolID, plan.ID.ValueString(), updateOpts).Extract(); err != nil {
		resp.Diagnostics.AddError("loadbalancer: updating member", err.Error())
		return
	}
	if err := waitForLoadBalancerActive(ctx, client, rootLB, defaultLBTimeout); err != nil {
		resp.Diagnostics.AddError("loadbalancer: waiting after member update", err.Error())
		return
	}

	_, readDiags := r.readInto(ctx, client, poolID, plan.ID.ValueString(), &plan)
	resp.Diagnostics.Append(readDiags...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *memberResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state memberModel
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
		resp.Diagnostics.AddError("loadbalancer: waiting before member delete", err.Error())
		return
	}
	if err := pools.DeleteMember(ctx, client, poolID, state.ID.ValueString()).ExtractErr(); err != nil {
		if gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
			return
		}
		resp.Diagnostics.AddError("loadbalancer: deleting member", err.Error())
		return
	}
	if err := waitForLoadBalancerActive(ctx, client, rootLB, defaultLBTimeout); err != nil {
		resp.Diagnostics.AddError("loadbalancer: waiting after member delete", err.Error())
	}
}

func (r *memberResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	poolID, memberID, err := splitParentChildID(req.ID)
	if err != nil {
		resp.Diagnostics.AddError("Invalid import ID", err.Error())
		return
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), memberID)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("pool_id"), poolID)...)
}

func (r *memberResource) readInto(ctx context.Context, client *gophercloud.ServiceClient, poolID, memberID string, m *memberModel) (notFound bool, diags diag.Diagnostics) {
	member, err := pools.GetMember(ctx, client, poolID, memberID).Extract()
	if err != nil {
		if gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
			return true, diags
		}
		diags.AddError("loadbalancer: reading member", err.Error())
		return false, diags
	}

	m.ID = types.StringValue(member.ID)
	m.PoolID = types.StringValue(poolID)
	m.Address = types.StringValue(member.Address)
	m.ProtocolPort = types.Int64Value(int64(member.ProtocolPort))
	m.Name = types.StringValue(member.Name)
	m.Weight = types.Int64Value(int64(member.Weight))
	m.SubnetID = types.StringValue(member.SubnetID)
	m.AdminStateUp = types.BoolValue(member.AdminStateUp)
	m.Backup = types.BoolValue(member.Backup)
	m.MonitorAddress = types.StringValue(member.MonitorAddress)
	m.MonitorPort = types.Int64Value(int64(member.MonitorPort))
	m.ProvisioningStatus = types.StringValue(member.ProvisioningStatus)
	m.OperatingStatus = types.StringValue(member.OperatingStatus)

	tagVals := member.Tags
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
