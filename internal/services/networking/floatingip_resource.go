// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0
//
// Ported from terraform-provider-openstack v3.4.0
// (openstack/resource_openstack_networking_floatingip_v2.go), adapted for the
// terraform-plugin-framework and PCD.

package networking

import (
	"context"
	"fmt"
	"net/http"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/extensions/external"
	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/extensions/layer3/floatingips"
	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/networks"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/setplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/platform9/terraform-provider-pcd/internal/clients"
)

var (
	_ resource.Resource                = (*floatingIPResource)(nil)
	_ resource.ResourceWithConfigure   = (*floatingIPResource)(nil)
	_ resource.ResourceWithImportState = (*floatingIPResource)(nil)
	_ resource.ResourceWithModifyPlan  = (*floatingIPResource)(nil)
)

// NewFloatingIPResource is the factory registered with the provider.
func NewFloatingIPResource() resource.Resource {
	return &floatingIPResource{}
}

type floatingIPResource struct {
	config *clients.Config
}

type floatingIPModel struct {
	ID                types.String `tfsdk:"id"`
	Pool              types.String `tfsdk:"pool"`
	FloatingNetworkID types.String `tfsdk:"floating_network_id"`
	Description       types.String `tfsdk:"description"`
	Address           types.String `tfsdk:"address"`
	PortID            types.String `tfsdk:"port_id"`
	FixedIP           types.String `tfsdk:"fixed_ip"`
	TenantID          types.String `tfsdk:"tenant_id"`
	Status            types.String `tfsdk:"status"`
	RouterID          types.String `tfsdk:"router_id"`
	Tags              types.Set    `tfsdk:"tags"`
	Region            types.String `tfsdk:"region"`
}

func (r *floatingIPResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_networking_floatingip"
}

func (r *floatingIPResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	useState := []planmodifier.String{stringplanmodifier.UseStateForUnknown()}
	forceNewStr := []planmodifier.String{stringplanmodifier.RequiresReplace(), stringplanmodifier.UseStateForUnknown()}
	resp.Schema = schema.Schema{
		MarkdownDescription: "Manages a floating IP allocated from an external network in PCD. Optionally associate " +
			"it with a port to expose an instance externally.",
		Attributes: map[string]schema.Attribute{
			"id":                  schema.StringAttribute{Computed: true, MarkdownDescription: "The floating IP ID.", PlanModifiers: useState},
			"pool":                schema.StringAttribute{Required: true, MarkdownDescription: "The name of the external network to allocate the floating IP from. Changing this forces a new resource.", PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()}},
			"floating_network_id": schema.StringAttribute{Computed: true, MarkdownDescription: "The ID of the external network `pool` resolved to.", PlanModifiers: useState},
			"description":         schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "A description of the floating IP.", PlanModifiers: useState},
			"address":             schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "The floating IP address. Request a specific address by setting this; changing it forces a new resource.", PlanModifiers: forceNewStr},
			// Set port_id to a port to associate, or to "" to disassociate. Leave
			// it unset to let a separate pcd_networking_floatingip_associate
			// resource manage the association (UseStateForUnknown keeps the
			// server-managed value stable in that case). ModifyPlan marks the
			// server-derived fields unknown when the association actually changes.
			"port_id":   schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "The port to associate the floating IP with. Set to a port ID to associate, or to an empty string to disassociate. Leave unset to manage the association with a separate `pcd_networking_floatingip_associate` resource.", PlanModifiers: useState},
			"fixed_ip":  schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "The specific fixed IP on the associated port to map to. Defaults to the port's first address.", PlanModifiers: useState},
			"tenant_id": schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "The owning project. Changing this forces a new resource.", PlanModifiers: forceNewStr},
			"status":    schema.StringAttribute{Computed: true, MarkdownDescription: "The operational status of the floating IP."},
			"router_id": schema.StringAttribute{Computed: true, MarkdownDescription: "The router through which the floating IP is routed."},
			"tags":      schema.SetAttribute{Optional: true, Computed: true, ElementType: types.StringType, MarkdownDescription: "Tags applied to the floating IP.", PlanModifiers: []planmodifier.Set{setplanmodifier.UseStateForUnknown()}},
			"region":    schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "The region. Defaults to the provider's region.", PlanModifiers: useState},
		},
	}
}

