// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0
//
// Ported from terraform-provider-openstack v3.4.0
// (openstack/resource_openstack_networking_subnet_v2.go), adapted for the
// terraform-plugin-framework and PCD.

package networking

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/subnets"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/int64default"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/listplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/setplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/platform9/terraform-provider-pcd/internal/clients"
)

var (
	_ resource.Resource                = (*subnetResource)(nil)
	_ resource.ResourceWithConfigure   = (*subnetResource)(nil)
	_ resource.ResourceWithImportState = (*subnetResource)(nil)
)

// poolObjType is the element type of the allocation_pools list.
var poolObjType = types.ObjectType{AttrTypes: map[string]attr.Type{
	"start": types.StringType,
	"end":   types.StringType,
}}

// NewSubnetResource is the factory registered with the provider.
func NewSubnetResource() resource.Resource {
	return &subnetResource{}
}

type subnetResource struct {
	config *clients.Config
}

type subnetModel struct {
	ID              types.String `tfsdk:"id"`
	NetworkID       types.String `tfsdk:"network_id"`
	Name            types.String `tfsdk:"name"`
	Description     types.String `tfsdk:"description"`
	CIDR            types.String `tfsdk:"cidr"`
	IPVersion       types.Int64  `tfsdk:"ip_version"`
	GatewayIP       types.String `tfsdk:"gateway_ip"`
	EnableDHCP      types.Bool   `tfsdk:"enable_dhcp"`
	DNSNameservers  types.List   `tfsdk:"dns_nameservers"`
	AllocationPools types.List   `tfsdk:"allocation_pools"`
	TenantID        types.String `tfsdk:"tenant_id"`
	Tags            types.Set    `tfsdk:"tags"`
	Region          types.String `tfsdk:"region"`
}

type allocationPoolModel struct {
	Start types.String `tfsdk:"start"`
	End   types.String `tfsdk:"end"`
}

func (r *subnetResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_networking_subnet"
}

func (r *subnetResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	forceNewStr := []planmodifier.String{stringplanmodifier.RequiresReplace(), stringplanmodifier.UseStateForUnknown()}
	resp.Schema = schema.Schema{
		MarkdownDescription: "Manages a subnet on a Neutron network in PCD.",
		Attributes: map[string]schema.Attribute{
			"id":          schema.StringAttribute{Computed: true, MarkdownDescription: "The subnet ID.", PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()}},
			"network_id":  schema.StringAttribute{Required: true, MarkdownDescription: "The network this subnet belongs to. Changing this forces a new resource.", PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()}},
			"name":        schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "The name of the subnet.", PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()}},
			"description": schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "A description of the subnet.", PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()}},
			"cidr":        schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "The CIDR of the subnet. Changing this forces a new resource.", PlanModifiers: forceNewStr},
			"ip_version":  schema.Int64Attribute{Optional: true, Computed: true, Default: int64default.StaticInt64(4), MarkdownDescription: "IP version (4 or 6)."},
			"gateway_ip":  schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "The gateway IP. Defaults to the first address in the CIDR.", PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()}},
			"enable_dhcp": schema.BoolAttribute{Optional: true, Computed: true, Default: booldefault.StaticBool(true), MarkdownDescription: "Whether DHCP is enabled for the subnet."},
			"dns_nameservers": schema.ListAttribute{
				Optional: true, Computed: true, ElementType: types.StringType, MarkdownDescription: "DNS nameservers for the subnet.",
				PlanModifiers: []planmodifier.List{listplanmodifier.UseStateForUnknown()},
			},
			"allocation_pools": schema.ListNestedAttribute{
				Optional: true, Computed: true, MarkdownDescription: "IP allocation pools (DHCP ranges).",
				NestedObject: schema.NestedAttributeObject{Attributes: map[string]schema.Attribute{
					"start": schema.StringAttribute{Required: true, MarkdownDescription: "First address of the pool."},
					"end":   schema.StringAttribute{Required: true, MarkdownDescription: "Last address of the pool."},
				}},
				PlanModifiers: []planmodifier.List{listplanmodifier.UseStateForUnknown()},
			},
			"tenant_id": schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "The owning project. Changing this forces a new resource.", PlanModifiers: forceNewStr},
			"tags":      schema.SetAttribute{Optional: true, Computed: true, ElementType: types.StringType, MarkdownDescription: "Tags applied to the subnet.", PlanModifiers: []planmodifier.Set{setplanmodifier.UseStateForUnknown()}},
			"region":    schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "The region. Defaults to the provider's region.", PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()}},
		},
	}
}

