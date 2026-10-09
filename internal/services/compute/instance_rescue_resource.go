// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package compute

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/servers"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/platform9/terraform-provider-pcd/internal/clients"
	"github.com/platform9/terraform-provider-pcd/internal/tfstate"
)

var (
	_ resource.Resource                = (*instanceRescueResource)(nil)
	_ resource.ResourceWithConfigure   = (*instanceRescueResource)(nil)
	_ resource.ResourceWithImportState = (*instanceRescueResource)(nil)
)

// computeMicroversionRescue is the first compute microversion that rescues
// an instance booted from a volume; below it Nova refuses such an instance.
const computeMicroversionRescue = "2.87"

// rescueTimeout bounds the waits for rescue and unrescue.
const rescueTimeout = 30 * time.Minute

// NewInstanceRescueResource is the factory registered with the provider.
func NewInstanceRescueResource() resource.Resource {
	return &instanceRescueResource{}
}

type instanceRescueResource struct {
	config *clients.Config
}

type instanceRescueModel struct {
	ID            types.String `tfsdk:"id"`
	InstanceID    types.String `tfsdk:"instance_id"`
	RescueImageID types.String `tfsdk:"rescue_image_id"`
	Region        types.String `tfsdk:"region"`
}

func (r *instanceRescueResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_compute_instance_rescue"
}

func (r *instanceRescueResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	forceNew := []planmodifier.String{stringplanmodifier.RequiresReplace()}
	stable := []planmodifier.String{stringplanmodifier.UseStateForUnknown()}
	resp.Schema = schema.Schema{
		MarkdownDescription: "Puts an instance into rescue mode while this resource exists, as the PCD UI's Rescue and " +
			"Unrescue actions do. Nova boots the instance from a rescue image and attaches its original root disk as a " +
			"second disk, so a broken system can be repaired; destroying this resource unrescues the instance, which " +
			"returns it to `ACTIVE` on its own disk. An instance can be rescued from `ACTIVE`, `SHUTOFF` or `ERROR`.\n\n" +
			"While the instance is rescued, Nova refuses changes to it (metadata, resize, power state, rebuild). Removing " +
			"this resource and changing the instance in the same apply works: Terraform unrescues the instance before it " +
			"updates it. After an unrescue the instance is " +
			"`ACTIVE`, so an instance with `power_state = \"shutoff\"` is stopped again by the next apply. If the " +
			"instance leaves rescue mode outside Terraform, the next refresh drops this resource from state and the next " +
			"apply rescues the instance again.\n\n" +
			"While the instance is rescued, `power_state` on its `pcd_compute_instance` keeps its last value: an apply " +
			"that changes `power_state` fails with an explanation, and an apply that leaves it unchanged skips the " +
			"power steps.\n\n" +
			"Nova generates a password for the rescue system; like the PCD UI, this resource does not keep it. Log in " +
			"with what the rescue image provides, such as the instance's key pair.\n\n" +
			"To rescue an instance that boots from a volume, set `rescue_image_id` to an image with the properties " +
			"`hw_rescue_device` and `hw_rescue_bus`, and the instance's host must report the `COMPUTE_RESCUE_BFV` trait. " +
			"The request uses compute microversion 2.87, so an image with those properties also gets Nova's stable-device " +
			"rescue for an instance booted from an image.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{Computed: true, MarkdownDescription: "The instance ID, as `instance_id`.", PlanModifiers: stable},
			"instance_id": schema.StringAttribute{Required: true,
				MarkdownDescription: "The ID of the instance to rescue. Changing this unrescues the old instance and rescues the new one.",
				PlanModifiers:       forceNew},
			"rescue_image_id": schema.StringAttribute{Optional: true,
				MarkdownDescription: "The image to boot the rescue system from. Omit it to use the instance's own image; an " +
					"instance that boots from a volume needs one (see above). Nova does not report it, so it is not read " +
					"back and stays unset after an import. Changing this rescues the instance again with the new image.",
				PlanModifiers: forceNew},
			"region": schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "The region. Defaults to the provider's region. Changing this forces a new resource.", PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace(), stringplanmodifier.UseStateForUnknown()}},
		},
	}
}

func (r *instanceRescueResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.config = configureClient(req.ProviderData, &resp.Diagnostics)
}

