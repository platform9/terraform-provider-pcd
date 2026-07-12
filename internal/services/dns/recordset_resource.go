// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0
//
// Ported from terraform-provider-openstack v3.4.0
// (openstack/resource_openstack_dns_recordset_v2.go), adapted for the
// terraform-plugin-framework and PCD.

package dns

import (
	"context"
	"fmt"
	"net/http"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/dns/v2/recordsets"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/int64planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/platform9/terraform-provider-pcd/internal/clients"
)

var (
	_ resource.Resource                = (*recordSetResource)(nil)
	_ resource.ResourceWithConfigure   = (*recordSetResource)(nil)
	_ resource.ResourceWithImportState = (*recordSetResource)(nil)
)

// NewRecordSetResource is the factory registered with the provider.
func NewRecordSetResource() resource.Resource {
	return &recordSetResource{}
}

type recordSetResource struct {
	config *clients.Config
}

type recordSetModel struct {
	ID          types.String `tfsdk:"id"`
	ZoneID      types.String `tfsdk:"zone_id"`
	Name        types.String `tfsdk:"name"`
	Type        types.String `tfsdk:"type"`
	Records     types.Set    `tfsdk:"records"`
	TTL         types.Int64  `tfsdk:"ttl"`
	Description types.String `tfsdk:"description"`
	Status      types.String `tfsdk:"status"`
	Region      types.String `tfsdk:"region"`
}

func (r *recordSetResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_dns_recordset"
}

func (r *recordSetResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	useState := []planmodifier.String{stringplanmodifier.UseStateForUnknown()}
	forceNew := []planmodifier.String{stringplanmodifier.RequiresReplace()}
	resp.Schema = schema.Schema{
		MarkdownDescription: "Manages a recordset within a DNS zone in PCD's Designate service. Operations are " +
			"asynchronous; applies wait for the recordset to reach an `ACTIVE` status.",
		Attributes: map[string]schema.Attribute{
			"id":          schema.StringAttribute{Computed: true, MarkdownDescription: "The recordset ID.", PlanModifiers: useState},
			"zone_id":     schema.StringAttribute{Required: true, MarkdownDescription: "The zone the recordset belongs to. Changing this forces a new resource.", PlanModifiers: forceNew},
			"name":        schema.StringAttribute{Required: true, MarkdownDescription: "The recordset name — a fully-qualified domain name ending in a dot. Changing this forces a new resource.", PlanModifiers: forceNew},
			"type":        schema.StringAttribute{Required: true, MarkdownDescription: "The record type (A, AAAA, CNAME, MX, TXT, SRV, ...). Changing this forces a new resource.", PlanModifiers: forceNew},
			"records":     schema.SetAttribute{Required: true, ElementType: types.StringType, MarkdownDescription: "The record values."},
			"ttl":         schema.Int64Attribute{Optional: true, Computed: true, MarkdownDescription: "The recordset TTL in seconds. Omit to inherit the zone default.", PlanModifiers: []planmodifier.Int64{int64planmodifier.UseStateForUnknown()}},
			"description": schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "A description of the recordset.", PlanModifiers: useState},
			"status":      schema.StringAttribute{Computed: true, MarkdownDescription: "The recordset status (e.g. ACTIVE).", PlanModifiers: useState},
			"region":      schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "The region. Defaults to the provider's region.", PlanModifiers: useState},
		},
	}
}

func (r *recordSetResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.config = configureClient(req.ProviderData, &resp.Diagnostics)
}

