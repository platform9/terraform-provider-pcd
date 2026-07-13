// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0
//
// Ported from terraform-provider-openstack v3.4.0
// (openstack/resource_openstack_blockstorage_volume_v3.go snapshot handling),
// adapted for the terraform-plugin-framework and PCD.

package blockstorage

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/blockstorage/v3/snapshots"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/boolplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/platform9/terraform-provider-pcd/internal/clients"
)

var (
	_ resource.Resource                = (*snapshotResource)(nil)
	_ resource.ResourceWithConfigure   = (*snapshotResource)(nil)
	_ resource.ResourceWithImportState = (*snapshotResource)(nil)
)

// NewSnapshotResource is the factory registered with the provider.
func NewSnapshotResource() resource.Resource {
	return &snapshotResource{}
}

type snapshotResource struct {
	config *clients.Config
}

type snapshotModel struct {
	ID          types.String `tfsdk:"id"`
	VolumeID    types.String `tfsdk:"volume_id"`
	Name        types.String `tfsdk:"name"`
	Description types.String `tfsdk:"description"`
	Force       types.Bool   `tfsdk:"force"`
	Metadata    types.Map    `tfsdk:"metadata"`
	Size        types.Int64  `tfsdk:"size"`
	Status      types.String `tfsdk:"status"`
	Region      types.String `tfsdk:"region"`
}

func (r *snapshotResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_blockstorage_snapshot"
}

func (r *snapshotResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	useState := []planmodifier.String{stringplanmodifier.UseStateForUnknown()}
	forceNew := []planmodifier.String{stringplanmodifier.RequiresReplace(), stringplanmodifier.UseStateForUnknown()}
	resp.Schema = schema.Schema{
		MarkdownDescription: "Manages a Cinder volume snapshot in PCD.",
		Attributes: map[string]schema.Attribute{
			"id":          schema.StringAttribute{Computed: true, MarkdownDescription: "The snapshot ID.", PlanModifiers: useState},
			"volume_id":   schema.StringAttribute{Required: true, MarkdownDescription: "The volume to snapshot. Changing this forces a new resource.", PlanModifiers: forceNew},
			"name":        schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "The name of the snapshot.", PlanModifiers: useState},
			"description": schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "A description of the snapshot.", PlanModifiers: useState},
			"force":       schema.BoolAttribute{Optional: true, Computed: true, Default: booldefault.StaticBool(false), MarkdownDescription: "Snapshot the volume even if it is attached/in-use. Changing this forces a new resource.", PlanModifiers: []planmodifier.Bool{boolplanmodifier.RequiresReplace()}},
			"metadata":    schema.MapAttribute{Optional: true, Computed: true, ElementType: types.StringType, MarkdownDescription: "Key-value metadata for the snapshot."},
			"size":        schema.Int64Attribute{Computed: true, MarkdownDescription: "The size of the snapshot in GB."},
			"status":      schema.StringAttribute{Computed: true, MarkdownDescription: "The Cinder status (e.g. available)."},
			"region":      schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "The region. Defaults to the provider's region.", PlanModifiers: useState},
		},
	}
}

func (r *snapshotResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.config = configureClient(req.ProviderData, &resp.Diagnostics)
}

