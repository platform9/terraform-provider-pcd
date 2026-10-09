// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package compute

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/servers"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/listplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

// volumeRebuildServerJSON renders a volume-backed server: no image, and the
// volumes Nova reports as attached.
func volumeRebuildServerJSON(status, task string) string {
	taskJSON := "null"
	if task != "" {
		taskJSON = fmt.Sprintf("%q", task)
	}
	return fmt.Sprintf(`{"server": {"id": "srv-1", "name": "vm-1", "status": %q, "OS-EXT-STS:task_state": %s,
		"image": "", "metadata": {}, "addresses": {}, "security_groups": [{"name": "default"}],
		"OS-EXT-AZ:availability_zone": "nova", "os-extended-volumes:volumes_attached": [{"id": "vol-data"}, {"id": "vol-root"}],
		"fault": {"message": "reimage failed"}}}`, status, taskJSON)
}

// withRootDeviceName adds the root device name Nova reports to an admin to a
// rendered server.
func withRootDeviceName(server, device string) string {
	return strings.Replace(server, `"image": "",`, fmt.Sprintf(`"image": "", "OS-EXT-SRV-ATTR:root_device_name": %q,`, device), 1)
}

// cinderVolumeJSON renders GET /volumes/<id> for a volume attached to srv-1.
func cinderVolumeJSON(id, bootable, device, image string) string {
	meta := `{}`
	if image != "" {
		meta = fmt.Sprintf(`{"image_id": %q}`, image)
	}
	return fmt.Sprintf(`{"volume": {"id": %q, "status": "in-use", "bootable": %q, "volume_image_metadata": %s,
		"attachments": [{"server_id": "srv-1", "device": %q, "volume_id": %q}]}}`, id, bootable, meta, device, id)
}

func blockDeviceType(t *testing.T) types.ObjectType {
	t.Helper()
	return rebuildSchema(t).Blocks["block_device"].(schema.ListNestedBlock).NestedObject.Type().(types.ObjectType)
}

// blockDeviceList builds a block_device list; each entry's fields override a
// root device written from image img-1 onto a 10 GiB volume.
func blockDeviceList(t *testing.T, entries ...map[string]attr.Value) types.List {
	t.Helper()
	objType := blockDeviceType(t)
	var items []attr.Value
	for _, e := range entries {
		vals := map[string]attr.Value{
			"source_type": types.StringValue("image"), "uuid": types.StringValue("img-1"), "volume_size": types.Int64Value(10),
			"destination_type": types.StringValue("volume"), "boot_index": types.Int64Value(0), "delete_on_termination": types.BoolValue(true),
			"volume_type": types.StringNull(), "guest_format": types.StringNull(), "device_type": types.StringNull(), "disk_bus": types.StringNull(),
		}
		for k, v := range e {
			vals[k] = v
		}
		o, d := types.ObjectValue(objType.AttrTypes, vals)
		if d.HasError() {
			t.Fatal(d)
		}
		items = append(items, o)
	}
	l, d := types.ListValue(objType, items)
	if d.HasError() {
		t.Fatal(d)
	}
	return l
}

func requiresReplace(t *testing.T, plan, state types.List) bool {
	t.Helper()
	var resp listplanmodifier.RequiresReplaceIfFuncResponse
	blockDeviceRequiresReplace(context.Background(), planmodifier.ListRequest{PlanValue: plan, StateValue: state}, &resp)
	if resp.Diagnostics.HasError() {
		t.Fatal(resp.Diagnostics)
	}
	return resp.RequiresReplace
}