func (r *floatingIPResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.config = configureClient(req.ProviderData, &resp.Diagnostics)
}

func (r *floatingIPResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan floatingIPModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := r.config.NetworkV2Client()
	if err != nil {
		resp.Diagnostics.AddError("networking: building v2 client", err.Error())
		return
	}

	networkID, err := externalNetworkIDByName(ctx, client, plan.Pool.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("networking: resolving floating IP pool", err.Error())
		return
	}

	createOpts := floatingips.CreateOpts{
		FloatingNetworkID: networkID,
		Description:       plan.Description.ValueString(),
		FloatingIP:        plan.Address.ValueString(),
		PortID:            plan.PortID.ValueString(),
		FixedIP:           plan.FixedIP.ValueString(),
		TenantID:          plan.TenantID.ValueString(),
	}

	fip, err := floatingips.Create(ctx, client, createOpts).Extract()
	if err != nil {
		resp.Diagnostics.AddError("networking: creating floating IP", err.Error())
		return
	}

	if !plan.Tags.IsNull() && !plan.Tags.IsUnknown() {
		var tags []string
		resp.Diagnostics.Append(plan.Tags.ElementsAs(ctx, &tags, false)...)
		if resp.Diagnostics.HasError() {
			return
		}
		if err := replaceTags(ctx, client, "floatingips", fip.ID, tags); err != nil {
			resp.Diagnostics.AddError("networking: setting floating IP tags", err.Error())
			return
		}
	}

	_, readDiags := r.readInto(ctx, client, fip.ID, &plan)
	resp.Diagnostics.Append(readDiags...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *floatingIPResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state floatingIPModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := r.config.NetworkV2Client()
	if err != nil {
		resp.Diagnostics.AddError("networking: building v2 client", err.Error())
		return
	}

	notFound, diags := r.readInto(ctx, client, state.ID.ValueString(), &state)
	if notFound {
		resp.Diagnostics.AddWarning("Floating IP not found",
			fmt.Sprintf("Floating IP %s no longer exists and was removed from state.", state.ID.ValueString()))
		resp.State.RemoveResource(ctx)
		return
	}
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

// ModifyPlan keeps the server-derived fields consistent when the association
// changes. When the user manages port_id inline (sets it to a port or to "") and
// that value differs from state, the server recomputes fixed_ip, router_id, and
// status — so they must be planned unknown, or Terraform reports an inconsistent
// result. When port_id is left unset (config null), the association is managed
// elsewhere (e.g. pcd_networking_floatingip_associate); UseStateForUnknown keeps
// the values stable and we make no changes.
func (r *floatingIPResource) ModifyPlan(ctx context.Context, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	if req.Plan.Raw.IsNull() {
		return // destroy
	}
	var config floatingIPModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &config)...)
	if resp.Diagnostics.HasError() || config.PortID.IsNull() {
		return
	}

	changing := true
	if !req.State.Raw.IsNull() {
		var state floatingIPModel
		resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
		changing = state.PortID.ValueString() != config.PortID.ValueString()
	}
	if !changing {
		return
	}

	resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, path.Root("router_id"), types.StringUnknown())...)
	resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, path.Root("status"), types.StringUnknown())...)
	if config.FixedIP.IsNull() {
		resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, path.Root("fixed_ip"), types.StringUnknown())...)
	}
}

