// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0
//
// Ported from terraform-provider-openstack v3.4.0
// (openstack/resource_openstack_blockstorage_volume_type_v3.go), adapted for the
// terraform-plugin-framework and PCD.

package blockstorage

import (
	"context"
	"net/http"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/blockstorage/v3/volumetypes"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/platform9/terraform-provider-pcd/internal/clients"
)

var (
	_ resource.Resource                = (*volumeTypeResource)(nil)
	_ resource.ResourceWithConfigure   = (*volumeTypeResource)(nil)
	_ resource.ResourceWithImportState = (*volumeTypeResource)(nil)
)

// NewVolumeTypeResource is the factory registered with the provider.
func NewVolumeTypeResource() resource.Resource {
	return &volumeTypeResource{}
}

type volumeTypeResource struct {
	config *clients.Config
}

type volumeTypeModel struct {
	ID          types.String `tfsdk:"id"`
	Name        types.String `tfsdk:"name"`
	Description types.String `tfsdk:"description"`
	IsPublic    types.Bool   `tfsdk:"is_public"`
	ExtraSpecs  types.Map    `tfsdk:"extra_specs"`
	Region      types.String `tfsdk:"region"`
}

func (r *volumeTypeResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_blockstorage_volume_type"
}

func (r *volumeTypeResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	useState := []planmodifier.String{stringplanmodifier.UseStateForUnknown()}
	resp.Schema = schema.Schema{
		MarkdownDescription: "Manages a Cinder volume type in PCD. Volume types describe a storage tier and carry " +
			"backend-selection `extra_specs`.",
		Attributes: map[string]schema.Attribute{
			"id":          schema.StringAttribute{Computed: true, MarkdownDescription: "The volume type ID.", PlanModifiers: useState},
			"name":        schema.StringAttribute{Required: true, MarkdownDescription: "The name of the volume type."},
			"description": schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "A description of the volume type.", PlanModifiers: useState},
			"is_public":   schema.BoolAttribute{Optional: true, Computed: true, Default: booldefault.StaticBool(true), MarkdownDescription: "Whether the volume type is visible to all projects."},
			"extra_specs": schema.MapAttribute{Optional: true, Computed: true, ElementType: types.StringType, MarkdownDescription: "Key-value backend specs (e.g. `volume_backend_name`)."},
			"region":      schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "The region. Defaults to the provider's region.", PlanModifiers: useState},
		},
	}
}

func (r *volumeTypeResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.config = configureClient(req.ProviderData, &resp.Diagnostics)
}

