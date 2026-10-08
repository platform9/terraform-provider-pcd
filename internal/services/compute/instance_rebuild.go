// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package compute

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/servers"
	"github.com/gophercloud/gophercloud/v2/openstack/image/v2/images"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/platform9/terraform-provider-pcd/internal/clients"
)

// rebuildTimeout bounds the wait for Nova to finish a rebuild, matching the
// provider's other instance waits.
const rebuildTimeout = 30 * time.Minute

// rebuildRefusalHint follows a refused rebuild request: Nova's own message
// names the cause, and these are the refusals a configuration can run into.
const rebuildRefusalHint = "Nova refuses a rebuild of an instance that is locked, that has a virtual TPM, " +
	"attached shares or accelerators, or a task in progress, and a rebuild to an image that is not active " +
	"or whose min_disk exceeds the flavor's disk."

// serverImageID returns the ID of the image a server was booted from, or ""
// for a server that boots from a volume (Nova reports its image as "").
func serverImageID(server *servers.Server) string {
	if server == nil || server.Image == nil {
		return ""
	}
	id, _ := server.Image["id"].(string)
	return id
}

// getRebuildable reads a server and checks that Nova will accept a rebuild of
// it now: Nova rebuilds only an ACTIVE, SHUTOFF or ERROR server with no task
// in progress, and answers anything else with a 409 that does not say how to
// get out of it.
func getRebuildable(ctx context.Context, client *gophercloud.ServiceClient, id string) (*servers.Server, error) {
	server, err := servers.Get(ctx, client, id).Extract()
	if err != nil {
		return nil, err
	}
	if server.TaskState != "" {
		return nil, fmt.Errorf("instance %s has task %q in progress; Nova rebuilds an instance only "+
			"when no task is running. Apply again once the task finishes", id, server.TaskState)
	}
	switch server.Status {
	case "ACTIVE", "SHUTOFF", "ERROR":
		return server, nil
	case "PAUSED", "SUSPENDED":
		return nil, fmt.Errorf("instance %s is %s, and Nova rebuilds only an ACTIVE, SHUTOFF or ERROR "+
			"instance. Unpause or resume it first; on pcd_compute_instance, power_state = \"active\" in "+
			"the same apply does that before the rebuild", id, server.Status)
	case "RESCUE":
		return nil, fmt.Errorf("instance %s is in rescue mode, and Nova rebuilds only an ACTIVE, SHUTOFF "+
			"or ERROR instance. Unrescue it first: remove its pcd_compute_instance_rescue in a separate "+
			"apply, or use Unrescue in the PCD UI if it was rescued there", id)
	default:
		return nil, fmt.Errorf("instance %s is %s, and Nova rebuilds only an ACTIVE, SHUTOFF or ERROR "+
			"instance", id, server.Status)
	}
}

// sendRebuild asks Nova to reimage server id with imageID. Only the image is
// sent, so Nova keeps the instance's name, metadata, key pair, user data,
// ports and attached volumes. microversion pins this one request ("" keeps
// the client's base version).
func sendRebuild(ctx context.Context, client *gophercloud.ServiceClient, id, imageID, microversion string) error {
	c := client
	if microversion != "" {
		c = withMicroversion(client, microversion)
	}
	if res := servers.Rebuild(ctx, c, id, servers.RebuildOpts{ImageRef: imageID}); res.Err != nil {
		return res.Err
	}
	return nil
}

// rebuildSettleTarget is the status a rebuild ends in: Nova restores a stopped
// server to SHUTOFF and leaves every other one ACTIVE (a server rebuilt out of
// ERROR comes back ACTIVE). A stopped server reports ACTIVE with no task for
// a moment before Nova powers it off again, so waiting for "ACTIVE or
// SHUTOFF" would return too early.
func rebuildSettleTarget(status string) []string {
	if status == "SHUTOFF" {
		return []string{"SHUTOFF"}
	}
	return []string{"ACTIVE"}
}