func (r *recordSetResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan recordSetModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := r.config.DNSV2Client()
	if err != nil {
		resp.Diagnostics.AddError("dns: building v2 client", err.Error())
		return
	}

	zoneID := plan.ZoneID.ValueString()
	createOpts := recordsets.CreateOpts{
		Name:        plan.Name.ValueString(),
		Type:        plan.Type.ValueString(),
		Description: plan.Description.ValueString(),
		Records:     setToStrings(ctx, plan.Records, &resp.Diagnostics),
	}
	if plan.TTL.ValueInt64() > 0 {
		createOpts.TTL = int(plan.TTL.ValueInt64())
	}
	if resp.Diagnostics.HasError() {
		return
	}

	rr, err := recordsets.Create(ctx, client, zoneID, createOpts).Extract()
	if err != nil {
		resp.Diagnostics.AddError("dns: creating recordset", err.Error())
		return
	}
	if err := waitForRecordSetActive(ctx, client, zoneID, rr.ID, defaultDNSTimeout); err != nil {
		resp.Diagnostics.AddError("dns: waiting for recordset to become active", err.Error())
		return
	}

	_, readDiags := r.readInto(ctx, client, zoneID, rr.ID, &plan)
	resp.Diagnostics.Append(readDiags...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *recordSetResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state recordSetModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := r.config.DNSV2Client()
	if err != nil {
		resp.Diagnostics.AddError("dns: building v2 client", err.Error())
		return
	}

	notFound, diags := r.readInto(ctx, client, state.ZoneID.ValueString(), state.ID.ValueString(), &state)
	if notFound {
		resp.Diagnostics.AddWarning("Recordset not found",
			fmt.Sprintf("Recordset %s no longer exists and was removed from state.", state.ID.ValueString()))
		resp.State.RemoveResource(ctx)
		return
	}
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *recordSetResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state recordSetModel
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

	zoneID := state.ZoneID.ValueString()
	desc := plan.Description.ValueString()
	updateOpts := recordsets.UpdateOpts{
		Description: &desc,
		Records:     setToStrings(ctx, plan.Records, &resp.Diagnostics),
	}
	if plan.TTL.ValueInt64() > 0 {
		ttl := int(plan.TTL.ValueInt64())
		updateOpts.TTL = &ttl
	}
	if resp.Diagnostics.HasError() {
		return
	}

	rrID := plan.ID.ValueString()
	if _, err := recordsets.Update(ctx, client, zoneID, rrID, updateOpts).Extract(); err != nil {
		resp.Diagnostics.AddError("dns: updating recordset", err.Error())
		return
	}
	if err := waitForRecordSetActive(ctx, client, zoneID, rrID, defaultDNSTimeout); err != nil {
		resp.Diagnostics.AddError("dns: waiting for recordset after update", err.Error())
		return
	}

	_, readDiags := r.readInto(ctx, client, zoneID, rrID, &plan)
	resp.Diagnostics.Append(readDiags...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *recordSetResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state recordSetModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := r.config.DNSV2Client()
	if err != nil {
		resp.Diagnostics.AddError("dns: building v2 client", err.Error())
		return
	}

	zoneID := state.ZoneID.ValueString()
	rrID := state.ID.ValueString()
	if err := recordsets.Delete(ctx, client, zoneID, rrID).ExtractErr(); err != nil {
		if gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
			return
		}
		resp.Diagnostics.AddError("dns: deleting recordset", err.Error())
		return
	}
	if err := waitForRecordSetDeleted(ctx, client, zoneID, rrID, defaultDNSTimeout); err != nil {
		resp.Diagnostics.AddError("dns: waiting for recordset deletion", err.Error())
	}
}

func (r *recordSetResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	zoneID, rrID, err := splitZoneChildID(req.ID)
	if err != nil {
		resp.Diagnostics.AddError("Invalid import ID", err.Error())
		return
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), rrID)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("zone_id"), zoneID)...)
}

func (r *recordSetResource) readInto(ctx context.Context, client *gophercloud.ServiceClient, zoneID, rrID string, m *recordSetModel) (notFound bool, diags diag.Diagnostics) {
	rr, err := recordsets.Get(ctx, client, zoneID, rrID).Extract()
	if err != nil {
		if gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
			return true, diags
		}
		diags.AddError("dns: reading recordset", err.Error())
		return false, diags
	}

	m.ID = types.StringValue(rr.ID)
	m.ZoneID = types.StringValue(rr.ZoneID)
	m.Name = types.StringValue(rr.Name)
	m.Type = types.StringValue(rr.Type)
	m.TTL = types.Int64Value(int64(rr.TTL))
	m.Description = types.StringValue(rr.Description)
	m.Status = types.StringValue(rr.Status)

	recVals := rr.Records
	if recVals == nil {
		recVals = []string{}
	}
	records, d := types.SetValueFrom(ctx, types.StringType, recVals)
	diags = append(diags, d...)
	m.Records = records

	if m.Region.IsNull() || m.Region.IsUnknown() {
		m.Region = types.StringValue(r.config.Region)
	}
	return false, diags
}
