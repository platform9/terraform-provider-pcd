// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0
//
// Ported from terraform-provider-openstack v3.4.0
// (openstack/resource_openstack_compute_flavor_v2.go), adapted for the
// terraform-plugin-framework and PCD.

package compute

import (
	"context"
	"fmt"
	"net/http"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/flavors"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/boolplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/float64planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/int64default"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/int64planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/platform9/terraform-provider-pcd/internal/clients"
)

var (
	_ resource.Resource                = (*flavorResource)(nil)
	_ resource.ResourceWithConfigure   = (*flavorResource)(nil)
	_ resource.ResourceWithImportState = (*flavorResource)(nil)
)

// NewFlavorResource is the factory registered with the provider.
func NewFlavorResource() resource.Resource {
	return &flavorResource{}
}

type flavorResource struct {
	config *clients.Config
}

type flavorModel struct {
	ID         types.String  `tfsdk:"id"`
	Name       types.String  `tfsdk:"name"`
	RAM        types.Int64   `tfsdk:"ram"`
	VCPUs      types.Int64   `tfsdk:"vcpus"`
	Disk       types.Int64   `tfsdk:"disk"`
	FlavorID   types.String  `tfsdk:"flavor_id"`
	Swap       types.Int64   `tfsdk:"swap"`
	RxTxFactor types.Float64 `tfsdk:"rx_tx_factor"`
	IsPublic   types.Bool    `tfsdk:"is_public"`
	Ephemeral  types.Int64   `tfsdk:"ephemeral"`
	ExtraSpecs types.Map     `tfsdk:"extra_specs"`
	Region     types.String  `tfsdk:"region"`
}

func (r *flavorResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_compute_flavor"
}

func (r *flavorResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	fn := []planmodifier.String{stringplanmodifier.RequiresReplace()}
	fnC := []planmodifier.String{stringplanmodifier.RequiresReplace(), stringplanmodifier.UseStateForUnknown()}
	fnInt := []planmodifier.Int64{int64planmodifier.RequiresReplace()}
	resp.Schema = schema.Schema{
		MarkdownDescription: "Manages a compute flavor in PCD's Nova service (admin). Every attribute except " +
			"`extra_specs` is immutable; changing one forces a new resource. `extra_specs` can be added, " +
			"changed, or removed in place.",
		Attributes: map[string]schema.Attribute{
			"id":           schema.StringAttribute{Computed: true, MarkdownDescription: "The flavor ID.", PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()}},
			"name":         schema.StringAttribute{Required: true, MarkdownDescription: "The name of the flavor. Changing this forces a new resource.", PlanModifiers: fn},
			"ram":          schema.Int64Attribute{Required: true, MarkdownDescription: "Memory in MB. Changing this forces a new resource.", PlanModifiers: fnInt},
			"vcpus":        schema.Int64Attribute{Required: true, MarkdownDescription: "Number of vCPUs. Changing this forces a new resource.", PlanModifiers: fnInt},
			"disk":         schema.Int64Attribute{Required: true, MarkdownDescription: "Root disk in GB. Changing this forces a new resource.", PlanModifiers: fnInt},
			"flavor_id":    schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "The desired flavor ID (auto-generated if omitted). Changing this forces a new resource.", PlanModifiers: fnC},
			"swap":         schema.Int64Attribute{Optional: true, Computed: true, Default: int64default.StaticInt64(0), MarkdownDescription: "Swap space in MB. Changing this forces a new resource.", PlanModifiers: fnInt},
			"rx_tx_factor": schema.Float64Attribute{Optional: true, Computed: true, MarkdownDescription: "RX/TX factor. Changing this forces a new resource.", PlanModifiers: []planmodifier.Float64{float64planmodifier.RequiresReplace(), float64planmodifier.UseStateForUnknown()}},
			"is_public":    schema.BoolAttribute{Optional: true, Computed: true, Default: booldefault.StaticBool(true), MarkdownDescription: "Whether the flavor is public. Changing this forces a new resource.", PlanModifiers: []planmodifier.Bool{boolplanmodifier.RequiresReplace()}},
			"ephemeral":    schema.Int64Attribute{Optional: true, Computed: true, Default: int64default.StaticInt64(0), MarkdownDescription: "Ephemeral disk in GB. Changing this forces a new resource.", PlanModifiers: fnInt},
			"extra_specs":  schema.MapAttribute{Optional: true, Computed: true, ElementType: types.StringType, MarkdownDescription: "Key/value extra specs (e.g. `hw:cpu_policy`). Can be added, changed, or removed on an existing flavor without replacing it."},
			"region":       schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "The region. Defaults to the provider's region.", PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()}},
		},
	}
}

func (r *flavorResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.config = configureClient(req.ProviderData, &resp.Diagnostics)
}

