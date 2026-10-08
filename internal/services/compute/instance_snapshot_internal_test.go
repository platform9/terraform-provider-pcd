// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package compute

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

// snapshotReply is one scripted answer of snapshotFake.
type snapshotReply struct {
	status  int
	body    string
	headers map[string]string
}

// snapshotFake plays Nova, Glance and Cinder on the package's shared fake
// server (newFakeNova, fake_nova_internal_test.go). Each "METHOD /path"
// answers from its script in order and repeats the last entry; the shared
// fake fails the test on any other request and records every call.
type snapshotFake struct {
	t      *testing.T
	routes map[string][]snapshotReply
	nova   *fakeNova
	served map[string]int
	bodies map[string]string // last request body per route
	hdrs   map[string]http.Header
}

// start serves the scripts and returns a snapshot resource pointed at them.
// The shared fake serves one request at a time, so the handlers need no lock
// of their own.
func (f *snapshotFake) start() *instanceSnapshotResource {
	f.served, f.bodies, f.hdrs = map[string]int{}, map[string]string{}, map[string]http.Header{}
	routes := novaRoutes{}
	for key, script := range f.routes {
		routes[key] = func(w http.ResponseWriter, r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			f.bodies[key], f.hdrs[key] = string(b), r.Header.Clone()
			answer := script[min(f.served[key], len(script)-1)]
			f.served[key]++
			w.Header().Set("Content-Type", "application/json")
			for k, v := range answer.headers {
				w.Header().Set(k, v)
			}
			if answer.status == 0 {
				answer.status = http.StatusOK
			}
			w.WriteHeader(answer.status)
			fmt.Fprint(w, answer.body)
		}
	}
	f.nova = newFakeNova(f.t, routes)
	shortPolls(f.t)
	old := snapshotPollInterval
	snapshotPollInterval = time.Millisecond
	f.t.Cleanup(func() { snapshotPollInterval = old })
	return &instanceSnapshotResource{config: f.nova.config}
}

// count is the number of requests route key answered.
func (f *snapshotFake) count(key string) int {
	return f.served[key]
}

// createImageReply is Nova's createImage answer at 2.45: the ID is in the
// body, and gophercloud reads it only with this exact Content-Type and the
// microversion header.
func createImageReply(contentType string) snapshotReply {
	return snapshotReply{status: http.StatusAccepted, body: `{"image_id": "img-9"}`,
		headers: map[string]string{"Content-Type": contentType, "X-OpenStack-Nova-API-Version": "2.45"}}
}

func snapshotImageJSON(status string, size int, extra string) snapshotReply {
	return snapshotReply{body: fmt.Sprintf(`{"id": "img-9", "name": "snap-1", "status": %q, "size": %d,
		"created_at": "2026-10-06T12:00:00Z", "instance_uuid": "srv-1"%s}`, status, size, extra)}
}

const snapshotVolumeBDM = `, "block_device_mapping": "[{\"source_type\": \"snapshot\", \"snapshot_id\": \"s-1\", \"boot_index\": 0}]"`

func cinderSnapshotJSON(id, status, instance string) snapshotReply {
	return snapshotReply{body: fmt.Sprintf(`{"snapshot": {"id": %q, "status": %q, "metadata": {"instance_uuid": %q}}}`, id, status, instance)}
}

// settledServer is GET /servers/srv-1 for an ACTIVE instance, with task ""
// sent as null.
func settledServer(task string) snapshotReply {
	taskJSON := "null"
	if task != "" {
		taskJSON = fmt.Sprintf("%q", task)
	}
	return snapshotReply{body: fmt.Sprintf(`{"server": {"id": "srv-1", "status": "ACTIVE", "OS-EXT-STS:task_state": %s}}`, taskJSON)}
}

func snapshotSchema(t *testing.T) schema.Schema {
	t.Helper()
	return schemaOf(t, &instanceSnapshotResource{})
}

