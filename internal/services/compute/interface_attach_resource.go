// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0
//
// Ported from terraform-provider-openstack v3.4.0
// (openstack/resource_openstack_compute_interface_attach_v2.go), adapted for the
// terraform-plugin-framework and PCD.

package compute

import (
	"context"
	"fmt"
	"net/http"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/attachinterfaces"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/platform9/terraform-provider-pcd/internal/clients"
)

var (
	_ resource.Resource                = (*interfaceAttachResource)(nil)
	_ resource.ResourceWithConfigure   = (*interfaceAttachResource)(nil)
	_ resource.ResourceWithImportState = (*interfaceAttachResource)(nil)
)

// NewInterfaceAttachResource is the factory registered with the provider.
func NewInterfaceAttachResource() resource.Resource {
	return &interfaceAttachResource{}
}

type interfaceAttachResource struct {
	config *clients.Config
}

type interfaceAttachModel struct {
	ID         types.String `tfsdk:"id"`
	InstanceID types.String `tfsdk:"instance_id"`
	PortID     types.String `tfsdk:"port_id"`
	NetworkID  types.String `tfsdk:"network_id"`
	FixedIP    types.String `tfsdk:"fixed_ip"`
	MAC        types.String `tfsdk:"mac"`
	PortState  types.String `tfsdk:"port_state"`
	Region     types.String `tfsdk:"region"`
}

func (r *interfaceAttachResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_compute_interface_attach"
}

func (r *interfaceAttachResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	forceNew := []planmodifier.String{stringplanmodifier.RequiresReplace()}
	forceNewC := []planmodifier.String{stringplanmodifier.RequiresReplace(), stringplanmodifier.UseStateForUnknown()}
	stable := []planmodifier.String{stringplanmodifier.UseStateForUnknown()}
	resp.Schema = schema.Schema{
		MarkdownDescription: "Attaches a network interface to a compute instance, either by an existing `port_id` " +
			"or by allocating a new port on `network_id`. Exactly one of `port_id`/`network_id` must be set. " +
			"All attributes force replacement (attach/detach has no in-place update).",
		Attributes: map[string]schema.Attribute{
			"id":          schema.StringAttribute{Computed: true, MarkdownDescription: "Composite ID: `instance_id/port_id`.", PlanModifiers: stable},
			"instance_id": schema.StringAttribute{Required: true, MarkdownDescription: "The instance (server) ID to attach to. Changing this forces a new resource.", PlanModifiers: forceNew},
			"port_id":     schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "An existing port to attach (mutually exclusive with network_id). Changing this forces a new resource.", PlanModifiers: forceNewC},
			"network_id":  schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "A network to allocate a new port on (mutually exclusive with port_id). Changing this forces a new resource.", PlanModifiers: forceNewC},
			"fixed_ip":    schema.StringAttribute{Optional: true, MarkdownDescription: "A specific fixed IP to request (only valid with network_id). Changing this forces a new resource.", PlanModifiers: forceNew},
			"mac":         schema.StringAttribute{Computed: true, MarkdownDescription: "The MAC address of the attached interface.", PlanModifiers: stable},
			"port_state":  schema.StringAttribute{Computed: true, MarkdownDescription: "The port state (e.g. ACTIVE).", PlanModifiers: stable},
			"region":      schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "The region. Defaults to the provider's region.", PlanModifiers: stable},
		},
	}
}

func (r *interfaceAttachResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.config = configureClient(req.ProviderData, &resp.Diagnostics)
}

func (r *interfaceAttachResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan interfaceAttachModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	portSet := !plan.PortID.IsNull() && !plan.PortID.IsUnknown() && plan.PortID.ValueString() != ""
	netSet := !plan.NetworkID.IsNull() && !plan.NetworkID.IsUnknown() && plan.NetworkID.ValueString() != ""
	fixedIP := plan.FixedIP.ValueString()
	switch {
	case portSet && netSet:
		resp.Diagnostics.AddError("Invalid interface", "Set only one of port_id or network_id.")
		return
	case !portSet && !netSet:
		resp.Diagnostics.AddError("Invalid interface", "Exactly one of port_id or network_id must be set.")
		return
	case fixedIP != "" && !netSet:
		resp.Diagnostics.AddError("Invalid interface", "fixed_ip may only be set together with network_id.")
		return
	}

	client, err := r.config.ComputeV2Client()
	if err != nil {
		resp.Diagnostics.AddError("compute: building v2 client", err.Error())
		return
	}

	createOpts := attachinterfaces.CreateOpts{
		PortID:    plan.PortID.ValueString(),
		NetworkID: plan.NetworkID.ValueString(),
	}
	if fixedIP != "" {
		createOpts.FixedIPs = []attachinterfaces.FixedIP{{IPAddress: fixedIP}}
	}

	iface, err := attachinterfaces.Create(ctx, client, plan.InstanceID.ValueString(), createOpts).Extract()
	if err != nil {
		resp.Diagnostics.AddError("compute: attaching interface", err.Error())
		return
	}

	plan.ID = types.StringValue(plan.InstanceID.ValueString() + "/" + iface.PortID)
	r.flatten(iface, &plan)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *interfaceAttachResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state interfaceAttachModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := r.config.ComputeV2Client()
	if err != nil {
		resp.Diagnostics.AddError("compute: building v2 client", err.Error())
		return
	}

	iface, err := attachinterfaces.Get(ctx, client, state.InstanceID.ValueString(), state.PortID.ValueString()).Extract()
	if err != nil {
		if gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
			resp.Diagnostics.AddWarning("Interface attachment not found",
				fmt.Sprintf("Interface %s on instance %s is gone; removed from state.", state.PortID.ValueString(), state.InstanceID.ValueString()))
			resp.State.RemoveResource(ctx)
			return
		}
		resp.Diagnostics.AddError("compute: reading interface attachment", err.Error())
		return
	}

	r.flatten(iface, &state)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

// Update is required by the interface but never invoked (every attribute forces replacement).
func (r *interfaceAttachResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan interfaceAttachModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *interfaceAttachResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state interfaceAttachModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := r.config.ComputeV2Client()
	if err != nil {
		resp.Diagnostics.AddError("compute: building v2 client", err.Error())
		return
	}

	if err := attachinterfaces.Delete(ctx, client, state.InstanceID.ValueString(), state.PortID.ValueString()).ExtractErr(); err != nil {
		if gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
			return
		}
		resp.Diagnostics.AddError("compute: detaching interface", err.Error())
	}
}

func (r *interfaceAttachResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	instanceID, portID, err := splitInstanceScopedID(req.ID)
	if err != nil {
		resp.Diagnostics.AddError("Invalid import ID", err.Error())
		return
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), req.ID)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("instance_id"), instanceID)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("port_id"), portID)...)
}

// flatten copies server-known fields. fixed_ip is echo-only: Nova may assign
// several fixed IPs while the config requests at most one, so refreshing it would
// churn the plan.
func (r *interfaceAttachResource) flatten(iface *attachinterfaces.Interface, m *interfaceAttachModel) {
	m.PortID = types.StringValue(iface.PortID)
	m.NetworkID = types.StringValue(iface.NetID)
	m.MAC = types.StringValue(iface.MACAddr)
	m.PortState = types.StringValue(iface.PortState)
	if m.Region.IsNull() || m.Region.IsUnknown() {
		m.Region = types.StringValue(r.config.Region)
	}
}
