// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0
//
// Ported from terraform-provider-openstack v3.4.0
// (openstack/resource_openstack_blockstorage_volume_v3.go), adapted for the
// terraform-plugin-framework and PCD.

package blockstorage

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/blockstorage/v3/volumes"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringdefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/platform9/terraform-provider-pcd/internal/clients"
	"github.com/platform9/terraform-provider-pcd/internal/tfstate"
)

var (
	_ resource.Resource                = (*volumeResource)(nil)
	_ resource.ResourceWithConfigure   = (*volumeResource)(nil)
	_ resource.ResourceWithImportState = (*volumeResource)(nil)
)

// NewVolumeResource is the factory registered with the provider.
func NewVolumeResource() resource.Resource {
	return &volumeResource{}
}

type volumeResource struct {
	config *clients.Config
}

type volumeModel struct {
	ID               types.String `tfsdk:"id"`
	Name             types.String `tfsdk:"name"`
	Size             types.Int64  `tfsdk:"size"`
	Description      types.String `tfsdk:"description"`
	VolumeType       types.String `tfsdk:"volume_type"`
	AvailabilityZone types.String `tfsdk:"availability_zone"`
	SnapshotID       types.String `tfsdk:"snapshot_id"`
	SourceVolID      types.String `tfsdk:"source_vol_id"`
	ImageID          types.String `tfsdk:"image_id"`
	Metadata         types.Map    `tfsdk:"metadata"`
	Bootable         types.Bool   `tfsdk:"bootable"`
	Encrypted        types.Bool   `tfsdk:"encrypted"`
	Status           types.String `tfsdk:"status"`
	Region           types.String `tfsdk:"region"`

	VolumeRetypePolicy types.String `tfsdk:"volume_retype_policy"`
}

func (r *volumeResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_blockstorage_volume"
}

func (r *volumeResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	stable := []planmodifier.String{stringplanmodifier.UseStateForUnknown()}
	// UseStateForUnknown runs first: on an in-place update the framework marks
	// an unset availability_zone unknown, and RequiresReplace would report
	// that as a change, which Terraform then discards.
	fnC := []planmodifier.String{stringplanmodifier.UseStateForUnknown(), stringplanmodifier.RequiresReplace()}
	fn := []planmodifier.String{stringplanmodifier.RequiresReplace()}
	resp.Schema = schema.Schema{
		MarkdownDescription: "Manages a block storage volume in PCD's Cinder service.",
		Attributes: map[string]schema.Attribute{
			"id":                schema.StringAttribute{Computed: true, MarkdownDescription: "The volume ID.", PlanModifiers: stable},
			"name":              schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "The name of the volume.", PlanModifiers: stable},
			"size":              schema.Int64Attribute{Required: true, MarkdownDescription: "Size in GB. Increasing extends the volume in place; decreasing is not allowed."},
			"description":       schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "A description of the volume.", PlanModifiers: stable},
			"availability_zone": schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "Availability zone. Changing this forces a new resource.", PlanModifiers: fnC},
			"snapshot_id":       schema.StringAttribute{Optional: true, MarkdownDescription: "Create the volume from this snapshot. Changing this forces a new resource.", PlanModifiers: fn},
			"source_vol_id":     schema.StringAttribute{Optional: true, MarkdownDescription: "Clone the volume from this source volume. Changing this forces a new resource.", PlanModifiers: fn},
			"image_id":          schema.StringAttribute{Optional: true, MarkdownDescription: "Create the volume from this image. Changing this forces a new resource.", PlanModifiers: fn},
			"metadata":          schema.MapAttribute{Optional: true, Computed: true, ElementType: types.StringType, MarkdownDescription: "Key-value metadata for the volume."},
			"bootable":          schema.BoolAttribute{Computed: true, MarkdownDescription: "Whether the volume is bootable."},
			"encrypted":         schema.BoolAttribute{Computed: true, MarkdownDescription: "Whether the volume is encrypted."},
			"status":            schema.StringAttribute{Computed: true, MarkdownDescription: "The Cinder status (e.g. available)."},
			"region":            schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "The region. Defaults to the provider's region. Changing this forces a new resource.", PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace(), stringplanmodifier.UseStateForUnknown()}},
			"volume_type": schema.StringAttribute{
				Optional: true, Computed: true, PlanModifiers: stable,
				Validators: []validator.String{stringvalidator.LengthAtLeast(1)},
				MarkdownDescription: "The volume type, by name. Changing it retypes the volume in place (Cinder " +
					"`os-retype`); `volume_retype_policy` decides whether Cinder may move the data to another storage " +
					"backend to do so. Use the type's name, not its ID: Cinder reports the name, so an ID never matches " +
					"what the provider reads back. Cinder refuses to retype a volume that has snapshots, that is " +
					"attached and whose new type changes front-end QoS, or that is not `available` or `in-use`. " +
					"Retyping an attached volume that has to move to another backend depends on the storage driver.",
			},
			"volume_retype_policy": schema.StringAttribute{
				Optional: true, Computed: true,
				Default:    stringdefault.StaticString("never"),
				Validators: []validator.String{stringvalidator.OneOf("never", "on-demand")},
				MarkdownDescription: "Whether Cinder may migrate the volume's data to another storage backend when " +
					"`volume_type` changes: `never` (the default) or `on-demand`. With `never`, a retype to a type on " +
					"another backend fails and the volume keeps its type; the PCD UI always sends `on-demand`. " +
					"The value is used only for a retype: changing it alone never retypes the volume.",
			},
		},
	}
}

