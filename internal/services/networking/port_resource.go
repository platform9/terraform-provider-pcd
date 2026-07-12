// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0
//
// Ported from terraform-provider-openstack v3.4.0
// (openstack/resource_openstack_networking_port_v2.go), adapted for the
// terraform-plugin-framework and PCD.

package networking

import (
	"context"
	"fmt"
	"net/http"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/ports"
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
	_ resource.Resource                = (*portResource)(nil)
	_ resource.ResourceWithConfigure   = (*portResource)(nil)
	_ resource.ResourceWithImportState = (*portResource)(nil)
)

// NewPortResource is the factory registered with the provider.
func NewPortResource() resource.Resource {
	return &portResource{}
}

type portResource struct {
	config *clients.Config
}

type portModel struct {
	ID                  types.String `tfsdk:"id"`
	NetworkID           types.String `tfsdk:"network_id"`
	Name                types.String `tfsdk:"name"`
	Description         types.String `tfsdk:"description"`
	AdminStateUp        types.Bool   `tfsdk:"admin_state_up"`
	MACAddress          types.String `tfsdk:"mac_address"`
	DeviceID            types.String `tfsdk:"device_id"`
	DeviceOwner         types.String `tfsdk:"device_owner"`
	FixedIP             types.List   `tfsdk:"fixed_ip"`
	SecurityGroupIDs    types.Set    `tfsdk:"security_group_ids"`
	AllowedAddressPairs types.Set    `tfsdk:"allowed_address_pairs"`
	TenantID            types.String `tfsdk:"tenant_id"`
	Tags                types.Set    `tfsdk:"tags"`
	Status              types.String `tfsdk:"status"`
	AllFixedIPs         types.List   `tfsdk:"all_fixed_ips"`
	Region              types.String `tfsdk:"region"`
}

type portFixedIPModel struct {
	SubnetID  types.String `tfsdk:"subnet_id"`
	IPAddress types.String `tfsdk:"ip_address"`
}

type portAddrPairModel struct {
	IPAddress  types.String `tfsdk:"ip_address"`
	MACAddress types.String `tfsdk:"mac_address"`
}

func (r *portResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_networking_port"
}

func (r *portResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	useState := []planmodifier.String{stringplanmodifier.UseStateForUnknown()}
	forceNewStr := []planmodifier.String{stringplanmodifier.RequiresReplace(), stringplanmodifier.UseStateForUnknown()}
	resp.Schema = schema.Schema{
		MarkdownDescription: "Manages a Neutron port in PCD. A port is a virtual switch connection on a " +
			"network; it can be attached to an instance, carry floating IPs, or host allowed-address-pairs " +
			"for HA/VRRP setups.\n\n" +
			"`fixed_ip` and `allowed_address_pairs` capture your requested configuration and are not refreshed " +
			"from the server on read (Neutron fills in addresses and MACs that would otherwise churn the plan); " +
			"use the computed `all_fixed_ips` to reference the addresses Neutron actually assigned.",
		Attributes: map[string]schema.Attribute{
			"id":             schema.StringAttribute{Computed: true, MarkdownDescription: "The port ID.", PlanModifiers: useState},
			"network_id":     schema.StringAttribute{Required: true, MarkdownDescription: "The network this port belongs to. Changing this forces a new resource.", PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()}},
			"name":           schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "The name of the port.", PlanModifiers: useState},
			"description":    schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "A description of the port.", PlanModifiers: useState},
			"admin_state_up": schema.BoolAttribute{Optional: true, Computed: true, Default: booldefault.StaticBool(true), MarkdownDescription: "The administrative state of the port (true = up)."},
			"mac_address":    schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "The MAC address of the port. Setting a specific MAC forces a new resource.", PlanModifiers: forceNewStr},
			"device_id":      schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "The ID of the device (e.g. instance) using the port.", PlanModifiers: useState},
			"device_owner":   schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "The entity type using the port (e.g. compute:nova).", PlanModifiers: useState},
			"fixed_ip": schema.ListNestedAttribute{
				Optional:            true,
				MarkdownDescription: "Requested fixed IPs. Each entry pins the port to a subnet and optionally a specific address. Not refreshed from the server; see `all_fixed_ips`.",
				NestedObject: schema.NestedAttributeObject{Attributes: map[string]schema.Attribute{
					"subnet_id":  schema.StringAttribute{Optional: true, MarkdownDescription: "Subnet to allocate the fixed IP from."},
					"ip_address": schema.StringAttribute{Optional: true, MarkdownDescription: "A specific IP address to assign."},
				}},
			},
			"security_group_ids": schema.SetAttribute{
				Optional: true, Computed: true, ElementType: types.StringType,
				MarkdownDescription: "Security groups applied to the port. Omit to inherit the network's default group; set to `[]` to apply none.",
				PlanModifiers:       []planmodifier.Set{setplanmodifier.UseStateForUnknown()},
			},
			"allowed_address_pairs": schema.SetNestedAttribute{
				Optional:            true,
				MarkdownDescription: "Additional IP/MAC pairs the port is allowed to source traffic from (VRRP/HA). Not refreshed from the server.",
				NestedObject: schema.NestedAttributeObject{Attributes: map[string]schema.Attribute{
					"ip_address":  schema.StringAttribute{Required: true, MarkdownDescription: "Allowed IP address or CIDR."},
					"mac_address": schema.StringAttribute{Optional: true, MarkdownDescription: "Allowed MAC address. Defaults to the port MAC."},
				}},
			},
			"tenant_id":     schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "The owning project. Changing this forces a new resource.", PlanModifiers: forceNewStr},
			"tags":          schema.SetAttribute{Optional: true, Computed: true, ElementType: types.StringType, MarkdownDescription: "Tags applied to the port.", PlanModifiers: []planmodifier.Set{setplanmodifier.UseStateForUnknown()}},
			"status":        schema.StringAttribute{Computed: true, MarkdownDescription: "The operational status of the port."},
			"all_fixed_ips": schema.ListAttribute{Computed: true, ElementType: types.StringType, MarkdownDescription: "The IP addresses Neutron assigned to the port, in order."},
			"region":        schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "The region. Defaults to the provider's region.", PlanModifiers: useState},
		},
	}
}

