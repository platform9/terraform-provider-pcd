// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0
//
// Ported from terraform-provider-openstack v3.4.0
// (openstack/resource_openstack_networking_subnet_route_v2.go), adapted for the
// terraform-plugin-framework and PCD.

package networking

import (
	"context"
	"fmt"
	"net/http"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/subnets"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/platform9/terraform-provider-pcd/internal/clients"
)

var (
	_ resource.Resource                = (*subnetRouteResource)(nil)
	_ resource.ResourceWithConfigure   = (*subnetRouteResource)(nil)
	_ resource.ResourceWithImportState = (*subnetRouteResource)(nil)
)

// NewSubnetRouteResource is the factory registered with the provider.
func NewSubnetRouteResource() resource.Resource {
	return &subnetRouteResource{}
}

type subnetRouteResource struct {
	config *clients.Config
}

type subnetRouteModel struct {
	ID              types.String `tfsdk:"id"`
	SubnetID        types.String `tfsdk:"subnet_id"`
	DestinationCIDR types.String `tfsdk:"destination_cidr"`
	NextHop         types.String `tfsdk:"next_hop"`
	Region          types.String `tfsdk:"region"`
}

func (r *subnetRouteResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_networking_subnet_route"
}

func (r *subnetRouteResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	forceNew := []planmodifier.String{stringplanmodifier.RequiresReplace()}
	resp.Schema = schema.Schema{
		MarkdownDescription: "Manages a single host route on a Neutron subnet (advertised to instances via DHCP) " +
			"without disturbing routes managed elsewhere. Every attribute forces a new resource. Multiple " +
			"subnet_route resources on the same subnet are applied serially so they do not clobber each other.",
		Attributes: map[string]schema.Attribute{
			"id":               schema.StringAttribute{Computed: true, MarkdownDescription: "Composite ID: `subnet_id/destination_cidr/next_hop`.", PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()}},
			"subnet_id":        schema.StringAttribute{Required: true, MarkdownDescription: "The subnet to add the host route to. Changing this forces a new resource.", PlanModifiers: forceNew},
			"destination_cidr": schema.StringAttribute{Required: true, MarkdownDescription: "The destination CIDR. Changing this forces a new resource.", PlanModifiers: forceNew},
			"next_hop":         schema.StringAttribute{Required: true, MarkdownDescription: "The next-hop IP address. Changing this forces a new resource.", PlanModifiers: forceNew},
			"region":           schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "The region. Defaults to the provider's region.", PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()}},
		},
	}
}

func (r *subnetRouteResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.config = configureClient(req.ProviderData, &resp.Diagnostics)
}

func (r *subnetRouteResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan subnetRouteModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := r.config.NetworkV2Client()
	if err != nil {
		resp.Diagnostics.AddError("networking: building v2 client", err.Error())
		return
	}

	subnetID := plan.SubnetID.ValueString()
	dst := plan.DestinationCIDR.ValueString()
	hop := plan.NextHop.ValueString()

	neutronParentMu.Lock(subnetID)
	defer neutronParentMu.Unlock(subnetID)

	subnet, err := subnets.Get(ctx, client, subnetID).Extract()
	if err != nil {
		resp.Diagnostics.AddError("networking: reading subnet", err.Error())
		return
	}
	for _, rt := range subnet.HostRoutes {
		if rt.DestinationCIDR == dst && rt.NextHop == hop {
			resp.Diagnostics.AddError("Host route already exists",
				fmt.Sprintf("Subnet %s already has a host route %s -> %s.", subnetID, dst, hop))
			return
		}
	}

	newRoutes := append(append([]subnets.HostRoute(nil), subnet.HostRoutes...), subnets.HostRoute{DestinationCIDR: dst, NextHop: hop})
	if _, err := subnets.Update(ctx, client, subnetID, subnets.UpdateOpts{HostRoutes: &newRoutes}).Extract(); err != nil {
		resp.Diagnostics.AddError("networking: adding subnet host route", err.Error())
		return
	}

	plan.ID = types.StringValue(routerRouteID(subnetID, dst, hop))
	if plan.Region.IsNull() || plan.Region.IsUnknown() {
		plan.Region = types.StringValue(r.config.Region)
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *subnetRouteResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state subnetRouteModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := r.config.NetworkV2Client()
	if err != nil {
		resp.Diagnostics.AddError("networking: building v2 client", err.Error())
		return
	}

	subnetID := state.SubnetID.ValueString()
	dst := state.DestinationCIDR.ValueString()
	hop := state.NextHop.ValueString()

	subnet, err := subnets.Get(ctx, client, subnetID).Extract()
	if err != nil {
		if gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
			resp.State.RemoveResource(ctx)
			return
		}
		resp.Diagnostics.AddError("networking: reading subnet", err.Error())
		return
	}

	found := false
	for _, rt := range subnet.HostRoutes {
		if rt.DestinationCIDR == dst && rt.NextHop == hop {
			found = true
			break
		}
	}
	if !found {
		resp.Diagnostics.AddWarning("Subnet host route not found",
			fmt.Sprintf("Host route %s -> %s is no longer on subnet %s and was removed from state.", dst, hop, subnetID))
		resp.State.RemoveResource(ctx)
		return
	}

	state.ID = types.StringValue(routerRouteID(subnetID, dst, hop))
	if state.Region.IsNull() || state.Region.IsUnknown() {
		state.Region = types.StringValue(r.config.Region)
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

// Update is required by the interface but never invoked (every attribute forces replacement).
func (r *subnetRouteResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan subnetRouteModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *subnetRouteResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state subnetRouteModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := r.config.NetworkV2Client()
	if err != nil {
		resp.Diagnostics.AddError("networking: building v2 client", err.Error())
		return
	}

	subnetID := state.SubnetID.ValueString()
	dst := state.DestinationCIDR.ValueString()
	hop := state.NextHop.ValueString()

	neutronParentMu.Lock(subnetID)
	defer neutronParentMu.Unlock(subnetID)

	subnet, err := subnets.Get(ctx, client, subnetID).Extract()
	if err != nil {
		if gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
			return
		}
		resp.Diagnostics.AddError("networking: reading subnet", err.Error())
		return
	}

	kept := make([]subnets.HostRoute, 0, len(subnet.HostRoutes))
	for _, rt := range subnet.HostRoutes {
		if rt.DestinationCIDR == dst && rt.NextHop == hop {
			continue
		}
		kept = append(kept, rt)
	}
	if _, err := subnets.Update(ctx, client, subnetID, subnets.UpdateOpts{HostRoutes: &kept}).Extract(); err != nil {
		resp.Diagnostics.AddError("networking: removing subnet host route", err.Error())
		return
	}
}

func (r *subnetRouteResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	subnetID, dst, hop, err := parseRouteID(req.ID)
	if err != nil {
		resp.Diagnostics.AddError("Invalid import ID", err.Error())
		return
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), routerRouteID(subnetID, dst, hop))...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("subnet_id"), subnetID)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("destination_cidr"), dst)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("next_hop"), hop)...)
}