func (r *volumeResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.config = configureClient(req.ProviderData, &resp.Diagnostics)
}

func (r *volumeResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan volumeModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := r.config.ForRegion(plan.Region.ValueString()).BlockStorageV3Client()
	if err != nil {
		resp.Diagnostics.AddError("blockstorage: building v3 client", err.Error())
		return
	}

	var meta map[string]string
	if !plan.Metadata.IsNull() && !plan.Metadata.IsUnknown() {
		resp.Diagnostics.Append(plan.Metadata.ElementsAs(ctx, &meta, false)...)
		if resp.Diagnostics.HasError() {
			return
		}
	}

	createOpts := volumes.CreateOpts{
		Name:             plan.Name.ValueString(),
		Size:             int(plan.Size.ValueInt64()),
		Description:      plan.Description.ValueString(),
		VolumeType:       plan.VolumeType.ValueString(),
		AvailabilityZone: plan.AvailabilityZone.ValueString(),
		SnapshotID:       plan.SnapshotID.ValueString(),
		SourceVolID:      plan.SourceVolID.ValueString(),
		ImageID:          plan.ImageID.ValueString(),
		Metadata:         meta,
	}

	vol, err := volumes.Create(ctx, client, createOpts, nil).Extract()
	if err != nil {
		resp.Diagnostics.AddError("blockstorage: creating volume", err.Error())
		return
	}

	plan.ID = types.StringValue(vol.ID)
	if !tfstate.RecordCreated(ctx, resp, &plan) {
		return
	}

	if _, err := waitForVolumeStatus(ctx, client, vol.ID, "available", 20*time.Minute); err != nil {
		resp.Diagnostics.AddError("blockstorage: waiting for volume to become available", err.Error())
		return
	}

	final, err := volumes.Get(ctx, client, vol.ID).Extract()
	if err != nil {
		resp.Diagnostics.AddError("blockstorage: reading volume after create", err.Error())
		return
	}
	resp.Diagnostics.Append(r.flatten(ctx, final, &plan)...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *volumeResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state volumeModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := r.config.ForRegion(state.Region.ValueString()).BlockStorageV3Client()
	if err != nil {
		resp.Diagnostics.AddError("blockstorage: building v3 client", err.Error())
		return
	}

	vol, err := volumes.Get(ctx, client, state.ID.ValueString()).Extract()
	if err != nil {
		if gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
			resp.Diagnostics.AddWarning("Volume not found",
				fmt.Sprintf("Volume %s no longer exists and was removed from state.", state.ID.ValueString()))
			resp.State.RemoveResource(ctx)
			return
		}
		resp.Diagnostics.AddError("blockstorage: reading volume", err.Error())
		return
	}

	resp.Diagnostics.Append(r.flatten(ctx, vol, &state)...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *volumeResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state volumeModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := r.config.ForRegion(plan.Region.ValueString()).BlockStorageV3Client()
	if err != nil {
		resp.Diagnostics.AddError("blockstorage: building v3 client", err.Error())
		return
	}

	if !plan.Name.Equal(state.Name) || !plan.Description.Equal(state.Description) || !plan.Metadata.Equal(state.Metadata) {
		name := plan.Name.ValueString()
		description := plan.Description.ValueString()
		updateOpts := volumeUpdate{UpdateOpts: volumes.UpdateOpts{Name: &name, Description: &description}}
		if !plan.Metadata.IsNull() && !plan.Metadata.IsUnknown() {
			meta := map[string]string{}
			resp.Diagnostics.Append(plan.Metadata.ElementsAs(ctx, &meta, false)...)
			if resp.Diagnostics.HasError() {
				return
			}
			updateOpts.metadata = meta
		}
		if _, err := volumes.Update(ctx, client, plan.ID.ValueString(), updateOpts).Extract(); err != nil {
			resp.Diagnostics.AddError("blockstorage: updating volume", err.Error())
			return
		}
	}

	if plan.Size.ValueInt64() > state.Size.ValueInt64() {
		if err := extendVolume(ctx, client, plan.ID.ValueString(), int(plan.Size.ValueInt64())); err != nil {
			resp.Diagnostics.AddError("blockstorage: extending volume", err.Error())
			return
		}
		if _, err := waitForVolumeStatus(ctx, client, plan.ID.ValueString(), "available", 20*time.Minute); err != nil {
			resp.Diagnostics.AddError("blockstorage: waiting for volume after extend", err.Error())
			return
		}
	} else if plan.Size.ValueInt64() < state.Size.ValueInt64() {
		resp.Diagnostics.AddError("Invalid volume size", "Volumes cannot be shrunk; size may only be increased.")
		return
	}

	if newType, ok := retypeTarget(&plan, &state); ok {
		policy := volumes.MigrationPolicy(plan.VolumeRetypePolicy.ValueString())
		vol, err := retypeVolume(ctx, client, plan.ID.ValueString(), newType, policy)
		if err != nil {
			detail := err.Error()
			if gophercloud.ResponseCodeIs(err, http.StatusBadRequest) && strings.Contains(detail, "same volume type") {
				detail += "\n\nThe volume already has this type. If volume_type is a volume type ID, set it to the " +
					"type's name: Cinder reports types by name, so an ID never matches."
			}
			resp.Diagnostics.AddError("blockstorage: retyping volume", detail)
			return
		}
		if vol.VolumeType != newType {
			// Cinder accepted the retype and then kept the old type. Record the
			// volume as it is, so state does not claim a type it does not have.
			resp.Diagnostics.Append(r.flatten(ctx, vol, &plan)...)
			resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
			resp.Diagnostics.AddError("blockstorage: retype did not take effect", fmt.Sprintf(
				"Cinder accepted the retype of volume %s to %q, but the volume still has type %q (status %q). "+
					"Cinder keeps the old type when the scheduler or the storage driver cannot complete a retype, "+
					"or when the retype needs a data migration that volume_retype_policy = \"never\" forbids. "+
					"If the new type is on another storage backend, set volume_retype_policy = \"on-demand\". "+
					"Cinder's user messages for the volume may give the reason. If volume_type is a type ID "+
					"rather than a name, set it to the type's name: Cinder reports types by name.",
				plan.ID.ValueString(), newType, vol.VolumeType, vol.Status))
			return
		}
	}

	vol, err := volumes.Get(ctx, client, plan.ID.ValueString()).Extract()
	if err != nil {
		resp.Diagnostics.AddError("blockstorage: reading volume after update", err.Error())
		return
	}
	resp.Diagnostics.Append(r.flatten(ctx, vol, &plan)...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

// volumeUpdate is a volume update whose metadata, when set, is sent even if
// empty, so metadata = {} clears it: volumes.UpdateOpts drops an empty map.
type volumeUpdate struct {
	volumes.UpdateOpts
	metadata map[string]string
}

func (o volumeUpdate) ToVolumeUpdateMap() (map[string]any, error) {
	b, err := o.UpdateOpts.ToVolumeUpdateMap()
	if err != nil || o.metadata == nil {
		return b, err
	}
	v, ok := b["volume"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("building the volume update: unexpected body %v", b)
	}
	v["metadata"] = o.metadata
	return b, nil
}

func (r *volumeResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state volumeModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := r.config.ForRegion(state.Region.ValueString()).BlockStorageV3Client()
	if err != nil {
		resp.Diagnostics.AddError("blockstorage: building v3 client", err.Error())
		return
	}

	if err := volumes.Delete(ctx, client, state.ID.ValueString(), volumes.DeleteOpts{}).ExtractErr(); err != nil {
		if gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
			return
		}
		resp.Diagnostics.AddError("blockstorage: deleting volume", err.Error())
		return
	}
	if err := waitForVolumeDeleted(ctx, client, state.ID.ValueString(), 10*time.Minute); err != nil {
		resp.Diagnostics.AddError("blockstorage: waiting for volume deletion", err.Error())
	}
}

func (r *volumeResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, path.Root("id"), req, resp)
}

func (r *volumeResource) flatten(ctx context.Context, vol *volumes.Volume, m *volumeModel) (diags diag.Diagnostics) {
	m.ID = types.StringValue(vol.ID)
	m.Name = types.StringValue(vol.Name)
	m.Size = types.Int64Value(int64(vol.Size))
	m.Description = types.StringValue(vol.Description)
	m.VolumeType = types.StringValue(vol.VolumeType)
	m.AvailabilityZone = types.StringValue(vol.AvailabilityZone)
	m.SnapshotID = optionalStr(vol.SnapshotID)
	m.SourceVolID = optionalStr(vol.SourceVolID)
	m.Bootable = types.BoolValue(vol.Bootable == "true")
	m.Encrypted = types.BoolValue(vol.Encrypted)
	m.Status = types.StringValue(vol.Status)

	meta := make(map[string]string, len(vol.Metadata))
	for k, v := range vol.Metadata {
		meta[k] = v
	}
	metaMap, d := types.MapValueFrom(ctx, types.StringType, meta)
	diags = append(diags, d...)
	m.Metadata = metaMap

	if m.Region.IsNull() || m.Region.IsUnknown() {
		m.Region = types.StringValue(r.config.Region)
	}
	// Cinder does not store the retype policy; keep the configured value and
	// fill in the default for state written by an import or an older provider.
	if m.VolumeRetypePolicy.IsNull() || m.VolumeRetypePolicy.IsUnknown() {
		m.VolumeRetypePolicy = types.StringValue("never")
	}
	return diags
}

func extendVolume(ctx context.Context, client *gophercloud.ServiceClient, id string, newSize int) error {
	return volumes.ExtendSize(ctx, client, id, volumes.ExtendSizeOpts{NewSize: newSize}).ExtractErr()
}

func optionalStr(v string) types.String {
	if v == "" {
		return types.StringNull()
	}
	return types.StringValue(v)
}

func waitForVolumeStatus(ctx context.Context, client *gophercloud.ServiceClient, id, target string, timeout time.Duration) (*volumes.Volume, error) {
	deadline := time.Now().Add(timeout)
	for {
		vol, err := volumes.Get(ctx, client, id).Extract()
		if err != nil {
			return nil, err
		}
		switch vol.Status {
		case target:
			return vol, nil
		case "error", "error_deleting":
			return nil, fmt.Errorf("volume %s entered %q state", id, vol.Status)
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("timed out waiting for volume %s to reach %q (last status %q)", id, target, vol.Status)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(3 * time.Second):
		}
	}
}

// volumeRetypePollInterval is a var so unit tests can shorten it.
var volumeRetypePollInterval = 3 * time.Second

// volumeRetypeTimeout bounds the wait for a retype. A retype that migrates the
// volume to another backend copies all of its data, so it gets far longer than
// a create.
const volumeRetypeTimeout = 60 * time.Minute

// retypeTarget returns the type Update has to retype the volume to, and
// whether a retype is planned at all. Only a volume_type that differs from
// state asks for one: a change to volume_retype_policy alone asks for none.
func retypeTarget(plan, state *volumeModel) (string, bool) {
	if plan.VolumeType.IsNull() || plan.VolumeType.IsUnknown() || plan.VolumeType.Equal(state.VolumeType) {
		return "", false
	}
	return plan.VolumeType.ValueString(), true
}

// retypeVolume changes a volume's type with Cinder's os-retype action and waits
// for the retype to end. It sends nothing when the volume already has the
// type: Cinder refuses a retype to the volume's own type, and that is the
// situation after the referenced volume type is renamed (Terraform renames the
// type first, and Cinder reports types by name) or after the same retype was
// done outside Terraform. It returns the volume as Cinder reports it at the
// end; the caller checks the type, because Cinder keeps the old one when a
// retype it accepted fails later.
func retypeVolume(ctx context.Context, client *gophercloud.ServiceClient, id, newType string, policy volumes.MigrationPolicy) (*volumes.Volume, error) {
	vol, err := clients.RequireObject(volumes.Get(ctx, client, id).Extract())
	// volumes.Get decodes a 200 without the volume to an empty volume, not to
	// nil, so RequireObject passes it; its empty type would send a retype.
	if err == nil && vol.ID == "" {
		err = clients.ErrNoObject
	}
	if err != nil {
		return nil, err
	}
	if vol.VolumeType == newType {
		return vol, nil
	}
	opts := volumes.ChangeTypeOpts{NewType: newType, MigrationPolicy: policy}
	if err := volumes.ChangeType(ctx, client, id, opts).ExtractErr(); err != nil {
		return nil, err
	}
	return waitForVolumeRetype(ctx, client, id, volumeRetypeTimeout)
}

// waitForVolumeRetype polls a volume until Cinder ends a retype. Cinder sets
// "retyping" before it answers the request and restores the volume's earlier
// status, "available" or "in-use", when the retype ends, whether it worked or
// not. "error" and "error_deleting" fail the wait.
func waitForVolumeRetype(ctx context.Context, client *gophercloud.ServiceClient, id string, timeout time.Duration) (*volumes.Volume, error) {
	// The HTTP client has no timeout, and the deadline below is checked only
	// between polls: the context also bounds a GET that never answers.
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	deadline := time.Now().Add(timeout)
	for {
		vol, err := clients.RequireObject(volumes.Get(ctx, client, id).Extract())
		// An empty volume (a 200 without the volume) has an empty status,
		// which would keep the wait polling until it times out.
		if err == nil && vol.ID == "" {
			err = clients.ErrNoObject
		}
		if err != nil {
			return nil, err
		}
		switch vol.Status {
		case "available", "in-use":
			return vol, nil
		case "error", "error_deleting":
			return nil, fmt.Errorf("volume %s entered %q state during the retype", id, vol.Status)
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("timed out after %s waiting for volume %s to finish the retype (last status %q)", timeout, id, vol.Status)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(volumeRetypePollInterval):
		}
	}
}

func waitForVolumeDeleted(ctx context.Context, client *gophercloud.ServiceClient, id string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		vol, err := volumes.Get(ctx, client, id).Extract()
		if err != nil {
			if gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
				return nil
			}
			return err
		}
		if vol.Status == "error_deleting" {
			return fmt.Errorf("volume %s entered error_deleting state", id)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out waiting for volume %s to delete", id)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(3 * time.Second):
		}
	}
}