func (r *portResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.config = configureClient(req.ProviderData, &resp.Diagnostics)
}

func (r *portResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan portModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := r.config.NetworkV2Client()
	if err != nil {
		resp.Diagnostics.AddError("networking: building v2 client", err.Error())
		return
	}

	adminUp := plan.AdminStateUp.ValueBool()
	createOpts := ports.CreateOpts{
		NetworkID:    plan.NetworkID.ValueString(),
		Name:         plan.Name.ValueString(),
		Description:  plan.Description.ValueString(),
		AdminStateUp: &adminUp,
		MACAddress:   plan.MACAddress.ValueString(),
		DeviceID:     plan.DeviceID.ValueString(),
		DeviceOwner:  plan.DeviceOwner.ValueString(),
		TenantID:     plan.TenantID.ValueString(),
	}
	if fips := fixedIPsFromList(ctx, plan.FixedIP, &resp.Diagnostics); len(fips) > 0 {
		createOpts.FixedIPs = fips
	}
	if !plan.SecurityGroupIDs.IsNull() && !plan.SecurityGroupIDs.IsUnknown() {
		var sgs []string
		resp.Diagnostics.Append(plan.SecurityGroupIDs.ElementsAs(ctx, &sgs, false)...)
		createOpts.SecurityGroups = &sgs
	}
	if pairs := addrPairsFromSet(ctx, plan.AllowedAddressPairs, &resp.Diagnostics); len(pairs) > 0 {
		createOpts.AllowedAddressPairs = pairs
	}
	if resp.Diagnostics.HasError() {
		return
	}

	port, err := ports.Create(ctx, client, createOpts).Extract()
	if err != nil {
		resp.Diagnostics.AddError("networking: creating port", err.Error())
		return
	}

	if !plan.Tags.IsNull() && !plan.Tags.IsUnknown() {
		var tags []string
		resp.Diagnostics.Append(plan.Tags.ElementsAs(ctx, &tags, false)...)
		if resp.Diagnostics.HasError() {
			return
		}
		if err := replaceTags(ctx, client, "ports", port.ID, tags); err != nil {
			resp.Diagnostics.AddError("networking: setting port tags", err.Error())
			return
		}
	}

	_, readDiags := r.readInto(ctx, client, port.ID, &plan)
	resp.Diagnostics.Append(readDiags...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *portResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state portModel
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
		resp.Diagnostics.AddWarning("Port not found",
			fmt.Sprintf("Port %s no longer exists and was removed from state.", state.ID.ValueString()))
		resp.State.RemoveResource(ctx)
		return
	}
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *portResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state portModel
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

	updateOpts := ports.UpdateOpts{}
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
	if !plan.DeviceID.Equal(state.DeviceID) {
		v := plan.DeviceID.ValueString()
		updateOpts.DeviceID = &v
	}
	if !plan.DeviceOwner.Equal(state.DeviceOwner) {
		v := plan.DeviceOwner.ValueString()
		updateOpts.DeviceOwner = &v
	}
	if !plan.SecurityGroupIDs.Equal(state.SecurityGroupIDs) {
		var sgs []string
		if !plan.SecurityGroupIDs.IsNull() && !plan.SecurityGroupIDs.IsUnknown() {
			resp.Diagnostics.Append(plan.SecurityGroupIDs.ElementsAs(ctx, &sgs, false)...)
		}
		updateOpts.SecurityGroups = &sgs
	}
	if !plan.FixedIP.Equal(state.FixedIP) {
		fips := fixedIPsFromList(ctx, plan.FixedIP, &resp.Diagnostics)
		if fips == nil {
			fips = []ports.IP{}
		}
		updateOpts.FixedIPs = fips
	}
	if !plan.AllowedAddressPairs.Equal(state.AllowedAddressPairs) {
		pairs := addrPairsFromSet(ctx, plan.AllowedAddressPairs, &resp.Diagnostics)
		if pairs == nil {
			pairs = []ports.AddressPair{}
		}
		updateOpts.AllowedAddressPairs = &pairs
	}
	if resp.Diagnostics.HasError() {
		return
	}

	if _, err := ports.Update(ctx, client, plan.ID.ValueString(), updateOpts).Extract(); err != nil {
		resp.Diagnostics.AddError("networking: updating port", err.Error())
		return
	}

	if !plan.Tags.Equal(state.Tags) {
		var tags []string
		if !plan.Tags.IsNull() && !plan.Tags.IsUnknown() {
			resp.Diagnostics.Append(plan.Tags.ElementsAs(ctx, &tags, false)...)
			if resp.Diagnostics.HasError() {
				return
			}
		}
		if err := replaceTags(ctx, client, "ports", plan.ID.ValueString(), tags); err != nil {
			resp.Diagnostics.AddError("networking: updating port tags", err.Error())
			return
		}
	}

	_, readDiags := r.readInto(ctx, client, plan.ID.ValueString(), &plan)
	resp.Diagnostics.Append(readDiags...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *portResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state portModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := r.config.NetworkV2Client()
	if err != nil {
		resp.Diagnostics.AddError("networking: building v2 client", err.Error())
		return
	}

	if err := ports.Delete(ctx, client, state.ID.ValueString()).ExtractErr(); err != nil {
		if gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
			return
		}
		resp.Diagnostics.AddError("networking: deleting port", err.Error())
	}
}

