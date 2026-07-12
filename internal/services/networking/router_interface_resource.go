// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0
//
// Ported from terraform-provider-openstack v3.4.0
// (openstack/resource_openstack_networking_router_interface_v2.go), adapted for
// the terraform-plugin-framework and PCD.

package networking

import (
	"context"
	"fmt"
	"net/http"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/extensions/layer3/routers"
	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/ports"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/platform9/terraform-provider-pcd/internal/clients"
)

var (
	_ resource.Resource                = (*routerInterfaceResource)(nil)
	_ resource.ResourceWithConfigure   = (*routerInterfaceResource)(nil)
	_ resource.ResourceWithImportState = (*routerInterfaceResource)(nil)
)

// NewRouterInterfaceResource is the factory registered with the provider.
func NewRouterInterfaceResource() resource.Resource {
	return &routerInterfaceResource{}
}

type routerInterfaceResource struct {
	config *clients.Config
}

type routerInterfaceModel struct {
	ID       types.String `tfsdk:"id"`
	RouterID types.String `tfsdk:"router_id"`
	SubnetID types.String `tfsdk:"subnet_id"`
	PortID   types.String `tfsdk:"port_id"`
	Region   types.String `tfsdk:"region"`
}

func (r *routerInterfaceResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_networking_router_interface"
}

func (r *routerInterfaceResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	fn := []planmodifier.String{stringplanmodifier.RequiresReplace()}
	fnC := []planmodifier.String{stringplanmodifier.RequiresReplace(), stringplanmodifier.UseStateForUnknown()}
	resp.Schema = schema.Schema{
		MarkdownDescription: "Attaches a subnet (or existing port) to a Neutron router. Immutable: any change forces a new resource.",
		Attributes: map[string]schema.Attribute{
			"id":        schema.StringAttribute{Computed: true, MarkdownDescription: "The interface port ID.", PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()}},
			"router_id": schema.StringAttribute{Required: true, MarkdownDescription: "The router to attach to.", PlanModifiers: fn},
			"subnet_id": schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "The subnet to attach (mutually exclusive with port_id).", PlanModifiers: fnC},
			"port_id":   schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "An existing port to attach (mutually exclusive with subnet_id).", PlanModifiers: fnC},
			"region":    schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "The region. Defaults to the provider's region.", PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()}},
		},
	}
}

func (r *routerInterfaceResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.config = configureClient(req.ProviderData, &resp.Diagnostics)
}

func (r *routerInterfaceResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan routerInterfaceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	subnetID := plan.SubnetID.ValueString()
	portID := plan.PortID.ValueString()
	if (subnetID == "") == (portID == "") {
		resp.Diagnostics.AddError("Invalid router interface", "Exactly one of subnet_id or port_id must be set.")
		return
	}

	client, err := r.config.NetworkV2Client()
	if err != nil {
		resp.Diagnostics.AddError("networking: building v2 client", err.Error())
		return
	}

	info, err := routers.AddInterface(ctx, client, plan.RouterID.ValueString(), routers.AddInterfaceOpts{
		SubnetID: subnetID,
		PortID:   portID,
	}).Extract()
	if err != nil {
		resp.Diagnostics.AddError("networking: adding router interface", err.Error())
		return
	}

	plan.ID = types.StringValue(info.PortID)
	if r.readInto(ctx, client, info.PortID, &plan) {
		resp.Diagnostics.AddError("networking: reading router interface", "interface port disappeared immediately after creation")
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *routerInterfaceResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state routerInterfaceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := r.config.NetworkV2Client()
	if err != nil {
		resp.Diagnostics.AddError("networking: building v2 client", err.Error())
		return
	}

	if r.readInto(ctx, client, state.ID.ValueString(), &state) {
		resp.Diagnostics.AddWarning("Router interface not found",
			fmt.Sprintf("Router interface %s no longer exists and was removed from state.", state.ID.ValueString()))
		resp.State.RemoveResource(ctx)
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

// Update is required by the interface but never invoked (every attribute forces replacement).
func (r *routerInterfaceResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan routerInterfaceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *routerInterfaceResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state routerInterfaceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := r.config.NetworkV2Client()
	if err != nil {
		resp.Diagnostics.AddError("networking: building v2 client", err.Error())
		return
	}

	removeOpts := routers.RemoveInterfaceOpts{SubnetID: state.SubnetID.ValueString(), PortID: state.PortID.ValueString()}
	if _, err := routers.RemoveInterface(ctx, client, state.RouterID.ValueString(), removeOpts).Extract(); err != nil {
		if gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
			return
		}
		resp.Diagnostics.AddError("networking: removing router interface", err.Error())
	}
}

func (r *routerInterfaceResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, path.Root("id"), req, resp)
}

// readInto loads the interface port and derives router_id/subnet_id from it.
// Returns true when the port no longer exists.
func (r *routerInterfaceResource) readInto(ctx context.Context, client *gophercloud.ServiceClient, portID string, m *routerInterfaceModel) (notFound bool) {
	port, err := ports.Get(ctx, client, portID).Extract()
	if err != nil {
		return gophercloud.ResponseCodeIs(err, http.StatusNotFound)
	}

	m.ID = types.StringValue(port.ID)
	m.PortID = types.StringValue(port.ID)
	m.RouterID = types.StringValue(port.DeviceID)
	if len(port.FixedIPs) > 0 {
		m.SubnetID = types.StringValue(port.FixedIPs[0].SubnetID)
	}
	if m.Region.IsNull() || m.Region.IsUnknown() {
		m.Region = types.StringValue(r.config.Region)
	}
	return false
}