// Only a new image on the root device rebuilds in place; every other
// block_device change still replaces the instance.
func TestBlockDeviceRequiresReplace(t *testing.T) {
	root := map[string]attr.Value{}
	data := map[string]attr.Value{"source_type": types.StringValue("blank"), "uuid": types.StringNull(), "boot_index": types.Int64Value(-1)}
	state := blockDeviceList(t, root, data)
	for _, tc := range []struct {
		name string
		plan types.List
		want bool
	}{
		{"new root image", blockDeviceList(t, map[string]attr.Value{"uuid": types.StringValue("img-2")}, data), false},
		{"root image not known yet", blockDeviceList(t, map[string]attr.Value{"uuid": types.StringUnknown()}, data), false},
		{"new root image and a bigger volume", blockDeviceList(t, map[string]attr.Value{"uuid": types.StringValue("img-2"), "volume_size": types.Int64Value(20)}, data), true},
		{"bigger data volume", blockDeviceList(t, root, map[string]attr.Value{"source_type": types.StringValue("blank"), "uuid": types.StringNull(), "boot_index": types.Int64Value(-1), "volume_size": types.Int64Value(20)}), true},
		{"device added", blockDeviceList(t, root, data, data), true},
		{"whole list unknown", types.ListUnknown(blockDeviceType(t)), true},
	} {
		if got := requiresReplace(t, tc.plan, state); got != tc.want {
			t.Errorf("%s: requires replace = %v, want %v", tc.name, got, tc.want)
		}
	}

	volRoot := blockDeviceList(t, map[string]attr.Value{"source_type": types.StringValue("volume"), "uuid": types.StringValue("vol-1")})
	if !requiresReplace(t, blockDeviceList(t, map[string]attr.Value{"source_type": types.StringValue("volume"), "uuid": types.StringValue("vol-2")}), volRoot) {
		t.Error("swapping the root volume for another one must replace the instance")
	}
	isoRoot := blockDeviceList(t, map[string]attr.Value{"boot_index": types.Int64Value(1), "device_type": types.StringValue("cdrom")})
	if !requiresReplace(t, blockDeviceList(t, map[string]attr.Value{"boot_index": types.Int64Value(1), "device_type": types.StringValue("cdrom"), "uuid": types.StringValue("iso-2")}), isoRoot) {
		t.Error("a new image on a non-root device must replace the instance")
	}
	localRoot := blockDeviceList(t, map[string]attr.Value{"destination_type": types.StringValue("local")})
	if !requiresReplace(t, blockDeviceList(t, map[string]attr.Value{"destination_type": types.StringValue("local"), "uuid": types.StringValue("img-2")}), localRoot) {
		t.Error("a new image on a local root must replace the instance")
	}
	defaultRoot := blockDeviceList(t, map[string]attr.Value{"destination_type": types.StringNull()})
	if requiresReplace(t, blockDeviceList(t, map[string]attr.Value{"destination_type": types.StringNull(), "uuid": types.StringValue("img-2")}), defaultRoot) {
		t.Error("a new image on a root whose destination_type defaults to volume must rebuild it in place")
	}
}

// Update reimages only a root volume: a new image on a local root has
// already been planned as a replacement.
func TestRootImageChange(t *testing.T) {
	rebuildFastPolls(t)
	for _, tc := range []struct {
		name    string
		dest    types.String
		image   string
		changed bool
	}{
		{"root volume", types.StringValue("volume"), "img-2", true},
		{"destination_type defaults to volume", types.StringNull(), "img-2", true},
		{"local root", types.StringValue("local"), "", false},
	} {
		var diags diag.Diagnostics
		state := blockDeviceList(t, map[string]attr.Value{"destination_type": tc.dest})
		plan := blockDeviceList(t, map[string]attr.Value{"destination_type": tc.dest, "uuid": types.StringValue("img-2")})
		image, changed := rootImageChange(context.Background(), plan, state, &diags)
		if diags.HasError() {
			t.Fatalf("%s: %v", tc.name, diags)
		}
		if image != tc.image || changed != tc.changed {
			t.Errorf("%s: rootImageChange = (%q, %v), want (%q, %v)", tc.name, image, changed, tc.image, tc.changed)
		}
	}
}