func snapshotPlanned() instanceSnapshotModel {
	return instanceSnapshotModel{
		ID:                types.StringUnknown(),
		InstanceID:        types.StringValue("srv-1"),
		Name:              types.StringValue("snap-1"),
		Status:            types.StringUnknown(),
		SizeBytes:         types.Int64Unknown(),
		VolumeBacked:      types.BoolUnknown(),
		VolumeSnapshotIDs: types.ListUnknown(types.StringType),
		CreatedAt:         types.StringUnknown(),
		Region:            types.StringUnknown(),
	}
}

func snapshotStored(ids ...string) instanceSnapshotModel {
	l, _ := types.ListValueFrom(context.Background(), types.StringType, append([]string{}, ids...))
	return instanceSnapshotModel{
		ID:                types.StringValue("img-9"),
		InstanceID:        types.StringValue("srv-1"),
		Name:              types.StringValue("snap-1"),
		Status:            types.StringValue("active"),
		SizeBytes:         types.Int64Value(0),
		VolumeBacked:      types.BoolValue(len(ids) > 0),
		VolumeSnapshotIDs: l,
		CreatedAt:         types.StringValue("2026-10-06T12:00:00Z"),
		Region:            types.StringValue("region-one"),
	}
}

// snapshotCreate runs Create on the planned snapshot the way Terraform does
// (runCreate, fake_nova_internal_test.go): the state starts null.
func snapshotCreate(t *testing.T, r *instanceSnapshotResource) resource.CreateResponse {
	t.Helper()
	planned := snapshotPlanned()
	return runCreate(r, newPlan(t, snapshotSchema(t), &planned))
}

func snapshotState(t *testing.T, m instanceSnapshotModel) tfsdk.State {
	t.Helper()
	return newState(t, snapshotSchema(t), &m)
}

func snapshotModelOf(t *testing.T, st tfsdk.State) instanceSnapshotModel {
	t.Helper()
	var m instanceSnapshotModel
	if d := st.Get(context.Background(), &m); d.HasError() {
		t.Fatalf("reading state: %v", d)
	}
	return m
}

// An instance booted from an image: createImage at 2.45 with only the name,
// a wait through queued and saving, and a wait for the instance's upload task
// to clear.
func TestSnapshotCreateFromImageBackedInstance(t *testing.T) {
	f := &snapshotFake{t: t, routes: map[string][]snapshotReply{
		"POST /servers/srv-1/action": {createImageReply("application/json")},
		"GET /v2/images/img-9":       {snapshotImageJSON("queued", 0, ""), snapshotImageJSON("saving", 0, ""), snapshotImageJSON("active", 1234, "")},
		"GET /servers/srv-1":         {settledServer("image_uploading"), settledServer("")},
	}}
	resp := snapshotCreate(t, f.start())
	if resp.Diagnostics.HasError() {
		t.Fatalf("create: %v", resp.Diagnostics)
	}
	if body := f.bodies["POST /servers/srv-1/action"]; body != `{"createImage":{"name":"snap-1"}}` {
		t.Errorf("createImage body %s", body)
	}
	if v := f.hdrs["POST /servers/srv-1/action"].Get("X-OpenStack-Nova-API-Version"); v != "2.45" {
		t.Errorf("createImage microversion %q, want 2.45 (below it the image ID is not in the body)", v)
	}
	if f.count("GET /servers/srv-1") < 2 {
		t.Error("create did not wait for the instance's upload task to clear")
	}
	if !resp.State.Raw.IsFullyKnown() {
		t.Fatalf("state holds unknown values: %v", resp.State.Raw)
	}
	got := snapshotModelOf(t, resp.State)
	if got.ID.ValueString() != "img-9" || got.Status.ValueString() != "active" || got.SizeBytes.ValueInt64() != 1234 ||
		got.VolumeBacked.ValueBool() || len(got.VolumeSnapshotIDs.Elements()) != 0 || got.CreatedAt.ValueString() != "2026-10-06T12:00:00Z" {
		t.Errorf("state %+v", got)
	}
}