func (r *portResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, path.Root("id"), req, resp)
}

// readInto refreshes the server-managed attributes of a port. fixed_ip and
// allowed_address_pairs are deliberately left untouched: they hold the user's
// requested configuration, and Neutron returns server-filled values (assigned
// IPs, the port MAC on each pair) that would otherwise produce a perpetual diff.
func (r *portResource) readInto(ctx context.Context, client *gophercloud.ServiceClient, id string, m *portModel) (notFound bool, diags diag.Diagnostics) {
	port, err := ports.Get(ctx, client, id).Extract()
	if err != nil {
		if gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
			return true, diags
		}
		diags.AddError("networking: reading port", err.Error())
		return false, diags
	}

	m.ID = types.StringValue(port.ID)
	m.NetworkID = types.StringValue(port.NetworkID)
	m.Name = types.StringValue(port.Name)
	m.Description = types.StringValue(port.Description)
	m.AdminStateUp = types.BoolValue(port.AdminStateUp)
	m.MACAddress = types.StringValue(port.MACAddress)
	m.DeviceID = types.StringValue(port.DeviceID)
	m.DeviceOwner = types.StringValue(port.DeviceOwner)
	m.TenantID = types.StringValue(port.TenantID)
	m.Status = types.StringValue(port.Status)

	sgVals := port.SecurityGroups
	if sgVals == nil {
		sgVals = []string{}
	}
	sgs, d := types.SetValueFrom(ctx, types.StringType, sgVals)
	diags = append(diags, d...)
	m.SecurityGroupIDs = sgs

	ips := make([]string, 0, len(port.FixedIPs))
	for _, fip := range port.FixedIPs {
		ips = append(ips, fip.IPAddress)
	}
	allIPs, d := types.ListValueFrom(ctx, types.StringType, ips)
	diags = append(diags, d...)
	m.AllFixedIPs = allIPs

	tagVals := port.Tags
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

func fixedIPsFromList(ctx context.Context, l types.List, diags *diag.Diagnostics) []ports.IP {
	if l.IsNull() || l.IsUnknown() {
		return nil
	}
	var models []portFixedIPModel
	diags.Append(l.ElementsAs(ctx, &models, false)...)
	out := make([]ports.IP, 0, len(models))
	for _, m := range models {
		out = append(out, ports.IP{SubnetID: m.SubnetID.ValueString(), IPAddress: m.IPAddress.ValueString()})
	}
	return out
}

func addrPairsFromSet(ctx context.Context, s types.Set, diags *diag.Diagnostics) []ports.AddressPair {
	if s.IsNull() || s.IsUnknown() {
		return nil
	}
	var models []portAddrPairModel
	diags.Append(s.ElementsAs(ctx, &models, false)...)
	out := make([]ports.AddressPair, 0, len(models))
	for _, m := range models {
		out = append(out, ports.AddressPair{IPAddress: m.IPAddress.ValueString(), MACAddress: m.MACAddress.ValueString()})
	}
	return out
}
