// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0
//
// Ported from terraform-provider-openstack v3.4.0
// (openstack/resource_openstack_lb_loadbalancer_v2.go), adapted for the
// terraform-plugin-framework and PCD.

package loadbalancer

import (
	"context"
	"fmt"
	"net/http"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/loadbalancer/v2/loadbalancers"
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
	_ resource.Resource                = (*loadBalancerResource)(nil)
	_ resource.ResourceWithConfigure   = (*loadBalancerResource)(nil)
	_ resource.ResourceWithImportState = (*loadBalancerResource)(nil)
)

// NewLoadBalancerResource is the factory registered with the provider.
func NewLoadBalancerResource() resource.Resource {
	return &loadBalancerResource{}
}

type loadBalancerResource struct {
	config *clients.Config
}

type loadBalancerModel struct {
	ID                 types.String `tfsdk:"id"`
	Name               types.String `tfsdk:"name"`
	Description        types.String `tfsdk:"description"`
	AdminStateUp       types.Bool   `tfsdk:"admin_state_up"`
	VipSubnetID        types.String `tfsdk:"vip_subnet_id"`
	VipNetworkID       types.String `tfsdk:"vip_network_id"`
	VipAddress         types.String `tfsdk:"vip_address"`
	VipPortID          types.String `tfsdk:"vip_port_id"`
	FlavorID           types.String `tfsdk:"flavor_id"`
	Tags               types.Set    `tfsdk:"tags"`
	ProvisioningStatus types.String `tfsdk:"provisioning_status"`
	OperatingStatus    types.String `tfsdk:"operating_status"`
	Region             types.String `tfsdk:"region"`
}

func (r *loadBalancerResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_lb_loadbalancer"
}

func (r *loadBalancerResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	useState := []planmodifier.String{stringplanmodifier.UseStateForUnknown()}
	forceNew := []planmodifier.String{stringplanmodifier.RequiresReplace(), stringplanmodifier.UseStateForUnknown()}
	resp.Schema = schema.Schema{
		MarkdownDescription: "Manages an Octavia load balancer in PCD. Exactly one of `vip_subnet_id` or " +
			"`vip_network_id` must be set. Octavia serializes changes per load balancer, so applies wait for the " +
			"load balancer to return to an `ACTIVE` provisioning status. PCD uses the OVN provider, which is " +
			"Layer 4 (TCP/UDP/SCTP): use L4 listener protocols and OVN-supported pool algorithms; HTTP/L7 features " +
			"are not available.",
		Attributes: map[string]schema.Attribute{
			"id":                  schema.StringAttribute{Computed: true, MarkdownDescription: "The load balancer ID.", PlanModifiers: useState},
			"name":                schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "The name of the load balancer.", PlanModifiers: useState},
			"description":         schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "A description of the load balancer.", PlanModifiers: useState},
			"admin_state_up":      schema.BoolAttribute{Optional: true, Computed: true, Default: booldefault.StaticBool(true), MarkdownDescription: "The administrative state of the load balancer."},
			"vip_subnet_id":       schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "Subnet on which to allocate the VIP (mutually exclusive with vip_network_id). Changing this forces a new resource.", PlanModifiers: forceNew},
			"vip_network_id":      schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "Network on which to allocate the VIP (mutually exclusive with vip_subnet_id). Changing this forces a new resource.", PlanModifiers: forceNew},
			"vip_address":         schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "The VIP address. Requesting a specific address changing it forces a new resource.", PlanModifiers: forceNew},
			"vip_port_id":         schema.StringAttribute{Computed: true, MarkdownDescription: "The ID of the VIP port.", PlanModifiers: useState},
			"flavor_id":           schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "The Octavia flavor to use. Changing this forces a new resource.", PlanModifiers: forceNew},
			"tags":                schema.SetAttribute{Optional: true, Computed: true, ElementType: types.StringType, MarkdownDescription: "Tags applied to the load balancer.", PlanModifiers: []planmodifier.Set{setplanmodifier.UseStateForUnknown()}},
			"provisioning_status": schema.StringAttribute{Computed: true, MarkdownDescription: "The provisioning status (e.g. ACTIVE).", PlanModifiers: useState},
			"operating_status":    schema.StringAttribute{Computed: true, MarkdownDescription: "The operating status (e.g. ONLINE).", PlanModifiers: useState},
			"region":              schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "The region. Defaults to the provider's region.", PlanModifiers: useState},
		},
	}
}

func (r *loadBalancerResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.config = configureClient(req.ProviderData, &resp.Diagnostics)
}

