// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package compute

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/blockstorage/v3/volumes"
	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/servers"
	"github.com/gophercloud/gophercloud/v2/openstack/image/v2/images"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/listplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/platform9/terraform-provider-pcd/internal/clients"
)

// computeMicroversionRebuildVolumeBacked is the first compute microversion
// that rebuilds an instance booted from a volume with a different image:
// Nova then has Cinder rewrite the root volume from the image.
const computeMicroversionRebuildVolumeBacked = "2.93"

// computeMicroversionRootDevice is the first compute microversion at which
// Nova reports OS-EXT-SRV-ATTR:root_device_name, to a caller its policy
// allows (administrators by default). 2.3 only adds fields to the server
// representation.
const computeMicroversionRootDevice = "2.3"

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
// get out of it. A server Nova would refuse returns a *rebuildRefusal. The
// server is read at computeMicroversionRootDevice, so that rootVolume can
// use the root device name Nova reports.
func getRebuildable(ctx context.Context, client *gophercloud.ServiceClient, id string) (*servers.Server, error) {
	server, err := servers.Get(ctx, withMicroversion(client, computeMicroversionRootDevice), id).Extract()
	if err != nil {
		return nil, err
	}
	if server.TaskState != "" {
		return nil, &rebuildRefusal{id: id, status: server.Status, task: server.TaskState}
	}
	switch server.Status {
	case "ACTIVE", "SHUTOFF", "ERROR":
		return server, nil
	default:
		return nil, &rebuildRefusal{id: id, status: server.Status}
	}
}

// rebuildRefusal is an instance Nova would not rebuild now. Its message says
// only what Nova refuses: the way out depends on the caller, since an apply
// runs the resource again but not an action whose trigger already fired.
type rebuildRefusal struct {
	id, status, task string
}

func (e *rebuildRefusal) Error() string {
	switch {
	case e.task != "":
		return fmt.Sprintf("instance %s has task %q in progress; Nova rebuilds an instance only when no task is running",
			e.id, e.task)
	case e.status == "RESCUE":
		return fmt.Sprintf("instance %s is in rescue mode, and Nova rebuilds only an ACTIVE, SHUTOFF or ERROR instance", e.id)
	default:
		return fmt.Sprintf("instance %s is %s, and Nova rebuilds only an ACTIVE, SHUTOFF or ERROR instance", e.id, e.status)
	}
}