func (r *floatingIPResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state floatingIPModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := r.config.NetworkV2Client()
	if err != nil {
		resp.Diagnostics.AddError("networking: building v2 client", err.Error())
		return
	}

	updateOpts := floatingips.UpdateOpts{}
	changed := false
	if !plan.Description.Equal(state.Description) {
		v := plan.Description.ValueString()
		updateOpts.Description = &v
		changed = true
	}
	if !plan.PortID.Equal(state.PortID) {
		v := plan.PortID.ValueString()
		updateOpts.PortID = &v // pointer to "" disassociates
		changed = true
	}
	if !plan.FixedIP.Equal(state.FixedIP) {
		updateOpts.FixedIP = plan.FixedIP.ValueString()
		changed = true
	}

	if changed {
		if _, err := floatingips.Update(ctx, client, plan.ID.ValueString(), updateOpts).Extract(); err != nil {
			resp.Diagnostics.AddError("networking: updating floating IP", err.Error())
			return
		}
	}

	if !plan.Tags.Equal(state.Tags) {
		var tags []string
		if !plan.Tags.IsNull() && !plan.Tags.IsUnknown() {
			resp.Diagnostics.Append(plan.Tags.ElementsAs(ctx, &tags, false)...)
			if resp.Diagnostics.HasError() {
				return
			}
		}
		if err := replaceTags(ctx, client, "floatingips", plan.ID.ValueString(), tags); err != nil {
			resp.Diagnostics.AddError("networking: updating floating IP tags", err.Error())
			return
		}
	}

	_, readDiags := r.readInto(ctx, client, plan.ID.ValueString(), &plan)
	resp.Diagnostics.Append(readDiags...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *floatingIPResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state floatingIPModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := r.config.NetworkV2Client()
	if err != nil {
		resp.Diagnostics.AddError("networking: building v2 client", err.Error())
		return
	}

	if err := floatingips.Delete(ctx, client, state.ID.ValueString()).ExtractErr(); err != nil {
		if gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
			return
		}
		resp.Diagnostics.AddError("networking: deleting floating IP", err.Error())
	}
}

func (r *floatingIPResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, path.Root("id"), req, resp)
}

// readInto refreshes a floating IP. pool is not refreshed: it holds the external
// network name the user allocated from (Neutron only reports the network ID,
// exposed separately as floating_network_id).
func (r *floatingIPResource) readInto(ctx context.Context, client *gophercloud.ServiceClient, id string, m *floatingIPModel) (notFound bool, diags diag.Diagnostics) {
	fip, err := floatingips.Get(ctx, client, id).Extract()
	if err != nil {
		if gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
			return true, diags
		}
		diags.AddError("networking: reading floating IP", err.Error())
		return false, diags
	}

	m.ID = types.StringValue(fip.ID)
	m.FloatingNetworkID = types.StringValue(fip.FloatingNetworkID)
	m.Description = types.StringValue(fip.Description)
	m.Address = types.StringValue(fip.FloatingIP)
	m.PortID = types.StringValue(fip.PortID)
	m.FixedIP = types.StringValue(fip.FixedIP)
	m.TenantID = types.StringValue(fip.TenantID)
	m.Status = types.StringValue(fip.Status)
	m.RouterID = types.StringValue(fip.RouterID)

	tagVals := fip.Tags
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

// externalNetworkIDByName resolves an external network name to its ID. Exactly
// one external network must match.
func externalNetworkIDByName(ctx context.Context, client *gophercloud.ServiceClient, name string) (string, error) {
	isExternal := true
	listOpts := external.ListOptsExt{
		ListOptsBuilder: networks.ListOpts{Name: name},
		External:        &isExternal,
	}
	pages, err := networks.List(client, listOpts).AllPages(ctx)
	if err != nil {
		return "", err
	}
	all, err := networks.ExtractNetworks(pages)
	if err != nil {
		return "", err
	}
	switch len(all) {
	case 0:
		return "", fmt.Errorf("no external network named %q found", name)
	case 1:
		return all[0].ID, nil
	default:
		return "", fmt.Errorf("%d external networks named %q found; names must be unique", len(all), name)
	}
}