// The rule is wired to the block: a new root image plans an update, and a
// new data volume size still plans a replacement.
func TestBlockDeviceModifierWiring(t *testing.T) {
	mods := rebuildSchema(t).Blocks["block_device"].(schema.ListNestedBlock).PlanModifiers
	if len(mods) != 1 {
		t.Fatalf("block_device has %d plan modifiers, want 1", len(mods))
	}
	exists := tftypes.NewValue(tftypes.String, "exists")
	plan := func(l types.List) bool {
		resp := planmodifier.ListResponse{PlanValue: l}
		mods[0].PlanModifyList(context.Background(), planmodifier.ListRequest{
			State: tfsdk.State{Raw: exists}, Plan: tfsdk.Plan{Raw: exists},
			StateValue: blockDeviceList(t, map[string]attr.Value{}), PlanValue: l,
		}, &resp)
		return resp.RequiresReplace
	}
	if plan(blockDeviceList(t, map[string]attr.Value{"uuid": types.StringValue("img-2")})) {
		t.Error("a new root image plans a replacement")
	}
	if !plan(blockDeviceList(t, map[string]attr.Value{"volume_size": types.Int64Value(20)})) {
		t.Error("a bigger root volume no longer plans a replacement")
	}
}

// A new root image on a volume-backed instance rebuilds it at 2.93 and
// checks the result in Cinder, since Nova reports no image for it.
func TestUpdateReimagesRootVolume(t *testing.T) {
	rebuildFastPolls(t)
	f := &rebuildFake{t: t,
		before: volumeRebuildServerJSON("ACTIVE", ""),
		after: []string{
			volumeRebuildServerJSON("REBUILD", "rebuilding"),
			volumeRebuildServerJSON("ACTIVE", ""),
		},
		images: map[string]string{"img-2": rebuildImageJSON("img-2", "ubuntu", "")},
		volumes: map[string]string{
			"vol-data": cinderVolumeJSON("vol-data", "false", "/dev/vdb", ""),
			"vol-root": cinderVolumeJSON("vol-root", "true", "/dev/vda", "img-2"),
		},
	}
	r := f.start()
	s := rebuildSchema(t)
	state := rebuildTestModel(t, s, types.StringValue(""), types.StringNull())
	state.BlockDevice = blockDeviceList(t, map[string]attr.Value{})
	plan := rebuildTestModel(t, s, types.StringUnknown(), types.StringNull())
	plan.BlockDevice = blockDeviceList(t, map[string]attr.Value{"uuid": types.StringValue("img-2")})

	resp, got := rebuildUpdate(t, r, s, plan, state)
	if resp.Diagnostics.HasError() {
		t.Fatalf("update: %v", resp.Diagnostics)
	}
	if len(f.rebuilds) != 1 || f.versions[0] != "2.93" {
		t.Fatalf("rebuilds %v at microversions %v; want one at 2.93", f.rebuilds, f.versions)
	}
	body, _ := json.Marshal(f.rebuilds[0])
	if string(body) != `{"rebuild":{"imageRef":"img-2"}}` {
		t.Errorf("rebuild body %s", body)
	}
	if got.ImageID.ValueString() != "" || got.ID.ValueString() != "srv-1" {
		t.Errorf("state image_id=%q id=%s, want \"\" and srv-1", got.ImageID.ValueString(), got.ID)
	}
}

// A reimage that left the root volume on its old image fails the update.
func TestUpdateRootVolumeKeptOldImage(t *testing.T) {
	rebuildFastPolls(t)
	f := &rebuildFake{t: t,
		before: volumeRebuildServerJSON("ACTIVE", ""), after: []string{volumeRebuildServerJSON("ACTIVE", "")},
		images: map[string]string{"img-2": rebuildImageJSON("img-2", "ubuntu", "")},
		volumes: map[string]string{
			"vol-data": cinderVolumeJSON("vol-data", "false", "/dev/vdb", ""),
			"vol-root": cinderVolumeJSON("vol-root", "true", "/dev/vda", "img-1"),
		},
	}
	r := f.start()
	s := rebuildSchema(t)
	state := rebuildTestModel(t, s, types.StringValue(""), types.StringNull())
	state.BlockDevice = blockDeviceList(t, map[string]attr.Value{})
	plan := rebuildTestModel(t, s, types.StringUnknown(), types.StringNull())
	plan.BlockDevice = blockDeviceList(t, map[string]attr.Value{"uuid": types.StringValue("img-2")})
	resp, _ := rebuildUpdate(t, r, s, plan, state)
	if !resp.Diagnostics.HasError() || !strings.Contains(resp.Diagnostics.Errors()[0].Detail(), "vol-root") {
		t.Fatalf("diagnostics %v, want an error naming the root volume", resp.Diagnostics)
	}
}