// withRebuildAdvice returns err's message and, when err is a rebuildRefusal,
// the caller's advice for its cause: busy for a task in progress, paused for
// a PAUSED or SUSPENDED instance, rescued for one in rescue mode. Any other
// error or status comes back as it is.
func withRebuildAdvice(err error, busy, paused, rescued string) string {
	var r *rebuildRefusal
	if !errors.As(err, &r) {
		return err.Error()
	}
	advice := ""
	switch {
	case r.task != "":
		advice = busy
	case r.status == "PAUSED" || r.status == "SUSPENDED":
		advice = paused
	case r.status == "RESCUE":
		advice = rescued
	}
	if advice == "" {
		return err.Error()
	}
	return err.Error() + ". " + advice
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
// image differs from the one it runs: image_id or image_name for an instance
// booted from an image, the uuid of the root block_device for one that boots
// from a volume (Nova then reimages the root volume through Cinder). It then
// sets plan.ImageID, which is unknown in the plan whenever image_id is not
// configured. A volume-backed instance whose root volume Cinder already shows
// on the new image is not reimaged again, unless it is in ERROR. It returns
// the status the instance settled in after a rebuild, or "" when it sent
// none, so that Update's later power_state step starts from that status.
func (r *instanceResource) rebuildIfImageChanged(ctx context.Context, client *gophercloud.ServiceClient, plan, state *instanceModel, diags *diag.Diagnostics) string {
	id := state.ID.ValueString()
	current := state.ImageID.ValueString()
	rootTarget, rootChanged := rootImageChange(ctx, plan.BlockDevice, state.BlockDevice, diags)
	if diags.HasError() {
		return ""
	}
	target, changed := r.configuredImage(ctx, plan, state, diags)
	if diags.HasError() {
		return ""
	}
	switch {
	case rootChanged && changed && target != current && target != rootTarget:
		diags.AddError("Invalid image", fmt.Sprintf("The root block_device names image %s but image_id or "+
			"image_name names %s; change one of them.", rootTarget, target))
		return ""
	case rootChanged:
		target = rootTarget
	case !changed || target == current:
		setPlannedImageID(plan, current)
		return ""
	}

	before, err := getRebuildable(ctx, client, id)
	if err != nil {
		diags.AddError("compute: rebuilding instance", withRebuildAdvice(err,
			"Apply again once the task finishes",
			"Unpause or resume it first; on pcd_compute_instance, power_state = \"active\" in the same apply "+
				"does that before the rebuild",
			"Unrescue it first: remove its pcd_compute_instance_rescue, or use Unrescue in the PCD UI if it "+
				"was rescued there"))
		return ""
	}
	volumeBacked := serverImageID(before) == ""
	if volumeBacked && !rootChanged {
		diags.AddError("compute: rebuilding instance",
			fmt.Sprintf("Instance %s boots from a volume, so image_id and image_name do not apply to it. To reimage "+
				"its root volume, change the uuid of the block_device with boot_index = 0.", id))
		return ""
	}
	var bs *gophercloud.ServiceClient
	var bsErr error
	if volumeBacked {
		// An apply interrupted, timed out or failed in a later step after
		// Nova reimaged the root volume keeps the old uuid in state, and
		// refresh does not read block_device back, so the next apply plans
		// the same change. Cinder records the image a volume was last
		// written from: a root volume that already holds the new image is
		// not erased again. A root that cannot be told or read is reimaged.
		// An instance in ERROR is reimaged anyway, because a rebuild is how
		// Nova recovers it and Cinder may have finished before Nova failed.
		bs, bsErr = r.config.ForRegion(plan.Region.ValueString()).BlockStorageV3Client()
		if bsErr == nil && before.Status != "ERROR" {
			if root, err := rootVolume(ctx, bs, before); err == nil && root.VolumeImageMetadata["image_id"] == target {
				setPlannedImageID(plan, current)
				return ""
			}
		}
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
	microversion := ""
	if volumeBacked {
		microversion = computeMicroversionRebuildVolumeBacked
	}
	if err := sendRebuild(ctx, client, id, target, microversion); err != nil {
		diags.AddError("compute: rebuilding instance", err.Error()+"\n\n"+rebuildRefusalHint)
		return ""
	}
	server, err := waitForServerSettled(ctx, client, id, rebuildSettleTarget(before.Status), rebuildTimeout)
	if err != nil {
		recovery := "A rebuild that fails leaves the instance in ERROR with the new image already recorded by Nova, " +
			"so the next apply does not rebuild it again. A hard reboot (the pcd_compute_instance_reboot action " +
			"with type = \"HARD\") may bring it back up on the new image; otherwise replace the instance with " +
			"terraform apply -replace=pcd_compute_instance.<name>."
		if volumeBacked {
			recovery = "A reimage that fails leaves the instance in ERROR. Nova records no image on an instance that " +
				"boots from a volume, and refresh does not read block_device back, so the next apply plans the same " +
				"change; it sends the rebuild again unless Cinder shows the root volume already holds the new image and " +
				"the instance is not in ERROR. If it keeps failing, replace the instance with " +
				"terraform apply -replace=pcd_compute_instance.<name>."
		}
		diags.AddError("compute: waiting for instance rebuild", err.Error()+"\n\n"+recovery)
		return ""
	}
	if volumeBacked {
		// Nova keeps no image on a volume-backed server; Cinder records the
		// image a volume was last written from. The root volume keeps its ID
		// through a reimage, so before's attachments and root device name
		// still find it.
		var attached []attachedVolume
		err := bsErr
		if err == nil {
			attached, err = attachedVolumes(ctx, bs, before)
		}
		if err != nil {
			diags.AddWarning("compute: could not confirm the reimaged root volume",
				fmt.Sprintf("Nova reported the rebuild of instance %s finished, but reading its volumes failed: %v. "+
					"Terraform recorded the new uuid for block_device; check the root volume's image in Cinder.", id, err))
			setPlannedImageID(plan, current)
			return server.Status
		}
		if root, err := findRootVolume(before, attached); err == nil {
			if got := root.VolumeImageMetadata["image_id"]; got != target {
				diags.AddError("compute: rebuilding instance",
					fmt.Sprintf("Nova finished the rebuild of instance %s, but its root volume %s holds image %q instead of %q. Nova's last fault: %q.",
						id, root.ID, got, target, server.Fault.Message))
				return ""
			}
			setPlannedImageID(plan, current)
			return server.Status
		}
		// The root cannot be told from a data volume. Nova's rebuild writes
		// only the root volume, so any bootable volume that holds the new
		// image confirms it.
		held := make([]string, 0, len(attached))
		for _, a := range attached {
			if !a.bootable {
				continue
			}
			got := a.volume.VolumeImageMetadata["image_id"]
			if got == target {
				setPlannedImageID(plan, current)
				return server.Status
			}
			if got == "" {
				got = "no image"
			}
			held = append(held, a.volume.ID+": "+got)
		}
		if len(held) == 0 {
			held = append(held, "no bootable volume attached")
		}
		diags.AddError("compute: rebuilding instance",
			fmt.Sprintf("Nova finished the rebuild of instance %s, but none of its bootable volumes holds image %q (%s). Nova's last fault: %q.",
				id, target, strings.Join(held, ", "), server.Fault.Message))
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

// blockDeviceRequiresReplace lets one block_device change through without
// replacing the instance: a new uuid on the root volume (boot_index 0) whose
// source is an image, with every other field and device unchanged. Update
// rebuilds the instance with that image. Any other change replaces it, as it
// did before.
func blockDeviceRequiresReplace(ctx context.Context, req planmodifier.ListRequest, resp *listplanmodifier.RequiresReplaceIfFuncResponse) {
	resp.RequiresReplace = true
	if req.PlanValue.IsNull() || req.PlanValue.IsUnknown() || req.StateValue.IsNull() || req.StateValue.IsUnknown() {
		return
	}
	var plan, state []instanceBlockDeviceModel
	if d := req.PlanValue.ElementsAs(ctx, &plan, false); d.HasError() {
		return
	}
	if d := req.StateValue.ElementsAs(ctx, &state, false); d.HasError() {
		return
	}
	if len(plan) != len(state) {
		return
	}
	changed := -1
	for i := range plan {
		if blockDeviceEqual(plan[i], state[i]) {
			continue
		}
		if changed >= 0 {
			return
		}
		changed = i
	}
	if changed < 0 {
		return
	}
	p, s := plan[changed], state[changed]
	if !isImageRoot(p) || !isImageRoot(s) {
		return
	}
	p.UUID, s.UUID = types.StringNull(), types.StringNull()
	resp.RequiresReplace = !blockDeviceEqual(p, s)
}

// isImageRoot reports whether a block_device is the root volume written from
// an image. A null destination_type is a volume, as Create defaults it. A
// local (ephemeral) root written from an image is image-backed in Nova's
// terms, so a new image on it keeps replacing the instance.
func isImageRoot(m instanceBlockDeviceModel) bool {
	return m.SourceType.ValueString() == string(servers.SourceImage) &&
		m.DestinationType.ValueString() != string(servers.DestinationLocal) &&
		!m.BootIndex.IsNull() && !m.BootIndex.IsUnknown() && m.BootIndex.ValueInt64() == 0
}

func blockDeviceEqual(a, b instanceBlockDeviceModel) bool {
	return a.SourceType.Equal(b.SourceType) && a.UUID.Equal(b.UUID) && a.VolumeSize.Equal(b.VolumeSize) &&
		a.DestinationType.Equal(b.DestinationType) && a.BootIndex.Equal(b.BootIndex) &&
		a.DeleteOnTermination.Equal(b.DeleteOnTermination) && a.VolumeType.Equal(b.VolumeType) &&
		a.GuestFormat.Equal(b.GuestFormat) && a.DeviceType.Equal(b.DeviceType) && a.DiskBus.Equal(b.DiskBus)
}

// rootImageChange returns the new image of the root block_device when the
// plan changes it. blockDeviceRequiresReplace has already turned every other
// block_device change into a replacement, so Update sees only this one.
func rootImageChange(ctx context.Context, planList, stateList types.List, diags *diag.Diagnostics) (string, bool) {
	if planList.IsNull() || planList.IsUnknown() || stateList.IsNull() || stateList.IsUnknown() || planList.Equal(stateList) {
		return "", false
	}
	var plan, state []instanceBlockDeviceModel
	diags.Append(planList.ElementsAs(ctx, &plan, false)...)
	diags.Append(stateList.ElementsAs(ctx, &state, false)...)
	if diags.HasError() || len(plan) != len(state) {
		return "", false
	}
	for i := range state {
		if isImageRoot(state[i]) && isImageRoot(plan[i]) && !plan[i].UUID.Equal(state[i].UUID) {
			return plan[i].UUID.ValueString(), true
		}
	}
	return "", false
}

// attachedVolume is a volume attached to a server: what Cinder reports for
// it, the device it is attached on for that server ("" when Cinder names none
// yet) and whether Cinder flags it bootable.
type attachedVolume struct {
	volume   *volumes.Volume
	device   string
	bootable bool
}

// attachedVolumes reads every volume attached to server. Cinder flags every
// volume written from an image bootable, so a data volume can be bootable,
// and a root volume written by hand need not be.
func attachedVolumes(ctx context.Context, bs *gophercloud.ServiceClient, server *servers.Server) ([]attachedVolume, error) {
	attached := make([]attachedVolume, 0, len(server.AttachedVolumes))
	for _, a := range server.AttachedVolumes {
		v, err := volumes.Get(ctx, bs, a.ID).Extract()
		if err != nil {
			return nil, fmt.Errorf("reading volume %s: %w", a.ID, err)
		}
		if v == nil {
			return nil, fmt.Errorf("reading volume %s: %w", a.ID, clients.ErrNoObject)
		}
		device := ""
		for _, at := range v.Attachments {
			if at.ServerID == server.ID {
				device = at.Device
			}
		}
		attached = append(attached, attachedVolume{volume: v, device: device, bootable: strings.EqualFold(v.Bootable, "true")})
	}
	return attached, nil
}

// rootVolume reads the volumes attached to server and returns the one it
// boots from, as findRootVolume tells it.
func rootVolume(ctx context.Context, bs *gophercloud.ServiceClient, server *servers.Server) (*volumes.Volume, error) {
	attached, err := attachedVolumes(ctx, bs, server)
	if err != nil {
		return nil, err
	}
	return findRootVolume(server, attached)
}

// findRootVolume finds the volume a server boots from among the volumes
// attached to it. When Nova reports the server's root device name, the root
// is the volume on that device, whether or not Cinder flags it bootable. Its
// callers read the server through getRebuildable, which asks for compute
// microversion 2.3, so the name is present whenever Nova's policy returns it.
// Otherwise the root is the volume with the first device name, because Nova
// names the root first on its bus (vda before vdb), but only when every
// attached volume has a device name, all of them are on one bus (a scsi sda
// sorts before a virtio vda) and that first volume is bootable (a data volume
// written from an image is bootable, a root written by hand need not be). In
// any other case findRootVolume refuses rather than guess.
func findRootVolume(server *servers.Server, attached []attachedVolume) (*volumes.Volume, error) {
	if len(attached) == 0 {
		return nil, fmt.Errorf("instance %s has no volume attached, so there is no bootable volume to reimage", server.ID)
	}
	if server.RootDeviceName != nil && *server.RootDeviceName != "" {
		want := strings.TrimPrefix(*server.RootDeviceName, "/dev/")
		for _, a := range attached {
			if strings.TrimPrefix(a.device, "/dev/") == want {
				return a.volume, nil
			}
		}
		return nil, fmt.Errorf("instance %s: Nova reports its root device as %s, but no attached volume is on it (attached: %s)",
			server.ID, *server.RootDeviceName, describeAttached(attached))
	}
	unknown := func(reason string) error {
		return fmt.Errorf("instance %s: Nova did not report its root device, and %s, so the root cannot be told from a "+
			"data volume; reimage it through the root block_device of its pcd_compute_instance, which names the image "+
			"explicitly", server.ID, reason)
	}
	for _, a := range attached {
		if a.device == "" {
			return nil, unknown(fmt.Sprintf("an attachment has no device name yet (%s)", a.volume.ID))
		}
	}
	root, bus := attached[0], deviceBus(attached[0].device)
	for _, a := range attached[1:] {
		if bus == "" || deviceBus(a.device) != bus {
			return nil, unknown(fmt.Sprintf("its volumes are attached on different buses (%s)", describeAttached(attached)))
		}
		if a.device < root.device {
			root = a
		}
	}
	if !root.bootable {
		return nil, unknown(fmt.Sprintf("the first attached device %s is not a bootable volume", root.device))
	}
	return root.volume, nil
}

// deviceBus returns the bus prefix of a device name in the form Nova gives
// it, the name without its trailing drive letters: "vd" for /dev/vda, "sd"
// for /dev/sdb, "xvd" for /dev/xvdc. A name in any other form, or none,
// returns "", which matches no other device.
func deviceBus(device string) string {
	name := strings.TrimPrefix(device, "/dev/")
	for _, prefix := range []string{"xvd", "vd", "sd", "hd"} {
		letters, ok := strings.CutPrefix(name, prefix)
		if ok && letters != "" && strings.Trim(letters, "abcdefghijklmnopqrstuvwxyz") == "" {
			return prefix
		}
	}
	return ""
}

// describeAttached lists attached volumes for an error: "vol-1 on /dev/vda,
// vol-2 on /dev/sda".
func describeAttached(attached []attachedVolume) string {
	parts := make([]string, 0, len(attached))
	for _, a := range attached {
		if a.device == "" {
			parts = append(parts, a.volume.ID+" (no device)")
			continue
		}
		parts = append(parts, a.volume.ID+" on "+a.device)
	}
	return strings.Join(parts, ", ")
}