// A volume-backed instance: Glance reports the zero-byte image active at
// once, so create also waits for the Cinder snapshot it refers to.
func TestSnapshotCreateFromVolumeBackedInstance(t *testing.T) {
	f := &snapshotFake{t: t, routes: map[string][]snapshotReply{
		"POST /servers/srv-1/action": {createImageReply("application/json")},
		"GET /v2/images/img-9":       {snapshotImageJSON("active", 0, snapshotVolumeBDM)},
		"GET /snapshots/s-1":         {cinderSnapshotJSON("s-1", "creating", "srv-1"), cinderSnapshotJSON("s-1", "available", "srv-1")},
		"GET /servers/srv-1":         {settledServer("")},
	}}
	resp := snapshotCreate(t, f.start())
	if resp.Diagnostics.HasError() {
		t.Fatalf("create: %v", resp.Diagnostics)
	}
	if f.count("GET /snapshots/s-1") < 2 {
		t.Error("create returned before the volume snapshot was available")
	}
	got := snapshotModelOf(t, resp.State)
	var ids []string
	got.VolumeSnapshotIDs.ElementsAs(context.Background(), &ids, false)
	if !got.VolumeBacked.ValueBool() || len(ids) != 1 || ids[0] != "s-1" {
		t.Errorf("volume_backed=%s volume_snapshot_ids=%v, want true and [s-1]", got.VolumeBacked, ids)
	}
}

// gophercloud returns "" with no error when the createImage response is not
// exactly application/json; create must fail rather than store an empty ID.
func TestSnapshotCreateWithoutImageID(t *testing.T) {
	f := &snapshotFake{t: t, routes: map[string][]snapshotReply{
		"POST /servers/srv-1/action": {createImageReply("application/json; charset=utf-8")},
	}}
	resp := snapshotCreate(t, f.start())
	if !resp.Diagnostics.HasError() {
		t.Fatal("create succeeded without an image ID")
	}
	if !resp.State.Raw.IsNull() {
		t.Fatal("create stored state without an image ID")
	}
}

// Nova deletes the image of a failed snapshot: create fails and keeps
// nothing. An image Glance marks killed stays in state, tainted, so the next
// apply deletes it.
func TestSnapshotCreateFailures(t *testing.T) {
	gone := &snapshotFake{t: t, routes: map[string][]snapshotReply{
		"POST /servers/srv-1/action": {createImageReply("application/json")},
		"GET /v2/images/img-9":       {snapshotImageJSON("queued", 0, ""), {status: http.StatusNotFound, body: `{}`}},
	}}
	resp := snapshotCreate(t, gone.start())
	if !resp.Diagnostics.HasError() || !resp.State.Raw.IsNull() {
		t.Errorf("image deleted by Nova: error=%v state null=%v, want an error and no state", resp.Diagnostics.HasError(), resp.State.Raw.IsNull())
	}

	killed := &snapshotFake{t: t, routes: map[string][]snapshotReply{
		"POST /servers/srv-1/action": {createImageReply("application/json")},
		"GET /v2/images/img-9":       {snapshotImageJSON("killed", 0, "")},
	}}
	resp = snapshotCreate(t, killed.start())
	if !resp.Diagnostics.HasError() || resp.State.Raw.IsNull() {
		t.Fatalf("killed image: error=%v state null=%v, want an error with the image in state", resp.Diagnostics.HasError(), resp.State.Raw.IsNull())
	}
	if id := snapshotModelOf(t, resp.State).ID.ValueString(); id != "img-9" {
		t.Errorf("state id %q, want img-9", id)
	}
}