// A reimage that ends in ERROR fails the update. Nova records no image on a
// volume-backed instance and refresh does not read block_device back, so the
// next apply sends the same rebuild again: the error says so, and gives none
// of the image path's advice, which rests on Nova recording the new image.
func TestUpdateRootVolumeReimageEndingInError(t *testing.T) {
	rebuildFastPolls(t)
	f := &rebuildFake{t: t,
		before: volumeRebuildServerJSON("ACTIVE", ""),
		after: []string{
			volumeRebuildServerJSON("REBUILD", "rebuilding"),
			volumeRebuildServerJSON("ERROR", ""),
		},
		images: map[string]string{"img-2": rebuildImageJSON("img-2", "ubuntu", "")},
	}
	r := f.start()
	s := rebuildSchema(t)
	state := rebuildTestModel(t, s, types.StringValue(""), types.StringNull())
	state.BlockDevice = blockDeviceList(t, map[string]attr.Value{})
	plan := rebuildTestModel(t, s, types.StringUnknown(), types.StringNull())
	plan.BlockDevice = blockDeviceList(t, map[string]attr.Value{"uuid": types.StringValue("img-2")})
	resp, _ := rebuildUpdate(t, r, s, plan, state)
	if !resp.Diagnostics.HasError() {
		t.Fatal("update succeeded although the reimage ended in ERROR")
	}
	if len(f.rebuilds) != 1 || f.versions[0] != "2.93" {
		t.Fatalf("rebuilds %v at microversions %v; want one at 2.93", f.rebuilds, f.versions)
	}
	detail := resp.Diagnostics.Errors()[0].Detail()
	for _, want := range []string{"the next apply sends the same rebuild again", "terraform apply -replace="} {
		if !strings.Contains(detail, want) {
			t.Errorf("error %q does not mention %q", detail, want)
		}
	}
	for _, imagePath := range []string{"already recorded by Nova", "does not rebuild it again", `type = "HARD"`} {
		if strings.Contains(detail, imagePath) {
			t.Errorf("error %q gives the image path's advice %q", detail, imagePath)
		}
	}
}

// image_id on a volume-backed instance points at the block_device path.
func TestUpdateImageIDOnVolumeBackedInstance(t *testing.T) {
	rebuildFastPolls(t)
	f := &rebuildFake{t: t, before: volumeRebuildServerJSON("ACTIVE", "")}
	r := f.start()
	s := rebuildSchema(t)
	resp, _ := rebuildUpdate(t, r, s,
		rebuildTestModel(t, s, types.StringValue("img-2"), types.StringNull()),
		rebuildTestModel(t, s, types.StringValue(""), types.StringNull()))
	if !resp.Diagnostics.HasError() || !strings.Contains(resp.Diagnostics.Errors()[0].Detail(), "block_device") {
		t.Fatalf("diagnostics %v, want an error pointing at block_device", resp.Diagnostics)
	}
	if len(f.rebuilds) != 0 {
		t.Fatal("a rebuild request was sent")
	}
}

