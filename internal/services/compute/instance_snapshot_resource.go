// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package compute

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/blockstorage/v3/snapshots"
	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/servers"
	"github.com/gophercloud/gophercloud/v2/openstack/image/v2/images"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/boolplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/int64planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/listplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/platform9/terraform-provider-pcd/internal/clients"
	"github.com/platform9/terraform-provider-pcd/internal/tfstate"
)

var (
	_ resource.Resource                = (*instanceSnapshotResource)(nil)
	_ resource.ResourceWithConfigure   = (*instanceSnapshotResource)(nil)
	_ resource.ResourceWithImportState = (*instanceSnapshotResource)(nil)
)

// computeMicroversionCreateImage is the first compute microversion that
// returns the new image's ID in the createImage response body; below it,
// gophercloud has to parse the Location header instead.
const computeMicroversionCreateImage = "2.45"

// snapshotTimeout bounds each wait in a snapshot's create and delete, as the
// provider's image waits do.
const snapshotTimeout = 30 * time.Minute

// snapshotPollInterval is how often the Glance and Cinder waits poll. A var
// so unit tests can shorten it.
var snapshotPollInterval = 3 * time.Second

// errSnapshotImageGone marks an image that disappeared while its snapshot was
// being taken: Nova deletes the image when the snapshot fails.
var errSnapshotImageGone = errors.New("snapshot image deleted")

// NewInstanceSnapshotResource is the factory registered with the provider.
func NewInstanceSnapshotResource() resource.Resource {
	return &instanceSnapshotResource{}
}

type instanceSnapshotResource struct {
	config *clients.Config
}

type instanceSnapshotModel struct {
	ID                types.String `tfsdk:"id"`
	InstanceID        types.String `tfsdk:"instance_id"`
	Name              types.String `tfsdk:"name"`
	Status            types.String `tfsdk:"status"`
	SizeBytes         types.Int64  `tfsdk:"size_bytes"`
	VolumeBacked      types.Bool   `tfsdk:"volume_backed"`
	VolumeSnapshotIDs types.List   `tfsdk:"volume_snapshot_ids"`
	CreatedAt         types.String `tfsdk:"created_at"`
	Region            types.String `tfsdk:"region"`
}

func (r *instanceSnapshotResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_compute_instance_snapshot"
}

func (r *instanceSnapshotResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	stable := []planmodifier.String{stringplanmodifier.UseStateForUnknown()}
	resp.Schema = schema.Schema{
		MarkdownDescription: "Takes a snapshot of an instance as a Glance image, as the PCD UI's Snapshot action does (Nova " +
			"`createImage`). The image can boot new instances or serve as a rebuild target, so it suits golden-image capture. " +
			"For an instance booted from an image, the snapshot holds the root disk only; attached volumes are not included. " +
			"For an instance that boots from a volume, Nova snapshots every attached volume in Cinder and creates a zero-byte " +
			"image that refers to those snapshots (`volume_backed = true`); create waits until the Cinder snapshots are " +
			"available, and destroy deletes the image and then those snapshots. Volume-backed snapshots need a Cinder backend " +
			"that supports snapshots; the NFS driver does so only with `nfs_snapshot_support = true`. A snapshot belongs to " +
			"its instance: replacing the instance replaces the snapshot, and destroying the snapshot deletes the image. The " +
			"snapshot is taken after any change to the instance in the same apply, so it cannot capture the instance as it " +
			"was before that change. An instance whose `image_id` refers to this snapshot is rebuilt in place, and its root " +
			"disk erased, whenever the snapshot is replaced (for example when `instance_id` changes); add " +
			"`lifecycle { ignore_changes = [image_id] }` to such an instance to keep it on the image it booted from.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{Computed: true, MarkdownDescription: "The Glance image ID.", PlanModifiers: stable},
			"instance_id": schema.StringAttribute{Required: true,
				MarkdownDescription: "The ID of the instance to snapshot. Changing this takes a new snapshot and deletes the old one.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.RequiresReplace()}},
			"name": schema.StringAttribute{Required: true,
				MarkdownDescription: "The image name. Changing it renames the image in place."},
			"status": schema.StringAttribute{Computed: true, MarkdownDescription: "The Glance image status, `active` once the snapshot is usable.", PlanModifiers: stable},
			"size_bytes": schema.Int64Attribute{Computed: true,
				MarkdownDescription: "The image size in bytes; `0` for a volume-backed snapshot, whose data is in Cinder.",
				PlanModifiers:       []planmodifier.Int64{int64planmodifier.UseStateForUnknown()}},
			"volume_backed": schema.BoolAttribute{Computed: true,
				MarkdownDescription: "Whether the image refers to Cinder volume snapshots (the instance boots from a volume).",
				PlanModifiers:       []planmodifier.Bool{boolplanmodifier.UseStateForUnknown()}},
			"volume_snapshot_ids": schema.ListAttribute{Computed: true, ElementType: types.StringType,
				MarkdownDescription: "The Cinder snapshots a volume-backed snapshot refers to; empty otherwise. Destroy deletes them.",
				PlanModifiers:       []planmodifier.List{listplanmodifier.UseStateForUnknown()}},
			"created_at": schema.StringAttribute{Computed: true, MarkdownDescription: "When the image was created (RFC 3339).", PlanModifiers: stable},
			"region":     schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "The region. Defaults to the provider's region. Changing this forces a new resource.", PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace(), stringplanmodifier.UseStateForUnknown()}},
		},
	}
}