// A volume snapshot that fails leaves the image in state, tainted, together
// with the Cinder snapshots it refers to: destroy deletes only what state
// names.
func TestSnapshotCreateRecordsVolumeSnapshotsBeforeWaiting(t *testing.T) {
	f := &snapshotFake{t: t, routes: map[string][]snapshotReply{
		"POST /servers/srv-1/action": {createImageReply("application/json")},
		"GET /v2/images/img-9":       {snapshotImageJSON("active", 0, snapshotVolumeBDM)},
		"GET /snapshots/s-1":         {cinderSnapshotJSON("s-1", "error", "srv-1")},
	}}
	resp := snapshotCreate(t, f.start())
	if !resp.Diagnostics.HasError() || resp.State.Raw.IsNull() {
		t.Fatalf("error=%v state null=%v, want an error with the image in state", resp.Diagnostics.HasError(), resp.State.Raw.IsNull())
	}
	var ids []string
	snapshotModelOf(t, resp.State).VolumeSnapshotIDs.ElementsAs(context.Background(), &ids, false)
	if len(ids) != 1 || ids[0] != "s-1" {
		t.Errorf("volume_snapshot_ids %v, want [s-1]", ids)
	}
}

// An import knows only the image ID. The instance comes from the image's
// instance_uuid for an image-backed snapshot, and from the Cinder snapshot
// for a volume-backed one, whose image property may be inherited and stale.
func TestSnapshotReadAfterImport(t *testing.T) {
	imported := instanceSnapshotModel{ID: types.StringValue("img-9"), InstanceID: types.StringNull(), Name: types.StringNull(),
		Status: types.StringNull(), SizeBytes: types.Int64Null(), VolumeBacked: types.BoolNull(),
		VolumeSnapshotIDs: types.ListNull(types.StringType), CreatedAt: types.StringNull(), Region: types.StringNull()}

	for _, tc := range []struct {
		name   string
		routes map[string][]snapshotReply
		want   string
	}{
		{name: "image-backed", want: "srv-1", routes: map[string][]snapshotReply{
			"GET /v2/images/img-9": {snapshotImageJSON("active", 1234, "")},
		}},
		{name: "volume-backed", want: "srv-1", routes: map[string][]snapshotReply{
			"GET /v2/images/img-9": {snapshotReply{body: `{"id": "img-9", "name": "snap-1", "status": "active", "size": 0,
				"created_at": "2026-10-06T12:00:00Z", "instance_uuid": "stale-base"` + snapshotVolumeBDM + `}`}},
			"GET /snapshots/s-1": {cinderSnapshotJSON("s-1", "available", "srv-1")},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &snapshotFake{t: t, routes: tc.routes}
			r := f.start()
			resp := runRead(r, snapshotState(t, imported))
			if resp.Diagnostics.HasError() {
				t.Fatalf("read: %v", resp.Diagnostics)
			}
			if got := snapshotModelOf(t, resp.State); got.InstanceID.ValueString() != tc.want || got.Name.ValueString() != "snap-1" {
				t.Errorf("instance_id=%s name=%s, want %s and snap-1", got.InstanceID, got.Name, tc.want)
			}
		})
	}

	t.Run("not a snapshot", func(t *testing.T) {
		f := &snapshotFake{t: t, routes: map[string][]snapshotReply{
			"GET /v2/images/img-9": {snapshotReply{body: `{"id": "img-9", "name": "ubuntu", "status": "active", "size": 10, "created_at": "2026-10-06T12:00:00Z"}`}},
		}}
		r := f.start()
		resp := runRead(r, snapshotState(t, imported))
		if !resp.Diagnostics.HasError() {
			t.Fatal("imported an uploaded image as an instance snapshot")
		}
	})
}

// Glance answering 200 with a JSON null body names no image: Read reports an
// error and keeps the row, rather than reading through a nil image.
func TestSnapshotReadNullImage(t *testing.T) {
	f := &snapshotFake{t: t, routes: map[string][]snapshotReply{
		"GET /v2/images/img-9": {{body: `null`}},
	}}
	r := f.start()
	resp := runRead(r, snapshotState(t, snapshotStored()))
	if !resp.Diagnostics.HasError() {
		t.Fatalf("diagnostics %v, want an error", resp.Diagnostics)
	}
	if resp.State.Raw.IsNull() {
		t.Error("read removed the row; a failed read keeps it")
	}
}