// The root volume is the bootable volume on the root device Nova reports.
// Without it, it is the only bootable volume, or the first device name when
// every bootable volume is on one bus; bootable volumes on different buses
// cannot be told apart.
func TestRootVolume(t *testing.T) {
	rebuildFastPolls(t)
	f := &rebuildFake{t: t, volumes: map[string]string{
		"vol-root":  cinderVolumeJSON("vol-root", "true", "/dev/vda", "img-root"),
		"vol-vdb":   cinderVolumeJSON("vol-vdb", "true", "/dev/vdb", "img-data"),
		"vol-scsi":  cinderVolumeJSON("vol-scsi", "true", "/dev/sda", "img-data"),
		"vol-plain": cinderVolumeJSON("vol-plain", "false", "/dev/vdc", ""),
	}}
	cfg := f.start().config
	bs, err := cfg.BlockStorageV3Client()
	if err != nil {
		t.Fatal(err)
	}
	device := func(name string) *string { return &name }
	for _, tc := range []struct {
		name       string
		attached   []string
		rootDevice *string
		want       string   // the root volume's ID; "" wants an error
		errHas     []string // what the error must name
	}{
		{"Nova's root device, although a bootable scsi volume sorts first", []string{"vol-scsi", "vol-root"}, device("/dev/vda"), "vol-root", nil},
		{"Nova's root device without /dev/", []string{"vol-scsi", "vol-root"}, device("vda"), "vol-root", nil},
		{"Nova's root device on no bootable volume", []string{"vol-scsi", "vol-plain"}, device("/dev/vdc"), "", []string{"/dev/vdc", "vol-scsi"}},
		{"one bootable volume", []string{"vol-plain", "vol-scsi"}, nil, "vol-scsi", nil},
		{"one bus: the first device name", []string{"vol-vdb", "vol-plain", "vol-root"}, nil, "vol-root", nil},
		{"different buses", []string{"vol-scsi", "vol-root"}, nil, "", []string{"2 bootable volumes", "different buses", "vol-scsi", "vol-root"}},
		{"no bootable volume", []string{"vol-plain"}, nil, "", []string{"no bootable volume"}},
	} {
		server := &servers.Server{ID: "srv-1", RootDeviceName: tc.rootDevice}
		for _, id := range tc.attached {
			server.AttachedVolumes = append(server.AttachedVolumes, servers.AttachedVolume{ID: id})
		}
		root, err := rootVolume(context.Background(), bs, server)
		if tc.want == "" {
			if err == nil {
				t.Errorf("%s: root %s, want an error", tc.name, root.ID)
				continue
			}
			for _, want := range tc.errHas {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("%s: error %q does not name %q", tc.name, err, want)
				}
			}
			continue
		}
		if err != nil || root.ID != tc.want {
			t.Errorf("%s: root %v, err %v; want %s", tc.name, root, err, tc.want)
		}
	}
}

// Nova's root device name picks the volume the action reimages and its image,
// although a bootable data volume on another bus sorts first.
func TestInstanceRebuildActionUsesNovaRootDevice(t *testing.T) {
	rebuildFastPolls(t)
	server := withRootDeviceName(volumeRebuildServerJSON("ACTIVE", ""), "/dev/vda")
	f := &rebuildFake{t: t, before: server, after: []string{server},
		images: map[string]string{
			"img-1": rebuildImageJSON("img-1", "cirros", ""),
			"img-x": rebuildImageJSON("img-x", "data", ""),
		},
		volumes: map[string]string{
			"vol-data": cinderVolumeJSON("vol-data", "true", "/dev/sda", "img-x"),
			"vol-root": cinderVolumeJSON("vol-root", "true", "/dev/vda", "img-1"),
		},
	}
	resp, _ := invokeRebuildAction(t, f)
	if resp.Diagnostics.HasError() {
		t.Fatalf("invoke: %v", resp.Diagnostics)
	}
	if len(f.rebuilds) != 1 {
		t.Fatalf("rebuilds %v, want one", f.rebuilds)
	}
	body, _ := json.Marshal(f.rebuilds[0])
	if string(body) != `{"rebuild":{"imageRef":"img-1"}}` || f.versions[0] != "2.93" {
		t.Errorf("rebuild body %s at %q, want img-1 at 2.93", body, f.versions[0])
	}
}

