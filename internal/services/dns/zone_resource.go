// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0
//
// Ported from terraform-provider-openstack v3.4.0
// (openstack/resource_openstack_dns_zone_v2.go), adapted for the
// terraform-plugin-framework and PCD.

package dns

import (
	"context"
	"fmt"
	"net/http"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/dns/v2/zones"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/int64planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/listplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/mapplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/platform9/terraform-provider-pcd/internal/clients"
)

var (
	_ resource.Resource                = (*zoneResource)(nil)
	_ resource.ResourceWithConfigure   = (*zoneResource)(nil)
	_ resource.ResourceWithImportState = (*zoneResource)(nil)
)

// NewZoneResource is the factory registered with the provider.
func NewZoneResource() resource.Resource {
	return &zoneResource{}
}

type zoneResource struct {
	config *clients.Config
}

type zoneModel struct {
	ID          types.String `tfsdk:"id"`
	Name        types.String `tfsdk:"name"`
	Type        types.String `tfsdk:"type"`
	Email       types.String `tfsdk:"email"`
	TTL         types.Int64  `tfsdk:"ttl"`
	Description types.String `tfsdk:"description"`
	Masters     types.List   `tfsdk:"masters"`
	Attributes  types.Map    `tfsdk:"attributes"`
	Serial      types.Int64  `tfsdk:"serial"`
	Status      types.String `tfsdk:"status"`
	PoolID      types.String `tfsdk:"pool_id"`
	ProjectID   types.String `tfsdk:"project_id"`
	Region      types.String `tfsdk:"region"`
}

func (r *zoneResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_dns_zone"
}

func (r *zoneResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	useState := []planmodifier.String{stringplanmodifier.UseStateForUnknown()}
	forceNew := []planmodifier.String{stringplanmodifier.RequiresReplace()}
	forceNewC := []planmodifier.String{stringplanmodifier.RequiresReplace(), stringplanmodifier.UseStateForUnknown()}
	resp.Schema = schema.Schema{
		MarkdownDescription: "Manages a DNS zone in PCD's Designate service. Zone operations are asynchronous; " +
			"applies wait for the zone to reach an `ACTIVE` status.",
		Attributes: map[string]schema.Attribute{
			"id":          schema.StringAttribute{Computed: true, MarkdownDescription: "The zone ID.", PlanModifiers: useState},
			"name":        schema.StringAttribute{Required: true, MarkdownDescription: "The zone name — a fully-qualified domain name ending in a dot (e.g. `example.com.`). Changing this forces a new resource.", PlanModifiers: forceNew},
			"type":        schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "The zone type: PRIMARY (default) or SECONDARY. Changing this forces a new resource.", PlanModifiers: forceNewC},
			"email":       schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "The email for the zone's SOA record (required for PRIMARY zones).", PlanModifiers: useState},
			"ttl":         schema.Int64Attribute{Optional: true, Computed: true, MarkdownDescription: "The zone TTL in seconds. Omit to accept the pool default.", PlanModifiers: []planmodifier.Int64{int64planmodifier.UseStateForUnknown()}},
			"description": schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "A description of the zone.", PlanModifiers: useState},
			"masters":     schema.ListAttribute{Optional: true, Computed: true, ElementType: types.StringType, MarkdownDescription: "Master nameservers for a SECONDARY zone.", PlanModifiers: []planmodifier.List{listplanmodifier.UseStateForUnknown()}},
			"attributes":  schema.MapAttribute{Optional: true, Computed: true, ElementType: types.StringType, MarkdownDescription: "Key/value zone attributes (e.g. pool scheduling hints). Changing this forces a new resource.", PlanModifiers: []planmodifier.Map{mapplanmodifier.RequiresReplace(), mapplanmodifier.UseStateForUnknown()}},
			"serial":      schema.Int64Attribute{Computed: true, MarkdownDescription: "The zone serial number.", PlanModifiers: []planmodifier.Int64{int64planmodifier.UseStateForUnknown()}},
			"status":      schema.StringAttribute{Computed: true, MarkdownDescription: "The zone status (e.g. ACTIVE).", PlanModifiers: useState},
			"pool_id":     schema.StringAttribute{Computed: true, MarkdownDescription: "The pool the zone is scheduled on.", PlanModifiers: useState},
			"project_id":  schema.StringAttribute{Computed: true, MarkdownDescription: "The owning project.", PlanModifiers: useState},
			"region":      schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "The region. Defaults to the provider's region.", PlanModifiers: useState},
		},
	}
}

func (r *zoneResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.config = configureClient(req.ProviderData, &resp.Diagnostics)
}