func (r *loadBalancerResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan loadBalancerModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	subnetSet := isSet(plan.VipSubnetID)
	networkSet := isSet(plan.VipNetworkID)
	if subnetSet == networkSet {
		resp.Diagnostics.AddError("Invalid VIP", "Exactly one of vip_subnet_id or vip_network_id must be set.")
		return
	}

	client, err := r.config.LoadBalancerV2Client()
	if err != nil {
		resp.Diagnostics.AddError("loadbalancer: building v2 client", err.Error())
		return
	}

	adminUp := plan.AdminStateUp.ValueBool()
	createOpts := loadbalancers.CreateOpts{
		Name:         plan.Name.ValueString(),
		Description:  plan.Description.ValueString(),
		VipSubnetID:  plan.VipSubnetID.ValueString(),
		VipNetworkID: plan.VipNetworkID.ValueString(),
		VipAddress:   plan.VipAddress.ValueString(),
		FlavorID:     plan.FlavorID.ValueString(),
		AdminStateUp: &adminUp,
	}
	if !plan.Tags.IsNull() && !plan.Tags.IsUnknown() {
		resp.Diagnostics.Append(plan.Tags.ElementsAs(ctx, &createOpts.Tags, false)...)
		if resp.Diagnostics.HasError() {
			return
		}
	}

	lb, err := loadbalancers.Create(ctx, client, createOpts).Extract()
	if err != nil {
		resp.Diagnostics.AddError("loadbalancer: creating load balancer", err.Error())
		return
	}

	if err := waitForLoadBalancerActive(ctx, client, lb.ID, defaultLBTimeout); err != nil {
		resp.Diagnostics.AddError("loadbalancer: waiting for load balancer to become active", err.Error())
		return
	}

	_, readDiags := r.readInto(ctx, client, lb.ID, &plan)
	resp.Diagnostics.Append(readDiags...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *loadBalancerResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state loadBalancerModel
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
		resp.Diagnostics.AddWarning("Load balancer not found",
			fmt.Sprintf("Load balancer %s no longer exists and was removed from state.", state.ID.ValueString()))
		resp.State.RemoveResource(ctx)
		return
	}
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *loadBalancerResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state loadBalancerModel
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

	id := plan.ID.ValueString()
	updateOpts := loadbalancers.UpdateOpts{}
	changed := false
	if !plan.Name.Equal(state.Name) {
		v := plan.Name.ValueString()
		updateOpts.Name = &v
		changed = true
	}
	if !plan.Description.Equal(state.Description) {
		v := plan.Description.ValueString()
		updateOpts.Description = &v
		changed = true
	}
	if !plan.AdminStateUp.Equal(state.AdminStateUp) {
		v := plan.AdminStateUp.ValueBool()
		updateOpts.AdminStateUp = &v
		changed = true
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
		changed = true
	}

	if changed {
		if err := waitForLoadBalancerActive(ctx, client, id, defaultLBTimeout); err != nil {
			resp.Diagnostics.AddError("loadbalancer: waiting before update", err.Error())
			return
		}
		if _, err := loadbalancers.Update(ctx, client, id, updateOpts).Extract(); err != nil {
			resp.Diagnostics.AddError("loadbalancer: updating load balancer", err.Error())
			return
		}
		if err := waitForLoadBalancerActive(ctx, client, id, defaultLBTimeout); err != nil {
			resp.Diagnostics.AddError("loadbalancer: waiting after update", err.Error())
			return
		}
	}

	_, readDiags := r.readInto(ctx, client, id, &plan)
	resp.Diagnostics.Append(readDiags...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *loadBalancerResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state loadBalancerModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := r.config.LoadBalancerV2Client()
	if err != nil {
		resp.Diagnostics.AddError("loadbalancer: building v2 client", err.Error())
		return
	}

	id := state.ID.ValueString()
	if err := loadbalancers.Delete(ctx, client, id, loadbalancers.DeleteOpts{Cascade: true}).ExtractErr(); err != nil {
		if gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
			return
		}
		resp.Diagnostics.AddError("loadbalancer: deleting load balancer", err.Error())
		return
	}
	if err := waitForLoadBalancerDeleted(ctx, client, id, defaultLBTimeout); err != nil {
		resp.Diagnostics.AddError("loadbalancer: waiting for load balancer deletion", err.Error())
	}
}

func (r *loadBalancerResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, path.Root("id"), req, resp)
}

func (r *loadBalancerResource) readInto(ctx context.Context, client *gophercloud.ServiceClient, id string, m *loadBalancerModel) (notFound bool, diags diag.Diagnostics) {
	lb, err := loadbalancers.Get(ctx, client, id).Extract()
	if err != nil {
		if gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
			return true, diags
		}
		diags.AddError("loadbalancer: reading load balancer", err.Error())
		return false, diags
	}

	m.ID = types.StringValue(lb.ID)
	m.Name = types.StringValue(lb.Name)
	m.Description = types.StringValue(lb.Description)
	m.AdminStateUp = types.BoolValue(lb.AdminStateUp)
	m.VipSubnetID = types.StringValue(lb.VipSubnetID)
	m.VipNetworkID = types.StringValue(lb.VipNetworkID)
	m.VipAddress = types.StringValue(lb.VipAddress)
	m.VipPortID = types.StringValue(lb.VipPortID)
	m.FlavorID = types.StringValue(lb.FlavorID)
	m.ProvisioningStatus = types.StringValue(lb.ProvisioningStatus)
	m.OperatingStatus = types.StringValue(lb.OperatingStatus)

	tagVals := lb.Tags
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

// isSet reports whether a string attribute holds a non-empty, known value.
func isSet(v types.String) bool {
	return !v.IsNull() && !v.IsUnknown() && v.ValueString() != ""
}
