// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0
//
// Ported from terraform-provider-openstack v3.4.0
// (openstack/resource_openstack_networking_floatingip_associate_v2.go), adapted
// for the terraform-plugin-framework and PCD.

package networking

import (
	"context"
	"fmt"
	"net/http"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/extensions/layer3/floatingips"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/platform9/terraform-provider-pcd/internal/clients"
)

var (
	_ resource.Resource                = (*floatingIPAssociateResource)(nil)
	_ resource.ResourceWithConfigure   = (*floatingIPAssociateResource)(nil)
	_ resource.ResourceWithImportState = (*floatingIPAssociateResource)(nil)
)

// NewFloatingIPAssociateResource is the factory registered with the provider.
func NewFloatingIPAssociateResource() resource.Resource {
	return &floatingIPAssociateResource{}
}

type floatingIPAssociateResource struct {
	config *clients.Config
}

type floatingIPAssociateModel struct {
	ID           types.String `tfsdk:"id"`
	FloatingIPID types.String `tfsdk:"floating_ip_id"`
	PortID       types.String `tfsdk:"port_id"`
	FixedIP      types.String `tfsdk:"fixed_ip"`
	Region       types.String `tfsdk:"region"`
}

func (r *floatingIPAssociateResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_networking_floatingip_associate"
}

func (r *floatingIPAssociateResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	useState := []planmodifier.String{stringplanmodifier.UseStateForUnknown()}
	resp.Schema = schema.Schema{
		MarkdownDescription: "Associates an existing floating IP with a port. Use this when the floating IP and the " +
			"port are managed separately (e.g. the floating IP is pre-allocated); to allocate and associate in one " +
			"step, use `pcd_networking_floatingip` with `port_id` instead. Deleting this resource disassociates the " +
			"floating IP but does not delete it.",
		Attributes: map[string]schema.Attribute{
			"id":             schema.StringAttribute{Computed: true, MarkdownDescription: "The floating IP ID (same as `floating_ip_id`).", PlanModifiers: useState},
			"floating_ip_id": schema.StringAttribute{Required: true, MarkdownDescription: "The ID of the floating IP to associate. Changing this forces a new resource.", PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()}},
			"port_id":        schema.StringAttribute{Required: true, MarkdownDescription: "The ID of the port to associate the floating IP with."},
			"fixed_ip":       schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "The specific fixed IP on the port to map to. Defaults to the port's first address.", PlanModifiers: useState},
			"region":         schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "The region. Defaults to the provider's region.", PlanModifiers: useState},
		},
	}
}

func (r *floatingIPAssociateResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.config = configureClient(req.ProviderData, &resp.Diagnostics)
}

func (r *floatingIPAssociateResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan floatingIPAssociateModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := r.config.NetworkV2Client()
	if err != nil {
		resp.Diagnostics.AddError("networking: building v2 client", err.Error())
		return
	}

	fipID := plan.FloatingIPID.ValueString()
	portID := plan.PortID.ValueString()
	updateOpts := floatingips.UpdateOpts{PortID: &portID}
	if fx := plan.FixedIP.ValueString(); fx != "" {
		updateOpts.FixedIP = fx
	}
	if _, err := floatingips.Update(ctx, client, fipID, updateOpts).Extract(); err != nil {
		resp.Diagnostics.AddError("networking: associating floating IP", err.Error())
		return
	}

	notFound, diags := r.readInto(ctx, client, fipID, &plan)
	resp.Diagnostics.Append(diags...)
	if notFound {
		resp.Diagnostics.AddError("networking: floating IP not found", fmt.Sprintf("Floating IP %s disappeared during association.", fipID))
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *floatingIPAssociateResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state floatingIPAssociateModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := r.config.NetworkV2Client()
	if err != nil {
		resp.Diagnostics.AddError("networking: building v2 client", err.Error())
		return
	}

	notFound, diags := r.readInto(ctx, client, state.FloatingIPID.ValueString(), &state)
	if notFound {
		resp.Diagnostics.AddWarning("Floating IP association gone",
			fmt.Sprintf("Floating IP %s no longer exists or is not associated with a port; removed from state.", state.FloatingIPID.ValueString()))
		resp.State.RemoveResource(ctx)
		return
	}
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *floatingIPAssociateResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan floatingIPAssociateModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := r.config.NetworkV2Client()
	if err != nil {
		resp.Diagnostics.AddError("networking: building v2 client", err.Error())
		return
	}

	fipID := plan.FloatingIPID.ValueString()
	portID := plan.PortID.ValueString()
	updateOpts := floatingips.UpdateOpts{PortID: &portID}
	if fx := plan.FixedIP.ValueString(); fx != "" {
		updateOpts.FixedIP = fx
	}
	if _, err := floatingips.Update(ctx, client, fipID, updateOpts).Extract(); err != nil {
		resp.Diagnostics.AddError("networking: updating floating IP association", err.Error())
		return
	}

	notFound, diags := r.readInto(ctx, client, fipID, &plan)
	resp.Diagnostics.Append(diags...)
	if notFound {
		resp.Diagnostics.AddError("networking: floating IP not found", fmt.Sprintf("Floating IP %s disappeared during update.", fipID))
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *floatingIPAssociateResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state floatingIPAssociateModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := r.config.NetworkV2Client()
	if err != nil {
		resp.Diagnostics.AddError("networking: building v2 client", err.Error())
		return
	}

	// Disassociate by clearing the port (pointer to empty string).
	empty := ""
	if _, err := floatingips.Update(ctx, client, state.FloatingIPID.ValueString(), floatingips.UpdateOpts{PortID: &empty}).Extract(); err != nil {
		if gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
			return
		}
		resp.Diagnostics.AddError("networking: disassociating floating IP", err.Error())
	}
}

func (r *floatingIPAssociateResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), req.ID)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("floating_ip_id"), req.ID)...)
}

func (r *floatingIPAssociateResource) readInto(ctx context.Context, client *gophercloud.ServiceClient, id string, m *floatingIPAssociateModel) (notFound bool, diags diag.Diagnostics) {
	fip, err := floatingips.Get(ctx, client, id).Extract()
	if err != nil {
		if gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
			return true, diags
		}
		diags.AddError("networking: reading floating IP", err.Error())
		return false, diags
	}
	// A cleared port means the association no longer exists.
	if fip.PortID == "" {
		return true, diags
	}

	m.ID = types.StringValue(fip.ID)
	m.FloatingIPID = types.StringValue(fip.ID)
	m.PortID = types.StringValue(fip.PortID)
	m.FixedIP = types.StringValue(fip.FixedIP)
	if m.Region.IsNull() || m.Region.IsUnknown() {
		m.Region = types.StringValue(r.config.Region)
	}
	return false, diags
}