func (r *subnetResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.config = configureClient(req.ProviderData, &resp.Diagnostics)
}

func (r *subnetResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan subnetModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := r.config.NetworkV2Client()
	if err != nil {
		resp.Diagnostics.AddError("networking: building v2 client", err.Error())
		return
	}

	enableDHCP := plan.EnableDHCP.ValueBool()
	createOpts := subnets.CreateOpts{
		NetworkID:       plan.NetworkID.ValueString(),
		CIDR:            plan.CIDR.ValueString(),
		Name:            plan.Name.ValueString(),
		Description:     plan.Description.ValueString(),
		IPVersion:       gophercloud.IPVersion(plan.IPVersion.ValueInt64()),
		EnableDHCP:      &enableDHCP,
		TenantID:        plan.TenantID.ValueString(),
		DNSNameservers:  listToStrings(ctx, plan.DNSNameservers, &resp.Diagnostics),
		AllocationPools: poolsFromList(ctx, plan.AllocationPools, &resp.Diagnostics),
	}
	if v := plan.GatewayIP.ValueString(); v != "" {
		createOpts.GatewayIP = &v
	}
	if resp.Diagnostics.HasError() {
		return
	}

	sub, err := subnets.Create(ctx, client, createOpts).Extract()
	if err != nil {
		resp.Diagnostics.AddError("networking: creating subnet", err.Error())
		return
	}

	if !plan.Tags.IsNull() && !plan.Tags.IsUnknown() {
		var tags []string
		resp.Diagnostics.Append(plan.Tags.ElementsAs(ctx, &tags, false)...)
		if resp.Diagnostics.HasError() {
			return
		}
		if err := replaceTags(ctx, client, "subnets", sub.ID, tags); err != nil {
			resp.Diagnostics.AddError("networking: setting subnet tags", err.Error())
			return
		}
	}

	_, readDiags := r.readInto(ctx, client, sub.ID, &plan)
	resp.Diagnostics.Append(readDiags...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *subnetResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state subnetModel
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
		resp.Diagnostics.AddWarning("Subnet not found",
			fmt.Sprintf("Subnet %s no longer exists and was removed from state.", state.ID.ValueString()))
		resp.State.RemoveResource(ctx)
		return
	}
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *subnetResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state subnetModel
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

	name := plan.Name.ValueString()
	description := plan.Description.ValueString()
	enableDHCP := plan.EnableDHCP.ValueBool()
	updateOpts := subnets.UpdateOpts{Name: &name, Description: &description, EnableDHCP: &enableDHCP}
	if v := plan.GatewayIP.ValueString(); v != "" {
		updateOpts.GatewayIP = &v
	}
	if !plan.DNSNameservers.Equal(state.DNSNameservers) {
		dns := listToStrings(ctx, plan.DNSNameservers, &resp.Diagnostics)
		updateOpts.DNSNameservers = &dns
	}
	if !plan.AllocationPools.Equal(state.AllocationPools) {
		updateOpts.AllocationPools = poolsFromList(ctx, plan.AllocationPools, &resp.Diagnostics)
	}
	if resp.Diagnostics.HasError() {
		return
	}

	if _, err := subnets.Update(ctx, client, plan.ID.ValueString(), updateOpts).Extract(); err != nil {
		resp.Diagnostics.AddError("networking: updating subnet", err.Error())
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
		if err := replaceTags(ctx, client, "subnets", plan.ID.ValueString(), tags); err != nil {
			resp.Diagnostics.AddError("networking: updating subnet tags", err.Error())
			return
		}
	}

	_, readDiags := r.readInto(ctx, client, plan.ID.ValueString(), &plan)
	resp.Diagnostics.Append(readDiags...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *subnetResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state subnetModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := r.config.NetworkV2Client()
	if err != nil {
		resp.Diagnostics.AddError("networking: building v2 client", err.Error())
		return
	}

	// A subnet can briefly report SubnetInUse (409) while Nova asynchronously
	// releases the ports of a just-deleted instance. Retry until the ports are
	// gone (or the subnet is), bounded by a timeout.
	id := state.ID.ValueString()
	waitCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	for {
		err := subnets.Delete(waitCtx, client, id).ExtractErr()
		if err == nil || gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
			return
		}
		if !gophercloud.ResponseCodeIs(err, http.StatusConflict) {
			resp.Diagnostics.AddError("networking: deleting subnet", err.Error())
			return
		}
		select {
		case <-waitCtx.Done():
			resp.Diagnostics.AddError("networking: deleting subnet",
				fmt.Sprintf("subnet %s still in use (ports not yet released): %v", id, err))
			return
		case <-time.After(3 * time.Second):
		}
	}
}