// Without Nova's root device, bootable volumes on different buses leave the
// root unknown: the action relays the error and sends no rebuild.
func TestInstanceRebuildActionBootableVolumesOnDifferentBuses(t *testing.T) {
	rebuildFastPolls(t)
	f := &rebuildFake{t: t,
		before: volumeRebuildServerJSON("ACTIVE", ""),
		images: map[string]string{
			"img-1": rebuildImageJSON("img-1", "cirros", ""),
			"img-x": rebuildImageJSON("img-x", "data", ""),
		},
		volumes: map[string]string{
			"vol-data": cinderVolumeJSON("vol-data", "true", "/dev/sda", "img-x"),
			"vol-root": cinderVolumeJSON("vol-root", "true", "/dev/vda", "img-1"),
		},
	}
	resp, _ := invokeRebuildAction(t, f)
	if !resp.Diagnostics.HasError() {
		t.Fatal("invoke succeeded; want the root volume refused as ambiguous")
	}
	if len(f.rebuilds) != 0 {
		t.Fatalf("rebuilds %v were sent", f.rebuilds)
	}
	detail := resp.Diagnostics.Errors()[0].Detail()
	for _, want := range []string{"vol-data", "vol-root", "different buses", "block_device"} {
		if !strings.Contains(detail, want) {
			t.Errorf("error %q does not mention %q", detail, want)
		}
	}
}

// reimageRootVolume runs Update on a volume-backed instance whose root
// block_device changes from image img-1 to img-2.
func reimageRootVolume(t *testing.T, f *rebuildFake) (resource.UpdateResponse, instanceModel) {
	t.Helper()
	r := f.start()
	s := rebuildSchema(t)
	state := rebuildTestModel(t, s, types.StringValue(""), types.StringNull())
	state.BlockDevice = blockDeviceList(t, map[string]attr.Value{})
	plan := rebuildTestModel(t, s, types.StringUnknown(), types.StringNull())
	plan.BlockDevice = blockDeviceList(t, map[string]attr.Value{"uuid": types.StringValue("img-2")})
	return rebuildUpdate(t, r, s, plan, state)
}

// Update confirms the reimage when a bootable volume holds the new image,
// although a bootable data volume on another bus sorts first and Nova
// reports no root device.
func TestUpdateRootVolumeCheckWithBootableDataVolume(t *testing.T) {
	rebuildFastPolls(t)
	f := &rebuildFake{t: t,
		before: volumeRebuildServerJSON("ACTIVE", ""),
		after: []string{
			volumeRebuildServerJSON("REBUILD", "rebuilding"),
			volumeRebuildServerJSON("ACTIVE", ""),
		},
		images: map[string]string{"img-2": rebuildImageJSON("img-2", "ubuntu", "")},
		volumes: map[string]string{
			"vol-data": cinderVolumeJSON("vol-data", "true", "/dev/sda", "img-x"),
			"vol-root": cinderVolumeJSON("vol-root", "true", "/dev/vda", "img-2"),
		},
	}
	resp, _ := reimageRootVolume(t, f)
	if resp.Diagnostics.HasError() {
		t.Fatalf("update: %v", resp.Diagnostics)
	}
	if len(f.rebuilds) != 1 || f.versions[0] != "2.93" {
		t.Fatalf("rebuilds %v at microversions %v; want one at 2.93", f.rebuilds, f.versions)
	}
}