func (r *snapshotResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan snapshotModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := r.config.BlockStorageV3Client()
	if err != nil {
		resp.Diagnostics.AddError("blockstorage: building v3 client", err.Error())
		return
	}

	snap, err := snapshots.Create(ctx, client, snapshots.CreateOpts{
		VolumeID:    plan.VolumeID.ValueString(),
		Force:       plan.Force.ValueBool(),
		Name:        plan.Name.ValueString(),
		Description: plan.Description.ValueString(),
		Metadata:    mapToStrings(ctx, plan.Metadata, &resp.Diagnostics),
	}).Extract()
	if err != nil {
		resp.Diagnostics.AddError("blockstorage: creating snapshot", err.Error())
		return
	}

	final, err := waitForSnapshotStatus(ctx, client, snap.ID, "available", 20*time.Minute)
	if err != nil {
		resp.Diagnostics.AddError("blockstorage: waiting for snapshot to become available", err.Error())
		return
	}

	// Build state from the object the waiter already fetched rather than a second
	// Get, so a transient read failure can't orphan the created snapshot.
	resp.Diagnostics.Append(r.setState(ctx, &plan, final)...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *snapshotResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state snapshotModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := r.config.BlockStorageV3Client()
	if err != nil {
		resp.Diagnostics.AddError("blockstorage: building v3 client", err.Error())
		return
	}

	notFound, diags := r.readIntoChecked(ctx, client, state.ID.ValueString(), &state)
	if notFound {
		resp.State.RemoveResource(ctx)
		return
	}
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *snapshotResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state snapshotModel
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
	if !plan.Name.Equal(state.Name) || !plan.Description.Equal(state.Description) {
		name := plan.Name.ValueString()
		desc := plan.Description.ValueString()
		if _, err := snapshots.Update(ctx, client, id, snapshots.UpdateOpts{Name: &name, Description: &desc}).Extract(); err != nil {
			resp.Diagnostics.AddError("blockstorage: updating snapshot", err.Error())
			return
		}
	}

	if !plan.Metadata.Equal(state.Metadata) {
		meta := map[string]any{}
		for k, v := range mapToStrings(ctx, plan.Metadata, &resp.Diagnostics) {
			meta[k] = v
		}
		if resp.Diagnostics.HasError() {
			return
		}
		if _, err := snapshots.UpdateMetadata(ctx, client, id, snapshots.UpdateMetadataOpts{Metadata: meta}).Extract(); err != nil {
			resp.Diagnostics.AddError("blockstorage: updating snapshot metadata", err.Error())
			return
		}
	}

	resp.Diagnostics.Append(r.readInto(ctx, client, id, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *snapshotResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state snapshotModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := r.config.BlockStorageV3Client()
	if err != nil {
		resp.Diagnostics.AddError("blockstorage: building v3 client", err.Error())
		return
	}

	if err := snapshots.Delete(ctx, client, state.ID.ValueString()).ExtractErr(); err != nil {
		if gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
			return
		}
		resp.Diagnostics.AddError("blockstorage: deleting snapshot", err.Error())
		return
	}
	if err := waitForSnapshotDeleted(ctx, client, state.ID.ValueString(), 10*time.Minute); err != nil {
		resp.Diagnostics.AddError("blockstorage: waiting for snapshot to delete", err.Error())
	}
}

func (r *snapshotResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, path.Root("id"), req, resp)
}

func (r *snapshotResource) readInto(ctx context.Context, client *gophercloud.ServiceClient, id string, m *snapshotModel) diag.Diagnostics {
	notFound, diags := r.readIntoChecked(ctx, client, id, m)
	if notFound {
		diags.AddError("blockstorage: reading snapshot", fmt.Sprintf("snapshot %s not found immediately after write", id))
	}
	return diags
}

func (r *snapshotResource) readIntoChecked(ctx context.Context, client *gophercloud.ServiceClient, id string, m *snapshotModel) (notFound bool, diags diag.Diagnostics) {
	snap, err := snapshots.Get(ctx, client, id).Extract()
	if err != nil {
		if gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
			return true, diags
		}
		diags.AddError("blockstorage: reading snapshot", err.Error())
		return false, diags
	}
	return false, r.setState(ctx, m, snap)
}

// setState populates the model from a snapshot. force is create-only (never
// returned by the API), so it is intentionally left untouched.
func (r *snapshotResource) setState(ctx context.Context, m *snapshotModel, snap *snapshots.Snapshot) diag.Diagnostics {
	var diags diag.Diagnostics
	m.ID = types.StringValue(snap.ID)
	m.VolumeID = types.StringValue(snap.VolumeID)
	m.Name = types.StringValue(snap.Name)
	m.Description = types.StringValue(snap.Description)
	m.Size = types.Int64Value(int64(snap.Size))
	m.Status = types.StringValue(snap.Status)

	meta := snap.Metadata
	if meta == nil {
		meta = map[string]string{}
	}
	mv, d := types.MapValueFrom(ctx, types.StringType, meta)
	diags = append(diags, d...)
	m.Metadata = mv

	if m.Region.IsNull() || m.Region.IsUnknown() {
		m.Region = types.StringValue(r.config.Region)
	}
	return diags
}

func waitForSnapshotStatus(ctx context.Context, client *gophercloud.ServiceClient, id, target string, timeout time.Duration) (*snapshots.Snapshot, error) {
	deadline := time.Now().Add(timeout)
	for {
		snap, err := snapshots.Get(ctx, client, id).Extract()
		if err != nil {
			return nil, err
		}
		switch snap.Status {
		case target:
			return snap, nil
		case "error", "error_deleting":
			return nil, fmt.Errorf("snapshot %s entered %q state", id, snap.Status)
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("timed out waiting for snapshot %s to reach %q (last status %q)", id, target, snap.Status)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(3 * time.Second):
		}
	}
}

func waitForSnapshotDeleted(ctx context.Context, client *gophercloud.ServiceClient, id string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		snap, err := snapshots.Get(ctx, client, id).Extract()
		if err != nil {
			if gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
				return nil
			}
			return err
		}
		if snap.Status == "error_deleting" {
			return fmt.Errorf("snapshot %s entered error_deleting state", id)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out waiting for snapshot %s to delete", id)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(3 * time.Second):
		}
	}
}