// An image deleted outside Terraform leaves state, and the warning names the
// Cinder snapshots that still exist, since nothing deletes them any more.
func TestSnapshotReadGoneNamesLeftovers(t *testing.T) {
	f := &snapshotFake{t: t, routes: map[string][]snapshotReply{
		"GET /v2/images/img-9": {{status: http.StatusNotFound, body: `{}`}},
		"GET /snapshots/s-1":   {cinderSnapshotJSON("s-1", "available", "srv-1")},
		"GET /snapshots/s-2":   {{status: http.StatusNotFound, body: `{}`}},
	}}
	r := f.start()
	resp := runRead(r, snapshotState(t, snapshotStored("s-1", "s-2")))
	if !resp.State.Raw.IsNull() {
		t.Fatal("a deleted image stayed in state")
	}
	if len(resp.Diagnostics.Warnings()) != 1 {
		t.Fatalf("diagnostics %v, want one warning", resp.Diagnostics)
	}
	detail := resp.Diagnostics.Warnings()[0].Detail()
	if !strings.Contains(detail, "s-1") || strings.Contains(detail, "s-2") {
		t.Errorf("warning %q should name s-1 only", detail)
	}
}

// A name change renames the image in place with one JSON-patch operation.
func TestSnapshotUpdateRenames(t *testing.T) {
	f := &snapshotFake{t: t, routes: map[string][]snapshotReply{
		"PATCH /v2/images/img-9": {snapshotImageJSON("active", 1234, "")},
		"GET /v2/images/img-9":   {{body: `{"id": "img-9", "name": "golden", "status": "active", "size": 1234, "created_at": "2026-10-06T12:00:00Z", "instance_uuid": "srv-1"}`}},
	}}
	r := f.start()
	planned := snapshotStored()
	planned.Name = types.StringValue("golden")
	resp := runUpdate(r, newPlan(t, snapshotSchema(t), &planned), snapshotState(t, snapshotStored()))
	if resp.Diagnostics.HasError() {
		t.Fatalf("update: %v", resp.Diagnostics)
	}
	if body := f.bodies["PATCH /v2/images/img-9"]; body != `[{"op":"replace","path":"/name","value":"golden"}]` {
		t.Errorf("patch body %s", body)
	}
	if got := snapshotModelOf(t, resp.State); got.Name.ValueString() != "golden" || got.ID.ValueString() != "img-9" {
		t.Errorf("state name=%s id=%s", got.Name, got.ID)
	}
}

// Destroy deletes the image first, then each Cinder snapshot, and waits
// until they are gone so a root volume deleted next has no snapshots left.
func TestSnapshotDeleteVolumeBacked(t *testing.T) {
	f := &snapshotFake{t: t, routes: map[string][]snapshotReply{
		"DELETE /v2/images/img-9": {{status: http.StatusNoContent}},
		"DELETE /snapshots/s-1":   {{status: http.StatusAccepted}},
		"GET /snapshots/s-1":      {cinderSnapshotJSON("s-1", "deleting", "srv-1"), {status: http.StatusNotFound, body: `{}`}},
	}}
	r := f.start()
	resp := runDelete(r, snapshotState(t, snapshotStored("s-1")))
	if resp.Diagnostics.HasError() {
		t.Fatalf("delete: %v", resp.Diagnostics)
	}
	want := []string{"DELETE /v2/images/img-9", "DELETE /snapshots/s-1", "GET /snapshots/s-1", "GET /snapshots/s-1"}
	if calls := f.nova.received(); strings.Join(calls, ",") != strings.Join(want, ",") {
		t.Errorf("calls %v, want %v", calls, want)
	}
}