func (r *zoneResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan zoneModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := r.config.DNSV2Client()
	if err != nil {
		resp.Diagnostics.AddError("dns: building v2 client", err.Error())
		return
	}

	createOpts := zones.CreateOpts{
		Name:        plan.Name.ValueString(),
		Type:        plan.Type.ValueString(),
		Email:       plan.Email.ValueString(),
		Description: plan.Description.ValueString(),
		Masters:     listToStrings(ctx, plan.Masters, &resp.Diagnostics),
		Attributes:  mapToStrings(ctx, plan.Attributes, &resp.Diagnostics),
	}
	if plan.TTL.ValueInt64() > 0 {
		createOpts.TTL = int(plan.TTL.ValueInt64())
	}
	if resp.Diagnostics.HasError() {
		return
	}

	zone, err := zones.Create(ctx, client, createOpts).Extract()
	if err != nil {
		resp.Diagnostics.AddError("dns: creating zone", err.Error())
		return
	}
	if err := waitForZoneActive(ctx, client, zone.ID, defaultDNSTimeout); err != nil {
		resp.Diagnostics.AddError("dns: waiting for zone to become active", err.Error())
		return
	}

	_, readDiags := r.readInto(ctx, client, zone.ID, &plan)
	resp.Diagnostics.Append(readDiags...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *zoneResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state zoneModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := r.config.DNSV2Client()
	if err != nil {
		resp.Diagnostics.AddError("dns: building v2 client", err.Error())
		return
	}

	notFound, diags := r.readInto(ctx, client, state.ID.ValueString(), &state)
	if notFound {
		resp.Diagnostics.AddWarning("Zone not found",
			fmt.Sprintf("Zone %s no longer exists and was removed from state.", state.ID.ValueString()))
		resp.State.RemoveResource(ctx)
		return
	}
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *zoneResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state zoneModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := r.config.DNSV2Client()
	if err != nil {
		resp.Diagnostics.AddError("dns: building v2 client", err.Error())
		return
	}

	desc := plan.Description.ValueString()
	updateOpts := zones.UpdateOpts{
		Email:       plan.Email.ValueString(),
		Description: &desc,
		Masters:     listToStrings(ctx, plan.Masters, &resp.Diagnostics),
	}
	if plan.TTL.ValueInt64() > 0 {
		updateOpts.TTL = int(plan.TTL.ValueInt64())
	}
	if resp.Diagnostics.HasError() {
		return
	}

	id := plan.ID.ValueString()
	if _, err := zones.Update(ctx, client, id, updateOpts).Extract(); err != nil {
		resp.Diagnostics.AddError("dns: updating zone", err.Error())
		return
	}
	if err := waitForZoneActive(ctx, client, id, defaultDNSTimeout); err != nil {
		resp.Diagnostics.AddError("dns: waiting for zone after update", err.Error())
		return
	}

	_, readDiags := r.readInto(ctx, client, id, &plan)
	resp.Diagnostics.Append(readDiags...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *zoneResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state zoneModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := r.config.DNSV2Client()
	if err != nil {
		resp.Diagnostics.AddError("dns: building v2 client", err.Error())
		return
	}

	id := state.ID.ValueString()
	if _, err := zones.Delete(ctx, client, id).Extract(); err != nil {
		if gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
			return
		}
		resp.Diagnostics.AddError("dns: deleting zone", err.Error())
		return
	}
	if err := waitForZoneDeleted(ctx, client, id, defaultDNSTimeout); err != nil {
		resp.Diagnostics.AddError("dns: waiting for zone deletion", err.Error())
	}
}

func (r *zoneResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, path.Root("id"), req, resp)
}

func (r *zoneResource) readInto(ctx context.Context, client *gophercloud.ServiceClient, id string, m *zoneModel) (notFound bool, diags diag.Diagnostics) {
	zone, err := zones.Get(ctx, client, id).Extract()
	if err != nil {
		if gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
			return true, diags
		}
		diags.AddError("dns: reading zone", err.Error())
		return false, diags
	}

	m.ID = types.StringValue(zone.ID)
	m.Name = types.StringValue(zone.Name)
	m.Type = types.StringValue(zone.Type)
	m.Email = types.StringValue(zone.Email)
	m.TTL = types.Int64Value(int64(zone.TTL))
	m.Description = types.StringValue(zone.Description)
	m.Serial = types.Int64Value(int64(zone.Serial))
	m.Status = types.StringValue(zone.Status)
	m.PoolID = types.StringValue(zone.PoolID)
	m.ProjectID = types.StringValue(zone.ProjectID)

	masterVals := zone.Masters
	if masterVals == nil {
		masterVals = []string{}
	}
	masters, d := types.ListValueFrom(ctx, types.StringType, masterVals)
	diags = append(diags, d...)
	m.Masters = masters

	attrVals := zone.Attributes
	if attrVals == nil {
		attrVals = map[string]string{}
	}
	attrs, d := types.MapValueFrom(ctx, types.StringType, attrVals)
	diags = append(diags, d...)
	m.Attributes = attrs

	if m.Region.IsNull() || m.Region.IsUnknown() {
		m.Region = types.StringValue(r.config.Region)
	}
	return false, diags
}