func (r *instanceSnapshotResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.config = configureClient(req.ProviderData, &resp.Diagnostics)
}

func (r *instanceSnapshotResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan instanceSnapshotModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	client, err := r.config.ForRegion(plan.Region.ValueString()).ComputeV2Client()
	if err != nil {
		resp.Diagnostics.AddError("compute: building v2 client", err.Error())
		return
	}
	imgClient, err := r.config.ForRegion(plan.Region.ValueString()).ImageV2Client()
	if err != nil {
		resp.Diagnostics.AddError("compute: building image v2 client", err.Error())
		return
	}

	instanceID := plan.InstanceID.ValueString()
	opts := servers.CreateImageOpts{Name: plan.Name.ValueString()}
	imageID, err := servers.CreateImage(ctx, withMicroversion(client, computeMicroversionCreateImage), instanceID, opts).ExtractImageID()
	if err != nil {
		resp.Diagnostics.AddError("compute: snapshotting instance "+instanceID, err.Error())
		return
	}
	// gophercloud reads the ID from the body only when the response is
	// exactly application/json; anything else yields "" and no error, and an
	// empty ID must never reach state.
	if imageID == "" {
		resp.Diagnostics.AddError("compute: snapshotting instance "+instanceID,
			"Nova accepted the snapshot but its response named no image ID. Find the image with "+
				"`openstack image list --property instance_uuid="+instanceID+"` and import it, or delete it.")
		return
	}

	plan.ID = types.StringValue(imageID)
	if plan.Region.IsNull() || plan.Region.IsUnknown() {
		plan.Region = types.StringValue(r.config.Region)
	}
	if !tfstate.RecordCreated(ctx, resp, &plan) {
		return
	}

	img, err := waitForSnapshotImage(ctx, imgClient, imageID, snapshotTimeout)
	if err != nil {
		if errors.Is(err, errSnapshotImageGone) {
			resp.State.RemoveResource(ctx)
		}
		resp.Diagnostics.AddError("compute: waiting for snapshot image", err.Error())
		return
	}
	entries, err := imageBlockDeviceMapping(img)
	if err != nil {
		resp.Diagnostics.AddError("compute: reading snapshot image", err.Error())
		return
	}
	// Record the Cinder snapshots before waiting on them: if a wait fails,
	// the row stays in state, tainted, and its destroy deletes them too.
	resp.Diagnostics.Append(r.flatten(ctx, img, entries, &plan)...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	snapIDs := volumeSnapshotIDs(entries)
	if len(snapIDs) > 0 {
		// Nova marks the image active before Cinder has finished the volume
		// snapshots it refers to; an instance booted from it before then fails.
		bs, err := r.config.ForRegion(plan.Region.ValueString()).BlockStorageV3Client()
		if err != nil {
			resp.Diagnostics.AddError("compute: building block storage v3 client", err.Error())
			return
		}
		for _, sid := range snapIDs {
			if err := waitForVolumeSnapshotAvailable(ctx, bs, sid, snapshotTimeout); err != nil {
				resp.Diagnostics.AddError("compute: waiting for volume snapshot", err.Error())
				return
			}
		}
	}
	// The instance holds a task (image_uploading) until Nova has finished
	// with it; wait so the next change to the instance is not refused.
	if _, err := waitForServerSettled(ctx, client, instanceID, []string{"ACTIVE", "SHUTOFF", "PAUSED", "SUSPENDED"}, snapshotTimeout); err != nil {
		resp.Diagnostics.AddError("compute: waiting for instance after snapshot", err.Error())
		return
	}

	resp.Diagnostics.Append(r.flatten(ctx, img, entries, &plan)...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *instanceSnapshotResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state instanceSnapshotModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	// A row with no ID names no image; a read would reach the image list.
	if tfstate.DropRowWithoutID(ctx, resp, state.ID, "snapshot", state.Name.ValueString()) {
		return
	}
	imgClient, err := r.config.ForRegion(state.Region.ValueString()).ImageV2Client()
	if err != nil {
		resp.Diagnostics.AddError("compute: building image v2 client", err.Error())
		return
	}
	img, err := images.Get(ctx, imgClient, state.ID.ValueString()).Extract()
	if err != nil {
		if gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
			resp.Diagnostics.AddWarning("Snapshot image not found", r.goneMessage(ctx, &state))
			resp.State.RemoveResource(ctx)
			return
		}
		resp.Diagnostics.AddError("compute: reading snapshot image", err.Error())
		return
	}
	entries, err := imageBlockDeviceMapping(img)
	if err != nil {
		resp.Diagnostics.AddError("compute: reading snapshot image", err.Error())
		return
	}
	if state.InstanceID.IsNull() || state.InstanceID.ValueString() == "" {
		// An import knows only the image ID.
		instanceID, err := r.sourceInstanceID(ctx, state.Region.ValueString(), img, volumeSnapshotIDs(entries))
		if err != nil {
			resp.Diagnostics.AddError("compute: finding the instance of snapshot "+img.ID, err.Error())
			return
		}
		state.InstanceID = types.StringValue(instanceID)
	}
	resp.Diagnostics.Append(r.flatten(ctx, img, entries, &state)...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *instanceSnapshotResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state instanceSnapshotModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	imgClient, err := r.config.ForRegion(plan.Region.ValueString()).ImageV2Client()
	if err != nil {
		resp.Diagnostics.AddError("compute: building image v2 client", err.Error())
		return
	}
	if !plan.Name.Equal(state.Name) {
		if _, err := images.Update(ctx, imgClient, state.ID.ValueString(), images.UpdateOpts{
			images.ReplaceImageName{NewName: plan.Name.ValueString()},
		}).Extract(); err != nil {
			resp.Diagnostics.AddError("compute: renaming snapshot image", err.Error())
			return
		}
	}
	img, err := images.Get(ctx, imgClient, state.ID.ValueString()).Extract()
	if err != nil {
		resp.Diagnostics.AddError("compute: reading snapshot image after rename", err.Error())
		return
	}
	entries, err := imageBlockDeviceMapping(img)
	if err != nil {
		resp.Diagnostics.AddError("compute: reading snapshot image", err.Error())
		return
	}
	resp.Diagnostics.Append(r.flatten(ctx, img, entries, &plan)...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

// Delete removes the image and then the Cinder snapshots a volume-backed
// snapshot refers to, waiting until they are gone: a volume deleted later in
// the same destroy (a root volume with delete_on_termination, say) cannot be
// deleted while snapshots of it remain. The image goes first because PCD
// protects a volume snapshot from deletion while an image refers to it.
func (r *instanceSnapshotResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state instanceSnapshotModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if tfstate.SkipDeleteWithoutID(resp, state.ID, "snapshot", state.Name.ValueString()) {
		return
	}
	imgClient, err := r.config.ForRegion(state.Region.ValueString()).ImageV2Client()
	if err != nil {
		resp.Diagnostics.AddError("compute: building image v2 client", err.Error())
		return
	}
	if err := images.Delete(ctx, imgClient, state.ID.ValueString()).ExtractErr(); err != nil &&
		!gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
		resp.Diagnostics.AddError("compute: deleting snapshot image", err.Error())
		return
	}

	snapIDs := stringList(ctx, state.VolumeSnapshotIDs, &resp.Diagnostics)
	if resp.Diagnostics.HasError() || len(snapIDs) == 0 {
		return
	}
	bs, err := r.config.ForRegion(state.Region.ValueString()).BlockStorageV3Client()
	if err != nil {
		resp.Diagnostics.AddError("compute: building block storage v3 client", err.Error())
		return
	}
	var left []string
	for _, sid := range snapIDs {
		if err := snapshots.Delete(ctx, bs, sid).ExtractErr(); err != nil && !gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
			left = append(left, fmt.Sprintf("%s (%v)", sid, err))
			continue
		}
		if err := waitForVolumeSnapshotDeleted(ctx, bs, sid, snapshotTimeout); err != nil {
			left = append(left, fmt.Sprintf("%s (%v)", sid, err))
		}
	}
	if len(left) > 0 {
		resp.Diagnostics.AddError("compute: deleting the volume snapshots of image "+state.ID.ValueString(),
			"The image is deleted, but these Cinder volume snapshots remain: "+strings.Join(left, "; ")+
				". Delete them with `openstack volume snapshot delete <id>` once nothing uses them, "+
				"for example after deleting a volume created from one.")
	}
}

func (r *instanceSnapshotResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, path.Root("id"), req, resp)
}

// flatten records what Glance reports. instance_id is an input and stays as
// configured (Read fills it after an import).
func (r *instanceSnapshotResource) flatten(ctx context.Context, img *images.Image, entries []map[string]any, m *instanceSnapshotModel) diag.Diagnostics {
	var diags diag.Diagnostics
	m.ID = types.StringValue(img.ID)
	m.Name = types.StringValue(img.Name)
	m.Status = types.StringValue(string(img.Status))
	m.SizeBytes = types.Int64Value(img.SizeBytes)
	m.VolumeBacked = types.BoolValue(len(entries) > 0)
	ids, d := types.ListValueFrom(ctx, types.StringType, volumeSnapshotIDs(entries))
	diags.Append(d...)
	m.VolumeSnapshotIDs = ids
	m.CreatedAt = types.StringValue(img.CreatedAt.UTC().Format(time.RFC3339))
	if m.Region.IsNull() || m.Region.IsUnknown() {
		m.Region = types.StringValue(r.config.Region)
	}
	return diags
}

// sourceInstanceID recovers the instance an imported snapshot was taken
// from. Nova records it as the image's instance_uuid property for an
// instance booted from an image. For a volume-backed instance that property
// can be a stale copy inherited from the image the instance was booted from,
// so the Cinder snapshots are asked instead: Nova tags each with the
// instance it was taken of.
func (r *instanceSnapshotResource) sourceInstanceID(ctx context.Context, region string, img *images.Image, snapIDs []string) (string, error) {
	if len(snapIDs) == 0 {
		if id, _ := img.Properties["instance_uuid"].(string); id != "" {
			return id, nil
		}
		return "", fmt.Errorf("image %s has no instance_uuid property, so it is not a snapshot of an instance", img.ID)
	}
	bs, err := r.config.ForRegion(region).BlockStorageV3Client()
	if err != nil {
		return "", err
	}
	snap, err := clients.RequireObject(snapshots.Get(ctx, bs, snapIDs[0]).Extract())
	if err != nil {
		return "", fmt.Errorf("reading volume snapshot %s: %w", snapIDs[0], err)
	}
	if id := snap.Metadata["instance_uuid"]; id != "" {
		return id, nil
	}
	return "", fmt.Errorf("volume snapshot %s of image %s has no instance_uuid metadata", snapIDs[0], img.ID)
}

// goneMessage explains an image that disappeared, naming any Cinder
// snapshots of it that still exist, since nothing would delete them now.
func (r *instanceSnapshotResource) goneMessage(ctx context.Context, m *instanceSnapshotModel) string {
	msg := fmt.Sprintf("Image %s no longer exists and was removed from state.", m.ID.ValueString())
	var discard diag.Diagnostics
	snapIDs := stringList(ctx, m.VolumeSnapshotIDs, &discard)
	if len(snapIDs) == 0 {
		return msg
	}
	bs, err := r.config.ForRegion(m.Region.ValueString()).BlockStorageV3Client()
	if err != nil {
		return msg
	}
	var left []string
	for _, sid := range snapIDs {
		if _, err := snapshots.Get(ctx, bs, sid).Extract(); err == nil {
			left = append(left, sid)
		}
	}
	if len(left) == 0 {
		return msg
	}
	return msg + " Its Cinder volume snapshots still exist and are no longer managed: " + strings.Join(left, ", ") +
		". Delete them with `openstack volume snapshot delete <id>`."
}

// volumeSnapshotIDs lists the Cinder snapshots in a block_device_mapping.
// It never returns nil, so the computed list is always known and empty for
// a snapshot of an instance booted from an image.
func volumeSnapshotIDs(entries []map[string]any) []string {
	ids := []string{}
	for _, e := range entries {
		if source, _ := e["source_type"].(string); source != "snapshot" {
			continue
		}
		if id, _ := e["snapshot_id"].(string); id != "" {
			ids = append(ids, id)
		}
	}
	return ids
}

// stringList reads a list of strings, treating null and unknown as empty.
func stringList(ctx context.Context, l types.List, diags *diag.Diagnostics) []string {
	if l.IsNull() || l.IsUnknown() {
		return nil
	}
	var out []string
	diags.Append(l.ElementsAs(ctx, &out, false)...)
	return out
}

// waitForSnapshotImage polls Glance until Nova has uploaded the snapshot.
// A 404 means Nova gave up and deleted the image.
func waitForSnapshotImage(ctx context.Context, client *gophercloud.ServiceClient, id string, timeout time.Duration) (*images.Image, error) {
	deadline := time.Now().Add(timeout)
	for {
		img, err := images.Get(ctx, client, id).Extract()
		if err != nil {
			if gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
				return nil, fmt.Errorf("%w: image %s no longer exists. Nova deletes the image of a snapshot that "+
					"fails; `openstack server event list <instance>` shows why", errSnapshotImageGone, id)
			}
			return nil, err
		}
		switch img.Status {
		case images.ImageStatusActive:
			return img, nil
		case images.ImageStatusKilled, images.ImageStatusDeleted, images.ImageStatusPendingDelete, images.ImageStatusDeactivated:
			return nil, fmt.Errorf("image %s entered status %q", id, img.Status)
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("timed out waiting for image %s to become active (last status %q)", id, img.Status)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(snapshotPollInterval):
		}
	}
}

func waitForVolumeSnapshotAvailable(ctx context.Context, client *gophercloud.ServiceClient, id string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		snap, err := clients.RequireObject(snapshots.Get(ctx, client, id).Extract())
		if err != nil {
			return fmt.Errorf("reading volume snapshot %s: %w", id, err)
		}
		switch snap.Status {
		case "available":
			return nil
		case "error", "error_deleting":
			return fmt.Errorf("volume snapshot %s entered status %q", id, snap.Status)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out waiting for volume snapshot %s to become available (last status %q)", id, snap.Status)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(snapshotPollInterval):
		}
	}
}

func waitForVolumeSnapshotDeleted(ctx context.Context, client *gophercloud.ServiceClient, id string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		snap, err := clients.RequireObject(snapshots.Get(ctx, client, id).Extract())
		if err != nil {
			if gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
				return nil
			}
			return err
		}
		if snap.Status == "error_deleting" {
			return fmt.Errorf("entered status error_deleting")
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out waiting for the deletion (last status %q)", snap.Status)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(snapshotPollInterval):
		}
	}
}
