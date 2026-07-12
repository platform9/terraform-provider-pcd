// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0
//
// Ported from terraform-provider-openstack v3.4.0
// (openstack/resource_openstack_compute_volume_attach_v2.go), adapted for the
// terraform-plugin-framework and PCD.

package compute

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/blockstorage/v3/volumes"
	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/volumeattach"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/platform9/terraform-provider-pcd/internal/clients"
)

var (
	_ resource.Resource                = (*volumeAttachResource)(nil)
	_ resource.ResourceWithConfigure   = (*volumeAttachResource)(nil)
	_ resource.ResourceWithImportState = (*volumeAttachResource)(nil)
)

// NewVolumeAttachResource is the factory registered with the provider.
func NewVolumeAttachResource() resource.Resource {
	return &volumeAttachResource{}
}

type volumeAttachResource struct {
	config *clients.Config
}

type volumeAttachModel struct {
	ID         types.String `tfsdk:"id"`
	InstanceID types.String `tfsdk:"instance_id"`
	VolumeID   types.String `tfsdk:"volume_id"`
	Device     types.String `tfsdk:"device"`
	Region     types.String `tfsdk:"region"`
}

func (r *volumeAttachResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_compute_volume_attach"
}

func (r *volumeAttachResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	forceNew := []planmodifier.String{stringplanmodifier.RequiresReplace()}
	forceNewC := []planmodifier.String{stringplanmodifier.RequiresReplace(), stringplanmodifier.UseStateForUnknown()}
	stable := []planmodifier.String{stringplanmodifier.UseStateForUnknown()}
	resp.Schema = schema.Schema{
		MarkdownDescription: "Attaches a Cinder volume to a compute instance via Nova. All attributes force " +
			"replacement (attach/detach has no in-place update). Deleting this resource detaches the volume " +
			"without deleting it.",
		Attributes: map[string]schema.Attribute{
			"id":          schema.StringAttribute{Computed: true, MarkdownDescription: "The Nova volume-attachment ID.", PlanModifiers: stable},
			"instance_id": schema.StringAttribute{Required: true, MarkdownDescription: "The instance (server) ID to attach the volume to. Changing this forces a new resource.", PlanModifiers: forceNew},
			"volume_id":   schema.StringAttribute{Required: true, MarkdownDescription: "The Cinder volume ID to attach. Changing this forces a new resource.", PlanModifiers: forceNew},
			"device":      schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "Device path (e.g. /dev/vdb); omit for Nova to auto-assign. Nova may return a different device than requested. Changing this forces a new resource.", PlanModifiers: forceNewC},
			"region":      schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "The region. Defaults to the provider's region.", PlanModifiers: stable},
		},
	}
}

func (r *volumeAttachResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.config = configureClient(req.ProviderData, &resp.Diagnostics)
}

func (r *volumeAttachResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan volumeAttachModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := r.config.ComputeV2Client()
	if err != nil {
		resp.Diagnostics.AddError("compute: building v2 client", err.Error())
		return
	}

	createOpts := volumeattach.CreateOpts{VolumeID: plan.VolumeID.ValueString()}
	if !plan.Device.IsNull() && !plan.Device.IsUnknown() && plan.Device.ValueString() != "" {
		createOpts.Device = plan.Device.ValueString()
	}

	att, err := volumeattach.Create(ctx, client, plan.InstanceID.ValueString(), createOpts).Extract()
	if err != nil {
		resp.Diagnostics.AddError("compute: attaching volume", err.Error())
		return
	}

	// Best-effort: wait for the volume to report in-use. Skipped silently when the
	// block-storage service is unavailable (e.g. no Cinder backend on the lab).
	r.waitForVolume(ctx, plan.VolumeID.ValueString(), "in-use", 5*time.Minute)

	plan.ID = types.StringValue(att.ID)
	plan.Device = types.StringValue(att.Device)
	if plan.Region.IsNull() || plan.Region.IsUnknown() {
		plan.Region = types.StringValue(r.config.Region)
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *volumeAttachResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state volumeAttachModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := r.config.ComputeV2Client()
	if err != nil {
		resp.Diagnostics.AddError("compute: building v2 client", err.Error())
		return
	}

	att, err := volumeattach.Get(ctx, client, state.InstanceID.ValueString(), state.ID.ValueString()).Extract()
	if err != nil {
		if gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
			resp.Diagnostics.AddWarning("Volume attachment not found",
				fmt.Sprintf("Attachment %s on instance %s is gone; removed from state.", state.ID.ValueString(), state.InstanceID.ValueString()))
			resp.State.RemoveResource(ctx)
			return
		}
		resp.Diagnostics.AddError("compute: reading volume attachment", err.Error())
		return
	}

	state.ID = types.StringValue(att.ID)
	state.VolumeID = types.StringValue(att.VolumeID)
	state.Device = types.StringValue(att.Device)
	if state.Region.IsNull() || state.Region.IsUnknown() {
		state.Region = types.StringValue(r.config.Region)
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

// Update is required by the interface but never invoked (every attribute forces replacement).
func (r *volumeAttachResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan volumeAttachModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *volumeAttachResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state volumeAttachModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := r.config.ComputeV2Client()
	if err != nil {
		resp.Diagnostics.AddError("compute: building v2 client", err.Error())
		return
	}

	if err := volumeattach.Delete(ctx, client, state.InstanceID.ValueString(), state.ID.ValueString()).ExtractErr(); err != nil {
		if gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
			return
		}
		resp.Diagnostics.AddError("compute: detaching volume", err.Error())
		return
	}

	// Best-effort: wait for the volume to return to available.
	r.waitForVolume(ctx, state.VolumeID.ValueString(), "available", 5*time.Minute)
}

func (r *volumeAttachResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	instanceID, attachmentID, err := splitInstanceScopedID(req.ID)
	if err != nil {
		resp.Diagnostics.AddError("Invalid import ID", err.Error())
		return
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), attachmentID)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("instance_id"), instanceID)...)
}

// waitForVolume best-effort polls a Cinder volume until it reaches target status.
// Any failure (no block-storage service, volume error, timeout) returns silently:
// the Nova attachment call is the source of truth, and this is only a courtesy
// wait so the volume's state has settled before Terraform reports success.
func (r *volumeAttachResource) waitForVolume(ctx context.Context, volumeID, target string, timeout time.Duration) {
	bs, err := r.config.BlockStorageV3Client()
	if err != nil {
		return
	}
	deadline := time.Now().Add(timeout)
	for {
		vol, err := volumes.Get(ctx, bs, volumeID).Extract()
		if err != nil {
			return
		}
		switch vol.Status {
		case target, "error", "error_deleting", "error_extending":
			return
		}
		if time.Now().After(deadline) {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(3 * time.Second):
		}
	}
}