// A volume read that fails after Nova finished the reimage is a warning:
// the rebuild happened, so the update succeeds and records the new image.
func TestUpdateRootVolumeCheckReadFailure(t *testing.T) {
	rebuildFastPolls(t)
	f := &rebuildFake{t: t,
		before: volumeRebuildServerJSON("ACTIVE", ""),
		after: []string{
			volumeRebuildServerJSON("REBUILD", "rebuilding"),
			volumeRebuildServerJSON("ACTIVE", ""),
		},
		images: map[string]string{"img-2": rebuildImageJSON("img-2", "ubuntu", "")},
		// vol-root answers 404.
		volumes: map[string]string{"vol-data": cinderVolumeJSON("vol-data", "false", "/dev/vdb", "")},
	}
	resp, got := reimageRootVolume(t, f)
	if resp.Diagnostics.HasError() {
		t.Fatalf("update: %v", resp.Diagnostics)
	}
	warnings := resp.Diagnostics.Warnings()
	if len(warnings) != 1 || warnings[0].Summary() != "compute: could not confirm the reimaged root volume" ||
		!strings.Contains(warnings[0].Detail(), "vol-root") {
		t.Fatalf("warnings %v, want one naming the volume that could not be read", warnings)
	}
	if len(f.rebuilds) != 1 || f.versions[0] != "2.93" {
		t.Fatalf("rebuilds %v at microversions %v; want one at 2.93", f.rebuilds, f.versions)
	}
	var devices []instanceBlockDeviceModel
	if d := got.BlockDevice.ElementsAs(context.Background(), &devices, false); d.HasError() {
		t.Fatal(d)
	}
	if len(devices) != 1 || devices[0].UUID.ValueString() != "img-2" || got.ImageID.ValueString() != "" {
		t.Errorf("state block_device %v, image_id %q; want the root on img-2 and image_id \"\"", devices, got.ImageID.ValueString())
	}
}

// When no bootable volume holds the new image, the error lists the image
// each one holds.
func TestUpdateRootVolumeMismatchListsBootableVolumes(t *testing.T) {
	rebuildFastPolls(t)
	f := &rebuildFake{t: t,
		before: volumeRebuildServerJSON("ACTIVE", ""),
		after:  []string{volumeRebuildServerJSON("ACTIVE", "")},
		images: map[string]string{"img-2": rebuildImageJSON("img-2", "ubuntu", "")},
		volumes: map[string]string{
			"vol-data": cinderVolumeJSON("vol-data", "true", "/dev/vdb", "img-x"),
			"vol-root": cinderVolumeJSON("vol-root", "true", "/dev/vda", "img-1"),
		},
	}
	resp, _ := reimageRootVolume(t, f)
	if !resp.Diagnostics.HasError() {
		t.Fatal("update succeeded although no volume holds the new image")
	}
	detail := resp.Diagnostics.Errors()[0].Detail()
	for _, want := range []string{`"img-2"`, "vol-root: img-1", "vol-data: img-x", "reimage failed"} {
		if !strings.Contains(detail, want) {
			t.Errorf("error %q does not mention %q", detail, want)
		}
	}
}

// The action reimages a volume-backed instance with the image its root
// volume was created from, at 2.93.
func TestInstanceRebuildActionReimagesVolumeBackedInstance(t *testing.T) {
	rebuildFastPolls(t)
	f := &rebuildFake{t: t,
		before: volumeRebuildServerJSON("ACTIVE", ""),
		after: []string{
			volumeRebuildServerJSON("REBUILD", "rebuilding"),
			volumeRebuildServerJSON("ACTIVE", ""),
		},
		images: map[string]string{"img-1": rebuildImageJSON("img-1", "cirros", "")},
		volumes: map[string]string{
			"vol-data": cinderVolumeJSON("vol-data", "false", "/dev/vdb", ""),
			"vol-root": cinderVolumeJSON("vol-root", "true", "/dev/vda", "img-1"),
		},
	}
	resp, _ := invokeRebuildAction(t, f)
	if resp.Diagnostics.HasError() {
		t.Fatalf("invoke: %v", resp.Diagnostics)
	}
	body, _ := json.Marshal(f.rebuilds[0])
	if string(body) != `{"rebuild":{"imageRef":"img-1"}}` || f.versions[0] != "2.93" {
		t.Errorf("rebuild body %s at %q, want img-1 at 2.93", body, f.versions[0])
	}
}