// A volume snapshot Cinder refuses to delete fails the destroy with its ID,
// and an image already gone is not an error.
func TestSnapshotDeleteLeftoversAndGoneImage(t *testing.T) {
	f := &snapshotFake{t: t, routes: map[string][]snapshotReply{
		"DELETE /v2/images/img-9": {{status: http.StatusNotFound, body: `{}`}},
		"DELETE /snapshots/s-1":   {{status: http.StatusBadRequest, body: `{"badRequest": {"message": "snapshot has dependent volumes"}}`}},
	}}
	r := f.start()
	resp := runDelete(r, snapshotState(t, snapshotStored("s-1")))
	if !resp.Diagnostics.HasError() || !strings.Contains(resp.Diagnostics.Errors()[0].Detail(), "s-1") {
		t.Fatalf("diagnostics %v, want an error naming s-1", resp.Diagnostics)
	}

	f2 := &snapshotFake{t: t, routes: map[string][]snapshotReply{
		"DELETE /v2/images/img-9": {{status: http.StatusNotFound, body: `{}`}},
	}}
	resp2 := runDelete(f2.start(), snapshotState(t, snapshotStored()))
	if resp2.Diagnostics.HasError() {
		t.Fatalf("delete of a gone image: %v", resp2.Diagnostics)
	}
}

// A state row with no ID names no image. Read drops it with a warning and
// Delete forgets it, and neither sends a request: the fake has no routes, so
// any request would fail the test.
func TestSnapshotRowWithoutID(t *testing.T) {
	f := &snapshotFake{t: t, routes: map[string][]snapshotReply{}}
	r := f.start()
	row := snapshotStored("s-1")
	row.ID = types.StringValue("")
	read := runRead(r, snapshotState(t, row))
	if read.Diagnostics.HasError() || len(read.Diagnostics.Warnings()) != 1 || !read.State.Raw.IsNull() {
		t.Errorf("read: diagnostics %v, state removed %v; want one warning and the row removed", read.Diagnostics, read.State.Raw.IsNull())
	}
	del := runDelete(r, snapshotState(t, row))
	if del.Diagnostics.HasError() || len(del.Diagnostics.Warnings()) != 1 {
		t.Errorf("delete: diagnostics %v, want one warning", del.Diagnostics)
	}
	if calls := f.nova.received(); len(calls) != 0 {
		t.Errorf("requests %v, want none", calls)
	}
}

// A Glance that takes the request and never answers: the wait's timeout ends
// the request itself, not only the sleep between polls.
func TestSnapshotWaitTimesOutOnSilentServer(t *testing.T) {
	release := make(chan struct{})
	nova := newFakeNova(t, novaRoutes{
		"GET /v2/images/img-9": func(_ http.ResponseWriter, r *http.Request) {
			select {
			case <-r.Context().Done():
			case <-release:
			}
		},
	})
	// Registered after newFakeNova, so it runs first: the server can close
	// even when the wait never ends.
	t.Cleanup(func() { close(release) })
	client, err := nova.config.ImageV2Client()
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := waitForSnapshotImage(context.Background(), client, "img-9", 20*time.Millisecond)
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "timed out waiting for image img-9") {
			t.Errorf("error %v, want the timed-out error", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the wait did not end at its timeout while Glance sent no answer")
	}
}

func TestVolumeSnapshotIDs(t *testing.T) {
	got := volumeSnapshotIDs([]map[string]any{
		{"source_type": "snapshot", "snapshot_id": "s-1"},
		{"source_type": "image", "image_id": "img-1"},
		{"source_type": "snapshot", "snapshot_id": ""},
	})
	if len(got) != 1 || got[0] != "s-1" {
		t.Errorf("got %v, want [s-1]", got)
	}
	if got := volumeSnapshotIDs(nil); got == nil || len(got) != 0 {
		t.Errorf("no entries: got %#v, want an empty, non-nil slice", got)
	}
}