// imageBlockDeviceMapping returns the block_device_mapping property Nova
// writes on a snapshot of a volume-backed instance: one entry per volume, each
// naming the Cinder snapshot it was taken to. Glance stores it as a JSON
// string. An image without the property returns no entries.
func imageBlockDeviceMapping(img *images.Image) ([]map[string]any, error) {
	raw, ok := img.Properties["block_device_mapping"]
	if !ok || raw == nil {
		return nil, nil
	}
	var entries []map[string]any
	switch v := raw.(type) {
	case string:
		if v == "" {
			return nil, nil
		}
		if err := json.Unmarshal([]byte(v), &entries); err != nil {
			return nil, fmt.Errorf("image %s: reading its block_device_mapping property: %w", img.ID, err)
		}
	case []any:
		for _, e := range v {
			if m, ok := e.(map[string]any); ok {
				entries = append(entries, m)
			}
		}
	default:
		return nil, fmt.Errorf("image %s: its block_device_mapping property is a %T, not a list", img.ID, raw)
	}
	return entries, nil
}

// checkRebuildTarget refuses a target the rebuild would turn into a broken
// instance: a snapshot of a volume-backed instance is a zero-byte image whose
// data lives in Cinder snapshots, which a rebuild cannot use. The PCD UI does
// not offer such images as rebuild targets either.
func checkRebuildTarget(ctx context.Context, imgClient *gophercloud.ServiceClient, imageID string) error {
	img, err := clients.RequireObject(images.Get(ctx, imgClient, imageID).Extract())
	if err != nil {
		if gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
			return fmt.Errorf("image %s does not exist", imageID)
		}
		return fmt.Errorf("reading image %s: %w", imageID, err)
	}
	entries, err := imageBlockDeviceMapping(img)
	if err != nil {
		return err
	}
	if len(entries) > 0 {
		return fmt.Errorf("image %s (%q) is a snapshot of a volume-backed instance: it holds no disk data of "+
			"its own, so an instance rebuilt from it would not boot. Boot a new instance from it instead", imageID, img.Name)
	}
	return nil
}

// rebuildIfImageChanged rebuilds the instance in place when the configured
// image differs from the one it runs, then sets plan.ImageID, which is
// unknown in the plan whenever image_id is not configured. It returns the
// status the instance settled in after a rebuild, or "" when it sent none,
// so that Update's later power_state step starts from that status.
func (r *instanceResource) rebuildIfImageChanged(ctx context.Context, client *gophercloud.ServiceClient, plan, state *instanceModel, diags *diag.Diagnostics) string {
	id := state.ID.ValueString()
	current := state.ImageID.ValueString()
	target, changed := r.configuredImage(ctx, plan, state, diags)
	if diags.HasError() {
		return ""
	}
	if !changed || target == current {
		setPlannedImageID(plan, current)
		return ""
	}

	before, err := getRebuildable(ctx, client, id)
	if err != nil {
		diags.AddError("compute: rebuilding instance", err.Error())
		return ""
	}
	if serverImageID(before) == "" {
		diags.AddError("compute: rebuilding instance",
			fmt.Sprintf("Instance %s boots from a volume, so image_id and image_name do not apply to it: "+
				"they name the image of an instance booted from an image.", id))
		return ""
	}
	imgClient, err := r.config.ForRegion(plan.Region.ValueString()).ImageV2Client()
	if err != nil {
		diags.AddError("compute: building image v2 client", err.Error())
		return ""
	}
	if err := checkRebuildTarget(ctx, imgClient, target); err != nil {
		diags.AddError("compute: rebuilding instance", err.Error())
		return ""
	}
	if err := sendRebuild(ctx, client, id, target, ""); err != nil {
		diags.AddError("compute: rebuilding instance", err.Error()+"\n\n"+rebuildRefusalHint)
		return ""
	}
	server, err := waitForServerSettled(ctx, client, id, rebuildSettleTarget(before.Status), rebuildTimeout)
	if err != nil {
		diags.AddError("compute: waiting for instance rebuild", err.Error()+
			"\n\nA rebuild that fails leaves the instance in ERROR with the new image already recorded by Nova, "+
			"so the next apply does not rebuild it again. A hard reboot (the pcd_compute_instance_reboot action "+
			"with type = \"HARD\") may bring it back up on the new image; otherwise replace the instance with "+
			"terraform apply -replace=pcd_compute_instance.<name>.")
		return ""
	}
	if got := serverImageID(server); got != target {
		diags.AddError("compute: rebuilding instance",
			fmt.Sprintf("Nova finished the rebuild of instance %s, but it runs image %q instead of %q. Nova's last fault: %q.",
				id, got, target, server.Fault.Message))
		return ""
	}
	setPlannedImageID(plan, target)
	return server.Status
}

