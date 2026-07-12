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
	"time"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/blockstorage/v3/volumes"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/platform9/terraform-provider-pcd/internal/clients"
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
}

func (r *volumeResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_blockstorage_volume"
}

func (r *volumeResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	stable := []planmodifier.String{stringplanmodifier.UseStateForUnknown()}
	fnC := []planmodifier.String{stringplanmodifier.RequiresReplace(), stringplanmodifier.UseStateForUnknown()}
	fn := []planmodifier.String{stringplanmodifier.RequiresReplace()}
	resp.Schema = schema.Schema{
		MarkdownDescription: "Manages a block storage volume in PCD's Cinder service.",
		Attributes: map[string]schema.Attribute{
			"id":                schema.StringAttribute{Computed: true, MarkdownDescription: "The volume ID.", PlanModifiers: stable},
			"name":              schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "The name of the volume.", PlanModifiers: stable},
			"size":              schema.Int64Attribute{Required: true, MarkdownDescription: "Size in GB. Increasing extends the volume in place; decreasing is not allowed."},
			"description":       schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "A description of the volume.", PlanModifiers: stable},
			"volume_type":       schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "The volume type. Changing this forces a new resource.", PlanModifiers: fnC},
			"availability_zone": schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "Availability zone. Changing this forces a new resource.", PlanModifiers: fnC},
			"snapshot_id":       schema.StringAttribute{Optional: true, MarkdownDescription: "Create the volume from this snapshot. Changing this forces a new resource.", PlanModifiers: fn},
			"source_vol_id":     schema.StringAttribute{Optional: true, MarkdownDescription: "Clone the volume from this source volume. Changing this forces a new resource.", PlanModifiers: fn},
			"image_id":          schema.StringAttribute{Optional: true, MarkdownDescription: "Create the volume from this image. Changing this forces a new resource.", PlanModifiers: fn},
			"metadata":          schema.MapAttribute{Optional: true, Computed: true, ElementType: types.StringType, MarkdownDescription: "Key-value metadata for the volume."},
			"bootable":          schema.BoolAttribute{Computed: true, MarkdownDescription: "Whether the volume is bootable."},
			"encrypted":         schema.BoolAttribute{Computed: true, MarkdownDescription: "Whether the volume is encrypted."},
			"status":            schema.StringAttribute{Computed: true, MarkdownDescription: "The Cinder status (e.g. available)."},
			"region":            schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "The region. Defaults to the provider's region.", PlanModifiers: stable},
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

	client, err := r.config.BlockStorageV3Client()
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

	client, err := r.config.BlockStorageV3Client()
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

	client, err := r.config.BlockStorageV3Client()
	if err != nil {
		resp.Diagnostics.AddError("blockstorage: building v3 client", err.Error())
		return
	}

	if !plan.Name.Equal(state.Name) || !plan.Description.Equal(state.Description) || !plan.Metadata.Equal(state.Metadata) {
		name := plan.Name.ValueString()
		description := plan.Description.ValueString()
		updateOpts := volumes.UpdateOpts{Name: &name, Description: &description}
		if !plan.Metadata.IsNull() && !plan.Metadata.IsUnknown() {
			var meta map[string]string
			resp.Diagnostics.Append(plan.Metadata.ElementsAs(ctx, &meta, false)...)
			if resp.Diagnostics.HasError() {
				return
			}
			updateOpts.Metadata = meta
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

	vol, err := volumes.Get(ctx, client, plan.ID.ValueString()).Extract()
	if err != nil {
		resp.Diagnostics.AddError("blockstorage: reading volume after update", err.Error())
		return
	}
	resp.Diagnostics.Append(r.flatten(ctx, vol, &plan)...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *volumeResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state volumeModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := r.config.BlockStorageV3Client()
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