func (r *subnetResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, path.Root("id"), req, resp)
}

func (r *subnetResource) readInto(ctx context.Context, client *gophercloud.ServiceClient, id string, m *subnetModel) (notFound bool, diags diag.Diagnostics) {
	sub, err := subnets.Get(ctx, client, id).Extract()
	if err != nil {
		if gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
			return true, diags
		}
		diags.AddError("networking: reading subnet", err.Error())
		return false, diags
	}

	m.ID = types.StringValue(sub.ID)
	m.NetworkID = types.StringValue(sub.NetworkID)
	m.Name = types.StringValue(sub.Name)
	m.Description = types.StringValue(sub.Description)
	m.CIDR = types.StringValue(sub.CIDR)
	m.IPVersion = types.Int64Value(int64(sub.IPVersion))
	m.GatewayIP = types.StringValue(sub.GatewayIP)
	m.EnableDHCP = types.BoolValue(sub.EnableDHCP)
	m.TenantID = types.StringValue(sub.TenantID)

	dnsList, d := types.ListValueFrom(ctx, types.StringType, sub.DNSNameservers)
	diags = append(diags, d...)
	m.DNSNameservers = dnsList

	pools := make([]allocationPoolModel, 0, len(sub.AllocationPools))
	for _, p := range sub.AllocationPools {
		pools = append(pools, allocationPoolModel{Start: types.StringValue(p.Start), End: types.StringValue(p.End)})
	}
	poolList, d := types.ListValueFrom(ctx, poolObjType, pools)
	diags = append(diags, d...)
	m.AllocationPools = poolList

	tagVals := sub.Tags
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

func listToStrings(ctx context.Context, l types.List, diags *diag.Diagnostics) []string {
	if l.IsNull() || l.IsUnknown() {
		return nil
	}
	var out []string
	diags.Append(l.ElementsAs(ctx, &out, false)...)
	return out
}

func poolsFromList(ctx context.Context, l types.List, diags *diag.Diagnostics) []subnets.AllocationPool {
	if l.IsNull() || l.IsUnknown() {
		return nil
	}
	var models []allocationPoolModel
	diags.Append(l.ElementsAs(ctx, &models, false)...)
	out := make([]subnets.AllocationPool, 0, len(models))
	for _, p := range models {
		out = append(out, subnets.AllocationPool{Start: p.Start.ValueString(), End: p.End.ValueString()})
	}
	return out
}