// configuredImage resolves the image the configuration asks for and reports
// whether it may differ from the one in state. image_name is resolved only
// when it changed, so an image re-uploaded under the same name never
// rebuilds an instance whose configuration did not change.
func (r *instanceResource) configuredImage(ctx context.Context, plan, state *instanceModel, diags *diag.Diagnostics) (string, bool) {
	nameSet := !plan.ImageName.IsNull() && !plan.ImageName.IsUnknown() && plan.ImageName.ValueString() != ""
	idSet := !plan.ImageID.IsNull() && !plan.ImageID.IsUnknown() && plan.ImageID.ValueString() != ""
	switch {
	case nameSet && idSet:
		diags.AddError("Invalid image", "Set only one of image_id or image_name.")
		return "", false
	case nameSet && plan.ImageName.Equal(state.ImageName):
		return "", false
	case nameSet:
		imgClient, err := r.config.ForRegion(plan.Region.ValueString()).ImageV2Client()
		if err != nil {
			diags.AddError("compute: building image v2 client", err.Error())
			return "", false
		}
		id, err := imageIDFromName(ctx, imgClient, plan.ImageName.ValueString())
		if err != nil {
			diags.AddError("compute: resolving image", err.Error())
			return "", false
		}
		return id, true
	case idSet:
		return plan.ImageID.ValueString(), true
	default:
		return "", false
	}
}

// setPlannedImageID fills image_id when the plan left it unknown (image_name
// or no image configured); a configured image_id is already what the
// instance runs.
func setPlannedImageID(plan *instanceModel, id string) {
	if plan.ImageID.IsUnknown() || plan.ImageID.IsNull() {
		plan.ImageID = types.StringValue(id)
	}
}

// refreshImageName follows a rebuild done outside Terraform: once Read has
// recorded a new image ID, image_name takes that image's Glance name, so a
// configuration that names its image shows the change as drift and the next
// apply rebuilds the instance back. When Glance cannot answer, the refresh
// warns and keeps priorImageID, so the next refresh asks again instead of
// hiding the change for good.
func (r *instanceResource) refreshImageName(ctx context.Context, m *instanceModel, priorImageID string, diags *diag.Diagnostics) {
	imgClient, err := r.config.ForRegion(m.Region.ValueString()).ImageV2Client()
	if err == nil {
		var img *images.Image
		img, err = clients.RequireObject(images.Get(ctx, imgClient, m.ImageID.ValueString()).Extract())
		if err == nil {
			m.ImageName = types.StringValue(img.Name)
			return
		}
		if gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
			diags.AddWarning("Instance image not found",
				fmt.Sprintf("Instance %s now runs image %s, which no longer exists, so image_name keeps %q.",
					m.ID.ValueString(), m.ImageID.ValueString(), m.ImageName.ValueString()))
			return
		}
	}
	diags.AddWarning("Instance image not refreshed",
		fmt.Sprintf("Instance %s now runs image %s, but reading that image failed: %v. image_id and image_name keep "+
			"%q and %q until a refresh can read it.", m.ID.ValueString(), m.ImageID.ValueString(), err,
			priorImageID, m.ImageName.ValueString()))
	m.ImageID = types.StringValue(priorImageID)
}