func (r *volumeTypeResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan volumeTypeModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := r.config.BlockStorageV3Client()
	if err != nil {
		resp.Diagnostics.AddError("blockstorage: building v3 client", err.Error())
		return
	}

	isPublic := plan.IsPublic.ValueBool()
	createOpts := volumetypes.CreateOpts{
		Name:        plan.Name.ValueString(),
		Description: plan.Description.ValueString(),
		IsPublic:    &isPublic,
		ExtraSpecs:  mapToStrings(ctx, plan.ExtraSpecs, &resp.Diagnostics),
	}
	if resp.Diagnostics.HasError() {
		return
	}

	vt, err := volumetypes.Create(ctx, client, createOpts).Extract()
	if err != nil {
		resp.Diagnostics.AddError("blockstorage: creating volume type", err.Error())
		return
	}

	// Build state from the create response (which echoes the type) rather than a
	// second Get, so a transient read failure can't orphan the created type.
	resp.Diagnostics.Append(r.setState(ctx, &plan, vt)...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *volumeTypeResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state volumeTypeModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := r.config.BlockStorageV3Client()
	if err != nil {
		resp.Diagnostics.AddError("blockstorage: building v3 client", err.Error())
		return
	}

	_, err = volumetypes.Get(ctx, client, state.ID.ValueString()).Extract()
	if err != nil {
		if gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
			resp.State.RemoveResource(ctx)
			return
		}
		resp.Diagnostics.AddError("blockstorage: reading volume type", err.Error())
		return
	}
	resp.Diagnostics.Append(r.readInto(ctx, client, state.ID.ValueString(), &state)...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *volumeTypeResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state volumeTypeModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := r.config.BlockStorageV3Client()
	if err != nil {
		resp.Diagnostics.AddError("blockstorage: building v3 client", err.Error())
		return
	}

	id := plan.ID.ValueString()
	if !plan.Name.Equal(state.Name) || !plan.Description.Equal(state.Description) || !plan.IsPublic.Equal(state.IsPublic) {
		name := plan.Name.ValueString()
		desc := plan.Description.ValueString()
		isPublic := plan.IsPublic.ValueBool()
		if _, err := volumetypes.Update(ctx, client, id, volumetypes.UpdateOpts{
			Name: &name, Description: &desc, IsPublic: &isPublic,
		}).Extract(); err != nil {
			resp.Diagnostics.AddError("blockstorage: updating volume type", err.Error())
			return
		}
	}

	if !plan.ExtraSpecs.Equal(state.ExtraSpecs) {
		resp.Diagnostics.Append(r.syncExtraSpecs(ctx, client, id, plan.ExtraSpecs, state.ExtraSpecs)...)
		if resp.Diagnostics.HasError() {
			return
		}
	}

	resp.Diagnostics.Append(r.readInto(ctx, client, id, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *volumeTypeResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state volumeTypeModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := r.config.BlockStorageV3Client()
	if err != nil {
		resp.Diagnostics.AddError("blockstorage: building v3 client", err.Error())
		return
	}

	if err := volumetypes.Delete(ctx, client, state.ID.ValueString()).ExtractErr(); err != nil {
		if gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
			return
		}
		resp.Diagnostics.AddError("blockstorage: deleting volume type", err.Error())
	}
}

func (r *volumeTypeResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, path.Root("id"), req, resp)
}

// syncExtraSpecs adds/updates the specs present in plan and removes those that
// state had but plan no longer does.
func (r *volumeTypeResource) syncExtraSpecs(ctx context.Context, client *gophercloud.ServiceClient, id string, plan, state types.Map) diag.Diagnostics {
	var diags diag.Diagnostics
	planSpecs := mapToStrings(ctx, plan, &diags)
	stateSpecs := mapToStrings(ctx, state, &diags)
	if diags.HasError() {
		return diags
	}

	if len(planSpecs) > 0 {
		if _, err := volumetypes.CreateExtraSpecs(ctx, client, id, volumetypes.ExtraSpecsOpts(planSpecs)).Extract(); err != nil {
			diags.AddError("blockstorage: updating volume type extra_specs", err.Error())
			return diags
		}
	}
	for k := range stateSpecs {
		if _, ok := planSpecs[k]; ok {
			continue
		}
		if err := volumetypes.DeleteExtraSpec(ctx, client, id, k).ExtractErr(); err != nil {
			diags.AddError("blockstorage: removing volume type extra_spec", err.Error())
			return diags
		}
	}
	return diags
}

func (r *volumeTypeResource) readInto(ctx context.Context, client *gophercloud.ServiceClient, id string, m *volumeTypeModel) diag.Diagnostics {
	vt, err := volumetypes.Get(ctx, client, id).Extract()
	if err != nil {
		var diags diag.Diagnostics
		diags.AddError("blockstorage: reading volume type", err.Error())
		return diags
	}
	return r.setState(ctx, m, vt)
}

func (r *volumeTypeResource) setState(ctx context.Context, m *volumeTypeModel, vt *volumetypes.VolumeType) diag.Diagnostics {
	var diags diag.Diagnostics
	m.ID = types.StringValue(vt.ID)
	m.Name = types.StringValue(vt.Name)
	m.Description = types.StringValue(vt.Description)
	m.IsPublic = types.BoolValue(vt.IsPublic)

	specs := vt.ExtraSpecs
	if specs == nil {
		specs = map[string]string{}
	}
	sv, d := types.MapValueFrom(ctx, types.StringType, specs)
	diags = append(diags, d...)
	m.ExtraSpecs = sv

	if m.Region.IsNull() || m.Region.IsUnknown() {
		m.Region = types.StringValue(r.config.Region)
	}
	return diags
}

// mapToStrings converts a Map attribute to a Go map (nil for null/unknown).
func mapToStrings(ctx context.Context, m types.Map, diags *diag.Diagnostics) map[string]string {
	if m.IsNull() || m.IsUnknown() {
		return nil
	}
	out := map[string]string{}
	diags.Append(m.ElementsAs(ctx, &out, false)...)
	return out
}
