// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0
//
// Ported from terraform-provider-openstack v3.4.0
// (openstack/resource_openstack_blockstorage_volume_type_access_v3.go), adapted
// for the terraform-plugin-framework and PCD. One resource is one project's
// access to one private volume type (POST /types/{id}/action addProjectAccess
// and removeProjectAccess; GET /types/{id}/os-volume-type-access).

package blockstorage

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/blockstorage/v3/volumetypes"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/platform9/terraform-provider-pcd/internal/clients"
)

var (
	_ resource.Resource                = (*volumeTypeAccessResource)(nil)
	_ resource.ResourceWithConfigure   = (*volumeTypeAccessResource)(nil)
	_ resource.ResourceWithImportState = (*volumeTypeAccessResource)(nil)
)

// NewVolumeTypeAccessResource is the factory registered with the provider.
func NewVolumeTypeAccessResource() resource.Resource {
	return &volumeTypeAccessResource{}
}

type volumeTypeAccessResource struct {
	config *clients.Config
}

type volumeTypeAccessModel struct {
	ID           types.String `tfsdk:"id"`
	VolumeTypeID types.String `tfsdk:"volume_type_id"`
	ProjectID    types.String `tfsdk:"project_id"`
	Region       types.String `tfsdk:"region"`
}

func (r *volumeTypeAccessResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_blockstorage_volume_type_access"
}

func (r *volumeTypeAccessResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	forceNew := []planmodifier.String{stringplanmodifier.RequiresReplace()}
	resp.Schema = schema.Schema{
		MarkdownDescription: "Grants one project access to one private Cinder volume type (admin only), the PCD UI's " +
			"tenant list on a volume type. Use one resource per type and project pair, with `for_each` for a list " +
			"of projects. Cinder does not give even the project that created a private type access to it, so a " +
			"type with `is_public = false` is usable only by the projects granted here. The type must be private: " +
			"Cinder refuses a grant on a public type. The resource is not authoritative: grants made outside " +
			"Terraform for other projects are left alone. A grant that already exists when the resource is created " +
			"is adopted, so destroying the resource revokes it.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{Computed: true,
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
				MarkdownDescription: "The composite `<volume_type_id>/<project_id>` ID."},
			"volume_type_id": schema.StringAttribute{Required: true, PlanModifiers: forceNew,
				MarkdownDescription: "The UUID of a private volume type (`pcd_blockstorage_volume_type.<name>.id`); " +
					"Cinder looks the type up by ID only, so a name is not found. Changing this forces a new resource."},
			"project_id": schema.StringAttribute{Required: true, PlanModifiers: forceNew,
				MarkdownDescription: "The ID of the project to grant access to. Changing this forces a new resource."},
			"region": schema.StringAttribute{Optional: true, Computed: true,
				PlanModifiers:       []planmodifier.String{stringplanmodifier.RequiresReplace(), stringplanmodifier.UseStateForUnknown()},
				MarkdownDescription: "The region. Defaults to the provider's region. Changing this forces a new resource."},
		},
	}
}

func (r *volumeTypeAccessResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.config = configureClient(req.ProviderData, &resp.Diagnostics)
}