func (r *flavorResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan flavorModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := r.config.ComputeV2Client()
	if err != nil {
		resp.Diagnostics.AddError("compute: building v2 client", err.Error())
		return
	}

	disk := int(plan.Disk.ValueInt64())
	swap := int(plan.Swap.ValueInt64())
	ephemeral := int(plan.Ephemeral.ValueInt64())
	isPublic := plan.IsPublic.ValueBool()
	createOpts := flavors.CreateOpts{
		Name:       plan.Name.ValueString(),
		RAM:        int(plan.RAM.ValueInt64()),
		VCPUs:      int(plan.VCPUs.ValueInt64()),
		Disk:       &disk,
		ID:         plan.FlavorID.ValueString(),
		Swap:       &swap,
		RxTxFactor: plan.RxTxFactor.ValueFloat64(),
		IsPublic:   &isPublic,
		Ephemeral:  &ephemeral,
	}

	flavor, err := flavors.Create(ctx, client, createOpts).Extract()
	if err != nil {
		resp.Diagnostics.AddError("compute: creating flavor", err.Error())
		return
	}

	if specs := extractStringMap(ctx, plan.ExtraSpecs, &resp.Diagnostics); len(specs) > 0 {
		if resp.Diagnostics.HasError() {
			return
		}
		if _, err := flavors.CreateExtraSpecs(ctx, client, flavor.ID, flavors.ExtraSpecsOpts(specs)).Extract(); err != nil {
			resp.Diagnostics.AddError("compute: setting flavor extra specs", err.Error())
			return
		}
	}

	r.flatten(flavor, &plan)
	resp.Diagnostics.Append(r.refreshExtraSpecs(ctx, client, flavor.ID, &plan)...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *flavorResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state flavorModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := r.config.ComputeV2Client()
	if err != nil {
		resp.Diagnostics.AddError("compute: building v2 client", err.Error())
		return
	}

	flavor, err := flavors.Get(ctx, client, state.ID.ValueString()).Extract()
	if err != nil {
		if gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
			resp.Diagnostics.AddWarning("Flavor not found",
				fmt.Sprintf("Flavor %s no longer exists and was removed from state.", state.ID.ValueString()))
			resp.State.RemoveResource(ctx)
			return
		}
		resp.Diagnostics.AddError("compute: reading flavor", err.Error())
		return
	}

	r.flatten(flavor, &state)
	resp.Diagnostics.Append(r.refreshExtraSpecs(ctx, client, state.ID.ValueString(), &state)...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

// Update reconciles extra_specs — the only in-place-mutable attribute (every
// other flavor field forces replacement).
func (r *flavorResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state flavorModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := r.config.ComputeV2Client()
	if err != nil {
		resp.Diagnostics.AddError("compute: building v2 client", err.Error())
		return
	}

	oldSpecs := extractStringMap(ctx, state.ExtraSpecs, &resp.Diagnostics)
	newSpecs := extractStringMap(ctx, plan.ExtraSpecs, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}

	id := plan.ID.ValueString()
	for k := range oldSpecs {
		if _, ok := newSpecs[k]; !ok {
			if err := flavors.DeleteExtraSpec(ctx, client, id, k).ExtractErr(); err != nil {
				resp.Diagnostics.AddError("compute: deleting flavor extra spec", err.Error())
				return
			}
		}
	}
	changed := map[string]string{}
	for k, v := range newSpecs {
		if oldSpecs[k] != v {
			changed[k] = v
		}
	}
	if len(changed) > 0 {
		if _, err := flavors.CreateExtraSpecs(ctx, client, id, flavors.ExtraSpecsOpts(changed)).Extract(); err != nil {
			resp.Diagnostics.AddError("compute: updating flavor extra specs", err.Error())
			return
		}
	}

	resp.Diagnostics.Append(r.refreshExtraSpecs(ctx, client, id, &plan)...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *flavorResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state flavorModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := r.config.ComputeV2Client()
	if err != nil {
		resp.Diagnostics.AddError("compute: building v2 client", err.Error())
		return
	}

	if err := flavors.Delete(ctx, client, state.ID.ValueString()).ExtractErr(); err != nil {
		if gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
			return
		}
		resp.Diagnostics.AddError("compute: deleting flavor", err.Error())
	}
}

func (r *flavorResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, path.Root("id"), req, resp)
}

// refreshExtraSpecs lists the flavor's extra specs and writes them onto the
// model as an (always non-null) map, so state reflects the server.
func (r *flavorResource) refreshExtraSpecs(ctx context.Context, client *gophercloud.ServiceClient, id string, m *flavorModel) diag.Diagnostics {
	var diags diag.Diagnostics
	specs, err := flavors.ListExtraSpecs(ctx, client, id).Extract()
	if err != nil {
		diags.AddError("compute: listing flavor extra specs", err.Error())
		return diags
	}
	if specs == nil {
		specs = map[string]string{}
	}
	sm, d := types.MapValueFrom(ctx, types.StringType, specs)
	diags.Append(d...)
	m.ExtraSpecs = sm
	return diags
}

func (r *flavorResource) flatten(flavor *flavors.Flavor, m *flavorModel) {
	m.ID = types.StringValue(flavor.ID)
	m.FlavorID = types.StringValue(flavor.ID)
	m.Name = types.StringValue(flavor.Name)
	m.RAM = types.Int64Value(int64(flavor.RAM))
	m.VCPUs = types.Int64Value(int64(flavor.VCPUs))
	m.Disk = types.Int64Value(int64(flavor.Disk))
	m.Swap = types.Int64Value(int64(flavor.Swap))
	m.RxTxFactor = types.Float64Value(flavor.RxTxFactor)
	m.IsPublic = types.BoolValue(flavor.IsPublic)
	m.Ephemeral = types.Int64Value(int64(flavor.Ephemeral))
	if m.Region.IsNull() || m.Region.IsUnknown() {
		m.Region = types.StringValue(r.config.Region)
	}
}