func (r *instanceRescueResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan instanceRescueModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	client, err := r.config.ForRegion(plan.Region.ValueString()).ComputeV2Client()
	if err != nil {
		resp.Diagnostics.AddError("compute: building v2 client", err.Error())
		return
	}
	id := plan.InstanceID.ValueString()
	// The response carries the generated rescue password; it is not kept.
	if res := servers.Rescue(ctx, withMicroversion(client, computeMicroversionRescue), id, servers.RescueOpts{
		RescueImageRef: plan.RescueImageID.ValueString(),
	}); res.Err != nil {
		resp.Diagnostics.AddError("compute: rescuing instance "+id, res.Err.Error())
		return
	}

	plan.ID = types.StringValue(id)
	if plan.Region.IsNull() || plan.Region.IsUnknown() {
		plan.Region = types.StringValue(r.config.Region)
	}
	if !tfstate.RecordCreated(ctx, resp, &plan) {
		return
	}

	// Nova sets the rescuing task before it answers, so the first settled
	// status is the outcome: RESCUE, or the status a failed rescue fell back to.
	server, err := waitForServerSettled(ctx, client, id, []string{"RESCUE", "ACTIVE", "SHUTOFF"}, rescueTimeout)
	if err != nil {
		resp.Diagnostics.AddError("compute: waiting for instance rescue", err.Error())
		return
	}
	if server.Status != "RESCUE" {
		resp.Diagnostics.AddError("compute: rescuing instance "+id,
			fmt.Sprintf("Nova finished without rescuing the instance; it is %s. Nova's last fault: %q. "+
				"`openstack server event list %s` shows the failed rescue.", server.Status, server.Fault.Message, id))
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *instanceRescueResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state instanceRescueModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	// A row with no ID names no instance; a read would reach the server list.
	if tfstate.DropRowWithoutID(ctx, resp, state.ID, "rescue", "") {
		return
	}
	client, err := r.config.ForRegion(state.Region.ValueString()).ComputeV2Client()
	if err != nil {
		resp.Diagnostics.AddError("compute: building v2 client", err.Error())
		return
	}
	id := state.InstanceID.ValueString()
	server, err := servers.Get(ctx, client, id).Extract()
	if err != nil {
		if gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
			resp.Diagnostics.AddWarning("Instance not found",
				fmt.Sprintf("Instance %s no longer exists; its rescue was removed from state.", id))
			resp.State.RemoveResource(ctx)
			return
		}
		resp.Diagnostics.AddError("compute: reading instance", err.Error())
		return
	}
	// An instance in the middle of a task keeps its rescue: rescuing and
	// unrescuing both report a task until Nova is done.
	if server.TaskState == "" && server.Status != "RESCUE" {
		resp.Diagnostics.AddWarning("Instance not in rescue mode",
			fmt.Sprintf("Instance %s is %s, not in rescue mode, so its rescue was removed from state.", id, server.Status))
		resp.State.RemoveResource(ctx)
		return
	}
	state.ID = types.StringValue(id)
	if state.Region.IsNull() || state.Region.IsUnknown() {
		state.Region = types.StringValue(r.config.Region)
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

// Update is required by the interface but never invoked: every input forces
// replacement.
func (r *instanceRescueResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan instanceRescueModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *instanceRescueResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state instanceRescueModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if tfstate.SkipDeleteWithoutID(resp, state.ID, "rescue", "") {
		return
	}
	client, err := r.config.ForRegion(state.Region.ValueString()).ComputeV2Client()
	if err != nil {
		resp.Diagnostics.AddError("compute: building v2 client", err.Error())
		return
	}
	id := state.InstanceID.ValueString()
	if res := servers.Unrescue(ctx, client, id); res.Err != nil {
		if gophercloud.ResponseCodeIs(res.Err, http.StatusNotFound) {
			return
		}
		// 409: Nova unrescues only a rescued instance. One that already left
		// rescue mode needs nothing.
		if gophercloud.ResponseCodeIs(res.Err, http.StatusConflict) {
			if server, err := servers.Get(ctx, client, id).Extract(); err == nil && server.TaskState == "" && server.Status != "RESCUE" {
				return
			}
		}
		resp.Diagnostics.AddError("compute: unrescuing instance "+id, res.Err.Error())
		return
	}
	server, err := waitForServerSettled(ctx, client, id, []string{"ACTIVE", "RESCUE"}, rescueTimeout)
	if err != nil {
		if _, gerr := servers.Get(ctx, client, id).Extract(); gophercloud.ResponseCodeIs(gerr, http.StatusNotFound) {
			return
		}
		resp.Diagnostics.AddError("compute: waiting for instance unrescue", err.Error())
		return
	}
	if server.Status == "RESCUE" {
		resp.Diagnostics.AddError("compute: unrescuing instance "+id,
			fmt.Sprintf("Nova finished without leaving rescue mode. Nova's last fault: %q. "+
				"`openstack server event list %s` shows the failed unrescue.", server.Fault.Message, id))
	}
}

// ImportState takes the instance ID. Read then keeps the resource only if
// the instance is in rescue mode.
func (r *instanceRescueResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), req.ID)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("instance_id"), req.ID)...)
}