func (r *volumeTypeAccessResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan volumeTypeAccessModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := r.config.ForRegion(plan.Region.ValueString()).BlockStorageV3Client()
	if err != nil {
		resp.Diagnostics.AddError("blockstorage: building v3 client", err.Error())
		return
	}

	typeID := plan.VolumeTypeID.ValueString()
	projectID := plan.ProjectID.ValueString()
	err = volumetypes.AddAccess(ctx, client, typeID, volumetypes.AddAccessOpts{Project: projectID}).ExtractErr()
	switch {
	case err == nil:
	case gophercloud.ResponseCodeIs(err, http.StatusConflict):
		// VolumeTypeAccessExists: the grant is already there, so it is adopted.
	case gophercloud.ResponseCodeIs(err, http.StatusBadRequest):
		resp.Diagnostics.AddAttributeError(path.Root("volume_type_id"), "Volume type access refused",
			fmt.Sprintf("Cinder refused to grant project %s access to volume type %s (400). Cinder refuses this "+
				"for a public type; set is_public = false on the type. Cinder said: %v", projectID, typeID, err))
		return
	default:
		resp.Diagnostics.AddError("blockstorage: granting volume type access", err.Error())
		return
	}

	plan.ID = types.StringValue(typeID + "/" + projectID)
	if plan.Region.IsNull() || plan.Region.IsUnknown() {
		plan.Region = types.StringValue(r.config.Region)
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *volumeTypeAccessResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state volumeTypeAccessModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := r.config.ForRegion(state.Region.ValueString()).BlockStorageV3Client()
	if err != nil {
		resp.Diagnostics.AddError("blockstorage: building v3 client", err.Error())
		return
	}

	typeID := state.VolumeTypeID.ValueString()
	projectID := state.ProjectID.ValueString()
	found, err := volumeTypeAccessExists(ctx, client, typeID, projectID)
	if err != nil {
		resp.Diagnostics.AddError("blockstorage: listing volume type access", err.Error())
		return
	}
	if !found {
		resp.Diagnostics.AddWarning("Volume type access not found",
			fmt.Sprintf("Project %s no longer has access to volume type %s (revoked, the type was deleted, or it is "+
				"now public); it was removed from state.", projectID, typeID))
		resp.State.RemoveResource(ctx)
		return
	}

	state.ID = types.StringValue(typeID + "/" + projectID)
	if state.Region.IsNull() || state.Region.IsUnknown() {
		state.Region = types.StringValue(r.config.Region)
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

// Update is unreachable: every attribute forces replacement.
func (r *volumeTypeAccessResource) Update(_ context.Context, _ resource.UpdateRequest, resp *resource.UpdateResponse) {
	resp.Diagnostics.AddError("blockstorage: volume type access update",
		"volume_type_id, project_id and region force replacement; this should not be reached.")
}

func (r *volumeTypeAccessResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state volumeTypeAccessModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := r.config.ForRegion(state.Region.ValueString()).BlockStorageV3Client()
	if err != nil {
		resp.Diagnostics.AddError("blockstorage: building v3 client", err.Error())
		return
	}

	typeID := state.VolumeTypeID.ValueString()
	projectID := state.ProjectID.ValueString()
	err = volumetypes.RemoveAccess(ctx, client, typeID, volumetypes.RemoveAccessOpts{Project: projectID}).ExtractErr()
	switch {
	case err == nil, gophercloud.ResponseCodeIs(err, http.StatusNotFound):
	case gophercloud.ResponseCodeIs(err, http.StatusBadRequest):
		// Cinder answers 400 for a type that became public: there is no access
		// list left to remove the project from.
		resp.Diagnostics.AddWarning("Volume type is public",
			fmt.Sprintf("Cinder refused to revoke project %s's access to volume type %s (400), which it does once "+
				"the type is public; every project can use a public type. The grant was removed from state. "+
				"Cinder said: %v", projectID, typeID, err))
	default:
		resp.Diagnostics.AddError("blockstorage: revoking volume type access", err.Error())
	}
}

func (r *volumeTypeAccessResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	typeID, projectID, err := splitVolumeTypeAccessID(req.ID)
	if err != nil {
		resp.Diagnostics.AddError("Invalid import ID", err.Error())
		return
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), req.ID)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("volume_type_id"), typeID)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("project_id"), projectID)...)
}

// splitVolumeTypeAccessID parses "<volume_type_id>/<project_id>".
func splitVolumeTypeAccessID(id string) (typeID, projectID string, err error) {
	parts := strings.SplitN(id, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("expected <volume_type_id>/<project_id>, got %q", id)
	}
	return parts[0], parts[1], nil
}

// volumeTypeAccessExists reports whether projectID is on the type's access
// list. A 404 means the type is gone or public (Cinder keeps no list for a
// public type) and counts as not found; any other error is returned.
func volumeTypeAccessExists(ctx context.Context, client *gophercloud.ServiceClient, typeID, projectID string) (bool, error) {
	pages, err := volumetypes.ListAccesses(client, typeID).AllPages(ctx)
	if err != nil {
		if gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
			return false, nil
		}
		return false, err
	}
	accesses, err := volumetypes.ExtractAccesses(pages)
	if err != nil {
		return false, err
	}
	for _, a := range accesses {
		if a.ProjectID == projectID {
			return true, nil
		}
	}
	return false, nil
}
