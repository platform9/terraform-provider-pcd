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

// computeMicroversionVolumeAttachUpdate is the compute microversion this
// resource speaks to create, read and update an attachment. 2.79 adds
// delete_on_termination to the attach request and to the attachment, and 2.85
// lets a PUT change it in place. Keep it below 2.89: from 2.89 Nova drops the
// attachment's `id`, which Read stores as the resource ID. Pinning it also
// lifts Nova's multiattach gate, which refuses multiattach volumes below 2.60.
const computeMicroversionVolumeAttachUpdate = "2.85"

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

	DeleteOnTermination types.Bool `tfsdk:"delete_on_termination"`
}

func (r *volumeAttachResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_compute_volume_attach"
}

func (r *volumeAttachResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	forceNew := []planmodifier.String{stringplanmodifier.RequiresReplace()}
	// UseStateForUnknown runs first: on an in-place update the framework marks
	// an unset device unknown, and RequiresReplace would report that as a
	// change, which Terraform then discards.
	forceNewC := []planmodifier.String{stringplanmodifier.UseStateForUnknown(), stringplanmodifier.RequiresReplace()}
	stable := []planmodifier.String{stringplanmodifier.UseStateForUnknown()}
	resp.Schema = schema.Schema{
		MarkdownDescription: "Attaches a Cinder volume to a compute instance via Nova. Changing `instance_id`, " +
			"`volume_id` or `device` forces a new attachment (a detach, then an attach); `delete_on_termination` " +
			"changes in place. Deleting this resource detaches the volume without deleting it.\n\n" +
			"This resource is for data volumes. An instance's root volume comes from `pcd_compute_instance` (its " +
			"image, or a `block_device` with `boot_index = 0`), and Nova refuses to detach a root volume, so a root " +
			"volume's attachment imported here could never be destroyed.",
		Attributes: map[string]schema.Attribute{
			"id":          schema.StringAttribute{Computed: true, MarkdownDescription: "The Nova volume-attachment ID.", PlanModifiers: stable},
			"instance_id": schema.StringAttribute{Required: true, MarkdownDescription: "The instance (server) ID to attach the volume to. Changing this forces a new resource.", PlanModifiers: forceNew},
			"volume_id":   schema.StringAttribute{Required: true, MarkdownDescription: "The Cinder volume ID to attach. Changing this forces a new resource.", PlanModifiers: forceNew},
			"device":      schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "Device path (e.g. /dev/vdb); omit for Nova to auto-assign. Nova may return a different device than requested. Changing this forces a new resource.", PlanModifiers: forceNewC},
			"region":      schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "The region. Defaults to the provider's region. Changing this forces a new resource.", PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace(), stringplanmodifier.UseStateForUnknown()}},
			"delete_on_termination": schema.BoolAttribute{
				Optional: true, Computed: true,
				MarkdownDescription: "Whether Nova deletes the volume when the instance is deleted. Changing it updates " +
					"the attachment in place. Leave it unset to leave the flag alone: the provider then sends nothing and " +
					"reports the value Nova holds, which is `false` unless it was set elsewhere, for example in the PCD UI. " +
					"Terraform detaches the volume before it destroys the instance, so the flag takes effect only when the " +
					"instance is deleted outside Terraform. If the volume is also a `pcd_blockstorage_volume` in your " +
					"configuration, such a deletion takes the volume with it, and the next apply creates an empty one.",
			},
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

	client, err := r.config.ForRegion(plan.Region.ValueString()).ComputeV2Client()
	if err != nil {
		resp.Diagnostics.AddError("compute: building v2 client", err.Error())
		return
	}

	client = withMicroversion(client, computeMicroversionVolumeAttachUpdate)

	att, err := clients.RequireObject(volumeattach.Create(ctx, client, plan.InstanceID.ValueString(), volumeAttachCreateOpts(&plan)).Extract())
	if err != nil {
		resp.Diagnostics.AddError("compute: attaching volume", err.Error())
		return
	}

	// Best-effort: wait for the volume to report in-use. Skipped silently when the
	// block-storage service is unavailable (e.g. no Cinder backend on the lab).
	r.waitForVolume(ctx, plan.Region.ValueString(), plan.VolumeID.ValueString(), "in-use", 5*time.Minute)

	plan.ID = types.StringValue(att.ID)
	plan.Device = types.StringValue(att.Device)
	plan.DeleteOnTermination = deleteOnTerminationValue(att.DeleteOnTermination)
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

	client, err := r.config.ForRegion(state.Region.ValueString()).ComputeV2Client()
	if err != nil {
		resp.Diagnostics.AddError("compute: building v2 client", err.Error())
		return
	}

	client = withMicroversion(client, computeMicroversionVolumeAttachUpdate)

	att, err := clients.RequireObject(volumeattach.Get(ctx, client, state.InstanceID.ValueString(), state.ID.ValueString()).Extract())
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
	state.DeleteOnTermination = deleteOnTerminationValue(att.DeleteOnTermination)
	if state.Region.IsNull() || state.Region.IsUnknown() {
		state.Region = types.StringValue(r.config.Region)
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

// Update changes delete_on_termination in place; every other argument forces
// a new attachment. The flag is sent
// only when the configuration sets it and Nova holds a different value, so an
// unset flag stays whatever it is (for example, set in the PCD UI).
func (r *volumeAttachResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state volumeAttachModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := r.config.ForRegion(state.Region.ValueString()).ComputeV2Client()
	if err != nil {
		resp.Diagnostics.AddError("compute: building v2 client", err.Error())
		return
	}
	client = withMicroversion(client, computeMicroversionVolumeAttachUpdate)

	instanceID, attachmentID := state.InstanceID.ValueString(), state.ID.ValueString()
	att, err := clients.RequireObject(volumeattach.Get(ctx, client, instanceID, attachmentID).Extract())
	if err != nil {
		resp.Diagnostics.AddError("compute: reading volume attachment", err.Error())
		return
	}

	want := plan.DeleteOnTermination
	if !want.IsNull() && !want.IsUnknown() && !want.Equal(deleteOnTerminationValue(att.DeleteOnTermination)) {
		if err := updateDeleteOnTermination(ctx, client, instanceID, state.VolumeID.ValueString(), want.ValueBool()); err != nil {
			resp.Diagnostics.AddError("compute: updating delete_on_termination", err.Error())
			return
		}
		if att, err = clients.RequireObject(volumeattach.Get(ctx, client, instanceID, attachmentID).Extract()); err != nil {
			resp.Diagnostics.AddError("compute: reading volume attachment after update", err.Error())
			return
		}
	}

	plan.ID = types.StringValue(att.ID)
	plan.Device = types.StringValue(att.Device)
	plan.DeleteOnTermination = deleteOnTerminationValue(att.DeleteOnTermination)
	if plan.Region.IsNull() || plan.Region.IsUnknown() {
		plan.Region = types.StringValue(r.config.Region)
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *volumeAttachResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state volumeAttachModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := r.config.ForRegion(state.Region.ValueString()).ComputeV2Client()
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
	r.waitForVolume(ctx, state.Region.ValueString(), state.VolumeID.ValueString(), "available", 5*time.Minute)
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

// volumeAttachCreateOpts builds the attach request. delete_on_termination
// goes in only when the configuration sets it: gophercloud drops a false value,
// which is Nova's default anyway, so unset and false attach the same way.
func volumeAttachCreateOpts(plan *volumeAttachModel) volumeattach.CreateOpts {
	opts := volumeattach.CreateOpts{VolumeID: plan.VolumeID.ValueString()}
	if !plan.Device.IsNull() && !plan.Device.IsUnknown() && plan.Device.ValueString() != "" {
		opts.Device = plan.Device.ValueString()
	}
	if !plan.DeleteOnTermination.IsNull() && !plan.DeleteOnTermination.IsUnknown() {
		opts.DeleteOnTermination = plan.DeleteOnTermination.ValueBool()
	}
	return opts
}

// deleteOnTerminationValue maps the attachment's flag to state. Nova reports
// it from microversion 2.79; a response without it means false, Nova's
// behavior before the flag existed.
func deleteOnTerminationValue(v *bool) types.Bool {
	return types.BoolValue(v != nil && *v)
}

// updateDeleteOnTermination changes the flag with Nova's attachment PUT. The
// path and the body name the same volume: a different volumeId in the body
// asks for a swap-volume, which Nova allows only to admins and only while
// Cinder migrates the volume.
func updateDeleteOnTermination(ctx context.Context, client *gophercloud.ServiceClient, instanceID, volumeID string, value bool) error {
	body := map[string]any{"volumeAttachment": map[string]any{
		"volumeId":              volumeID,
		"delete_on_termination": value,
	}}
	url := client.ServiceURL("servers", instanceID, "os-volume_attachments", volumeID)
	_, err := client.Put(ctx, url, body, nil, &gophercloud.RequestOpts{OkCodes: []int{http.StatusAccepted}})
	return err
}

// waitForVolume best-effort polls a Cinder volume until it reaches target status.
// Any failure (no block-storage service, volume error, timeout) returns silently:
// the Nova attachment call is the source of truth, and this is only a courtesy
// wait so the volume's state has settled before Terraform reports success.
func (r *volumeAttachResource) waitForVolume(ctx context.Context, region, volumeID, target string, timeout time.Duration) {
	bs, err := r.config.ForRegion(region).BlockStorageV3Client()
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
