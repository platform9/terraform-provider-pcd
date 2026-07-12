// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0
//
// Ported from terraform-provider-openstack v3.4.0
// (openstack/resource_openstack_networking_router_route_v2.go), adapted for the
// terraform-plugin-framework and PCD.

package networking

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/extensions/layer3/routers"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/platform9/terraform-provider-pcd/internal/clients"
)

var (
	_ resource.Resource                = (*routerRouteResource)(nil)
	_ resource.ResourceWithConfigure   = (*routerRouteResource)(nil)
	_ resource.ResourceWithImportState = (*routerRouteResource)(nil)
)

// NewRouterRouteResource is the factory registered with the provider.
func NewRouterRouteResource() resource.Resource {
	return &routerRouteResource{}
}

type routerRouteResource struct {
	config *clients.Config
}

type routerRouteModel struct {
	ID              types.String `tfsdk:"id"`
	RouterID        types.String `tfsdk:"router_id"`
	DestinationCIDR types.String `tfsdk:"destination_cidr"`
	NextHop         types.String `tfsdk:"next_hop"`
	Region          types.String `tfsdk:"region"`
}

func (r *routerRouteResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_networking_router_route"
}

func (r *routerRouteResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	forceNew := []planmodifier.String{stringplanmodifier.RequiresReplace()}
	resp.Schema = schema.Schema{
		MarkdownDescription: "Manages a single static route on a Neutron router without disturbing routes managed " +
			"elsewhere. Every attribute forces a new resource. Multiple router_route resources on the same router are " +
			"applied serially so they do not clobber each other's routes.",
		Attributes: map[string]schema.Attribute{
			"id":               schema.StringAttribute{Computed: true, MarkdownDescription: "Composite ID: `router_id/destination_cidr/next_hop`.", PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()}},
			"router_id":        schema.StringAttribute{Required: true, MarkdownDescription: "The router to add the route to. Changing this forces a new resource.", PlanModifiers: forceNew},
			"destination_cidr": schema.StringAttribute{Required: true, MarkdownDescription: "The destination CIDR. Changing this forces a new resource.", PlanModifiers: forceNew},
			"next_hop":         schema.StringAttribute{Required: true, MarkdownDescription: "The next-hop IP address. Changing this forces a new resource.", PlanModifiers: forceNew},
			"region":           schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "The region. Defaults to the provider's region.", PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()}},
		},
	}
}

func (r *routerRouteResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.config = configureClient(req.ProviderData, &resp.Diagnostics)
}

func (r *routerRouteResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan routerRouteModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := r.config.NetworkV2Client()
	if err != nil {
		resp.Diagnostics.AddError("networking: building v2 client", err.Error())
		return
	}

	routerID := plan.RouterID.ValueString()
	dst := plan.DestinationCIDR.ValueString()
	hop := plan.NextHop.ValueString()

	neutronParentMu.Lock(routerID)
	defer neutronParentMu.Unlock(routerID)

	router, err := routers.Get(ctx, client, routerID).Extract()
	if err != nil {
		resp.Diagnostics.AddError("networking: reading router", err.Error())
		return
	}
	for _, rt := range router.Routes {
		if rt.DestinationCIDR == dst && rt.NextHop == hop {
			resp.Diagnostics.AddError("Route already exists",
				fmt.Sprintf("Router %s already has a route %s -> %s.", routerID, dst, hop))
			return
		}
	}

	newRoutes := append(append([]routers.Route(nil), router.Routes...), routers.Route{DestinationCIDR: dst, NextHop: hop})
	if _, err := routers.Update(ctx, client, routerID, routers.UpdateOpts{Routes: &newRoutes}).Extract(); err != nil {
		resp.Diagnostics.AddError("networking: adding router route", err.Error())
		return
	}

	plan.ID = types.StringValue(routerRouteID(routerID, dst, hop))
	if plan.Region.IsNull() || plan.Region.IsUnknown() {
		plan.Region = types.StringValue(r.config.Region)
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *routerRouteResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state routerRouteModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := r.config.NetworkV2Client()
	if err != nil {
		resp.Diagnostics.AddError("networking: building v2 client", err.Error())
		return
	}

	routerID := state.RouterID.ValueString()
	dst := state.DestinationCIDR.ValueString()
	hop := state.NextHop.ValueString()

	router, err := routers.Get(ctx, client, routerID).Extract()
	if err != nil {
		if gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
			resp.State.RemoveResource(ctx)
			return
		}
		resp.Diagnostics.AddError("networking: reading router", err.Error())
		return
	}

	found := false
	for _, rt := range router.Routes {
		if rt.DestinationCIDR == dst && rt.NextHop == hop {
			found = true
			break
		}
	}
	if !found {
		resp.Diagnostics.AddWarning("Router route not found",
			fmt.Sprintf("Route %s -> %s is no longer on router %s and was removed from state.", dst, hop, routerID))
		resp.State.RemoveResource(ctx)
		return
	}

	state.ID = types.StringValue(routerRouteID(routerID, dst, hop))
	if state.Region.IsNull() || state.Region.IsUnknown() {
		state.Region = types.StringValue(r.config.Region)
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

// Update is required by the interface but never invoked (every attribute forces replacement).
func (r *routerRouteResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan routerRouteModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *routerRouteResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state routerRouteModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := r.config.NetworkV2Client()
	if err != nil {
		resp.Diagnostics.AddError("networking: building v2 client", err.Error())
		return
	}

	routerID := state.RouterID.ValueString()
	dst := state.DestinationCIDR.ValueString()
	hop := state.NextHop.ValueString()

	neutronParentMu.Lock(routerID)
	defer neutronParentMu.Unlock(routerID)

	router, err := routers.Get(ctx, client, routerID).Extract()
	if err != nil {
		if gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
			return
		}
		resp.Diagnostics.AddError("networking: reading router", err.Error())
		return
	}

	kept := make([]routers.Route, 0, len(router.Routes))
	for _, rt := range router.Routes {
		if rt.DestinationCIDR == dst && rt.NextHop == hop {
			continue
		}
		kept = append(kept, rt)
	}
	if _, err := routers.Update(ctx, client, routerID, routers.UpdateOpts{Routes: &kept}).Extract(); err != nil {
		resp.Diagnostics.AddError("networking: removing router route", err.Error())
		return
	}
}

func (r *routerRouteResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	routerID, dst, hop, err := parseRouteID(req.ID)
	if err != nil {
		resp.Diagnostics.AddError("Invalid import ID", err.Error())
		return
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), routerRouteID(routerID, dst, hop))...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("router_id"), routerID)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("destination_cidr"), dst)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("next_hop"), hop)...)
}

func routerRouteID(routerID, dst, hop string) string {
	return fmt.Sprintf("%s/%s/%s", routerID, dst, hop)
}

// parseRouteID splits a composite route ID of the form
// "<parent_id>/<destination_cidr>/<next_hop>". The destination CIDR itself
// contains a slash, so the parent is the first segment, the next hop is the
// last, and everything between is the CIDR.
func parseRouteID(id string) (parent, dst, hop string, err error) {
	parts := strings.Split(id, "/")
	if len(parts) < 3 {
		return "", "", "", fmt.Errorf("expected <parent_id>/<destination_cidr>/<next_hop>, got %q", id)
	}
	parent = parts[0]
	hop = parts[len(parts)-1]
	dst = strings.Join(parts[1:len(parts)-1], "/")
	if parent == "" || dst == "" || hop == "" {
		return "", "", "", fmt.Errorf("empty segment in import ID %q", id)
	}
	return parent, dst, hop, nil
}
