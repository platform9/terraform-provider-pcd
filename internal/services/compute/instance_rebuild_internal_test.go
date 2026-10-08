// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package compute

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/servers"
	"github.com/gophercloud/gophercloud/v2/openstack/image/v2/images"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/platform9/terraform-provider-pcd/internal/clients"
)

// rebuildFastPolls shortens the server poll interval for one test through
// shortPolls. Tests that call it must not run in parallel: the interval is
// package state.
func rebuildFastPolls(t *testing.T) {
	t.Helper()
	shortPolls(t)
}

// rebuildServerJSON renders GET /servers/srv-1. task "" is sent as null, and
// image "" as Nova reports a volume-backed server: "image": "".
func rebuildServerJSON(status, task, image string) string {
	taskJSON := "null"
	if task != "" {
		taskJSON = fmt.Sprintf("%q", task)
	}
	imageJSON := `""`
	if image != "" {
		imageJSON = fmt.Sprintf(`{"id": %q}`, image)
	}
	return fmt.Sprintf(`{"server": {"id": "srv-1", "name": "vm-1", "status": %q, "OS-EXT-STS:task_state": %s,
		"image": %s, "metadata": {}, "addresses": {}, "security_groups": [{"name": "default"}],
		"OS-EXT-AZ:availability_zone": "nova", "fault": {"message": "rebuild failed"}}}`, status, taskJSON, imageJSON)
}

// rebuildFake is one httptest server playing Nova, Glance and Cinder. GET
// /servers/srv-1 answers before until a rebuild request arrives, then the
// entries of after in order, repeating the last one. Keying the answers to
// the rebuild request, not to a count of GETs, keeps the tests independent of
// how many times Update reads the server before it rebuilds.
type rebuildFake struct {
	t         *testing.T
	mu        sync.Mutex
	before    string
	after     []string
	afterGets int
	images    map[string]string // GET /v2/images/<id> bodies; a missing id answers 404, "500" answers 500
	lists     map[string]string // GET /v2/images?name=<name> bodies
	volumes   map[string]string // GET /volumes/<id> bodies (Cinder)
	rebuilds  []map[string]any  // every POST /servers/srv-1/action body: rebuilds and power actions
	versions  []string          // X-OpenStack-Nova-API-Version of each action request
	glance    int
}

func (f *rebuildFake) handler(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	switch {
	case r.Method == "GET" && r.URL.Path == "/servers/srv-1":
		if len(f.rebuilds) == 0 || len(f.after) == 0 {
			fmt.Fprint(w, f.before)
			return
		}
		i := f.afterGets
		if i >= len(f.after) {
			i = len(f.after) - 1
		}
		f.afterGets++
		fmt.Fprint(w, f.after[i])
	case r.Method == "POST" && r.URL.Path == "/servers/srv-1/action":
		var body map[string]any
		b, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(b, &body); err != nil {
			f.t.Errorf("action body %s: %v", b, err)
		}
		f.rebuilds = append(f.rebuilds, body)
		f.versions = append(f.versions, r.Header.Get("X-OpenStack-Nova-API-Version"))
		w.WriteHeader(http.StatusAccepted)
		fmt.Fprint(w, rebuildServerJSON("REBUILD", "rebuilding", ""))
	case r.Method == "GET" && r.URL.Path == "/v2/images":
		f.glance++
		body, ok := f.lists[r.URL.Query().Get("name")]
		if !ok {
			body = `{"images": []}`
		}
		fmt.Fprint(w, body)
	case r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/v2/images/"):
		f.glance++
		body, ok := f.images[strings.TrimPrefix(r.URL.Path, "/v2/images/")]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if body == "500" {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		fmt.Fprint(w, body)
	case r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/volumes/"):
		body, ok := f.volumes[strings.TrimPrefix(r.URL.Path, "/volumes/")]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		fmt.Fprint(w, body)
	default:
		f.t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusNotImplemented)
	}
}

// start serves the fake and returns an instance resource pointed at it
// through fakeConfig (fake_nova_internal_test.go), which gives the client a
// transport of its own.
func (f *rebuildFake) start() *instanceResource {
	srv := httptest.NewServer(http.HandlerFunc(f.handler))
	f.t.Cleanup(srv.Close)
	return &instanceResource{config: fakeConfig(srv.URL)}
}

func rebuildImageJSON(id, name, extra string) string {
	return fmt.Sprintf(`{"id": %q, "name": %q, "status": "active"%s}`, id, name, extra)
}

// rebuildTestModel is an instance as state holds it after an apply: the
// collections are typed nulls or empty values, because a zero-value
// types.List/Set/Map is not a usable null.
func rebuildTestModel(t *testing.T, s schema.Schema, imageID, imageName types.String) instanceModel {
	t.Helper()
	nested := func(block string) types.ObjectType {
		return s.Blocks[block].(schema.ListNestedBlock).NestedObject.Type().(types.ObjectType)
	}
	sgs, d := types.SetValueFrom(context.Background(), types.StringType, []string{"default"})
	if d.HasError() {
		t.Fatal(d)
	}
	return instanceModel{
		ID:                types.StringValue("srv-1"),
		Name:              types.StringValue("vm-1"),
		ImageID:           imageID,
		ImageName:         imageName,
		FlavorID:          types.StringValue("flv-1"),
		FlavorName:        types.StringNull(),
		KeyPair:           types.StringNull(),
		SecurityGroups:    sgs,
		Network:           types.ListNull(nested("network")),
		BlockDevice:       types.ListNull(nested("block_device")),
		SchedulerHints:    types.ListNull(nested("scheduler_hints")),
		Metadata:          types.MapValueMust(types.StringType, nil),
		MigrationPriority: types.StringValue(""),
		UserData:          types.StringNull(),
		AvailabilityZone:  types.StringValue("nova"),
		ConfigDrive:       types.BoolNull(),
		AccessIPv4:        types.StringValue(""),
		Status:            types.StringValue("ACTIVE"),
		Region:            types.StringValue("region-one"),
	}
}

func rebuildSchema(t *testing.T) schema.Schema {
	t.Helper()
	return schemaOf(t, &instanceResource{})
}

// rebuildUpdate runs Update with plan and state the way Terraform calls it
// (runUpdate, fake_nova_internal_test.go) and returns the updated model.
func rebuildUpdate(t *testing.T, r *instanceResource, s schema.Schema, plan, state instanceModel) (resource.UpdateResponse, instanceModel) {
	t.Helper()
	resp := runUpdate(r, newPlan(t, s, &plan), newState(t, s, &state))
	var got instanceModel
	if !resp.Diagnostics.HasError() {
		if d := resp.State.Get(context.Background(), &got); d.HasError() {
			t.Fatalf("reading the updated state: %v", d)
		}
	}
	return resp, got
}

// rebuildRead runs Read on state (runRead) and returns the refreshed model.
func rebuildRead(t *testing.T, r *instanceResource, s schema.Schema, state instanceModel) (resource.ReadResponse, instanceModel) {
	t.Helper()
	resp := runRead(r, newState(t, s, &state))
	var got instanceModel
	if !resp.Diagnostics.HasError() {
		if d := resp.State.Get(context.Background(), &got); d.HasError() {
			t.Fatalf("reading the refreshed state: %v", d)
		}
	}
	return resp, got
}

// An image change used to replace the instance. It must now plan an in-place
// update: neither attribute may carry a plan modifier, RequiresReplace above
// all.
func TestInstanceImageAttributesUpdateInPlace(t *testing.T) {
	s := rebuildSchema(t)
	for _, name := range []string{"image_id", "image_name"} {
		attr, ok := s.Attributes[name].(schema.StringAttribute)
		if !ok {
			t.Fatalf("%s is not a string attribute", name)
		}
		if len(attr.PlanModifiers) != 0 {
			t.Errorf("%s has %d plan modifiers; an image change must rebuild in place, and "+
				"UseStateForUnknown on image_id breaks a rebuild driven by image_name", name, len(attr.PlanModifiers))
		}
	}
}

// Nova returns a stopped server to SHUTOFF after a rebuild and briefly shows
// it ACTIVE with no task on the way, so the wait must target the status the
// server had before.
func TestRebuildSettleTarget(t *testing.T) {
	for status, want := range map[string]string{"ACTIVE": "ACTIVE", "SHUTOFF": "SHUTOFF", "ERROR": "ACTIVE"} {
		if got := rebuildSettleTarget(status); len(got) != 1 || got[0] != want {
			t.Errorf("rebuildSettleTarget(%s) = %v, want [%s]", status, got, want)
		}
	}
}

func TestImageBlockDeviceMapping(t *testing.T) {
	for _, tc := range []struct {
		name    string
		props   map[string]any
		want    int
		wantErr bool
	}{
		{name: "no property", props: map[string]any{}},
		{name: "empty string", props: map[string]any{"block_device_mapping": ""}},
		{name: "json string, as Glance stores it", want: 2, props: map[string]any{"block_device_mapping": `[{"source_type": "snapshot", "snapshot_id": "s-1", "boot_index": 0}, {"source_type": "snapshot", "snapshot_id": "s-2"}]`}},
		{name: "decoded list", want: 1, props: map[string]any{"block_device_mapping": []any{map[string]any{"source_type": "snapshot", "snapshot_id": "s-1"}}}},
		{name: "malformed json", wantErr: true, props: map[string]any{"block_device_mapping": `[{`}},
		{name: "wrong type", wantErr: true, props: map[string]any{"block_device_mapping": 7.0}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := imageBlockDeviceMapping(&images.Image{ID: "img-1", Properties: tc.props})
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if len(got) != tc.want {
				t.Fatalf("got %d entries, want %d: %v", len(got), tc.want, got)
			}
		})
	}
}

func TestServerImageID(t *testing.T) {
	if got := serverImageID(&servers.Server{Image: map[string]any{"id": "img-1"}}); got != "img-1" {
		t.Errorf("image-backed: got %q", got)
	}
	if got := serverImageID(&servers.Server{}); got != "" {
		t.Errorf("volume-backed: got %q, want \"\"", got)
	}
}

// Changing image_id rebuilds the same server with only imageRef in the body,
// at the client's base microversion, and records the new image.
func TestUpdateRebuildsOnImageIDChange(t *testing.T) {
	rebuildFastPolls(t)
	f := &rebuildFake{t: t,
		before: rebuildServerJSON("ACTIVE", "", "img-1"),
		after: []string{
			rebuildServerJSON("REBUILD", "rebuilding", ""),
			rebuildServerJSON("ACTIVE", "", "img-2"),
		},
		images: map[string]string{"img-2": rebuildImageJSON("img-2", "ubuntu", "")},
	}
	r := f.start()
	s := rebuildSchema(t)
	state := rebuildTestModel(t, s, types.StringValue("img-1"), types.StringNull())
	plan := rebuildTestModel(t, s, types.StringValue("img-2"), types.StringNull())
	plan.Status = types.StringUnknown()

	resp, got := rebuildUpdate(t, r, s, plan, state)
	if resp.Diagnostics.HasError() {
		t.Fatalf("update: %v", resp.Diagnostics)
	}
	if len(f.rebuilds) != 1 {
		t.Fatalf("%d rebuild requests, want 1", len(f.rebuilds))
	}
	body, _ := json.Marshal(f.rebuilds[0])
	if string(body) != `{"rebuild":{"imageRef":"img-2"}}` {
		t.Errorf("rebuild body %s; anything besides imageRef would overwrite what Nova keeps", body)
	}
	if f.versions[0] != "" {
		t.Errorf("rebuild sent microversion %q; an image-backed rebuild needs none", f.versions[0])
	}
	if got.ID.ValueString() != "srv-1" || got.ImageID.ValueString() != "img-2" {
		t.Errorf("state id=%s image_id=%s, want srv-1 and img-2", got.ID, got.ImageID)
	}
}

// A stopped server rebuilt by image_name: the name is resolved through
// Glance, and the wait does not stop at the moment Nova shows the server
// ACTIVE with no task before powering it off again.
func TestUpdateRebuildByImageNameWaitsForShutoff(t *testing.T) {
	rebuildFastPolls(t)
	f := &rebuildFake{t: t,
		before: rebuildServerJSON("SHUTOFF", "", "img-1"),
		after: []string{
			rebuildServerJSON("REBUILD", "rebuilding", ""),
			rebuildServerJSON("ACTIVE", "", "img-2"),
			rebuildServerJSON("ACTIVE", "powering-off", "img-2"),
			rebuildServerJSON("SHUTOFF", "", "img-2"),
		},
		lists:  map[string]string{"ubuntu": `{"images": [` + rebuildImageJSON("img-2", "ubuntu", "") + `]}`},
		images: map[string]string{"img-2": rebuildImageJSON("img-2", "ubuntu", "")},
	}
	r := f.start()
	s := rebuildSchema(t)
	state := rebuildTestModel(t, s, types.StringValue("img-1"), types.StringValue("cirros"))
	state.Status = types.StringValue("SHUTOFF")
	plan := rebuildTestModel(t, s, types.StringUnknown(), types.StringValue("ubuntu"))
	plan.Status = types.StringUnknown()

	resp, got := rebuildUpdate(t, r, s, plan, state)
	if resp.Diagnostics.HasError() {
		t.Fatalf("update: %v", resp.Diagnostics)
	}
	if f.afterGets < len(f.after) {
		t.Errorf("the wait stopped after %d of %d reads; it accepted the transient ACTIVE of a stopped server", f.afterGets, len(f.after))
	}
	if got.ImageID.ValueString() != "img-2" || got.ImageName.ValueString() != "ubuntu" || got.Status.ValueString() != "SHUTOFF" {
		t.Errorf("state image_id=%s image_name=%s status=%s, want img-2, ubuntu, SHUTOFF", got.ImageID, got.ImageName, got.Status)
	}
}

// A changed image_name that resolves to the image the server already runs
// does not rebuild it.
func TestUpdateSkipsRebuildWhenNameResolvesToCurrentImage(t *testing.T) {
	rebuildFastPolls(t)
	f := &rebuildFake{t: t,
		before: rebuildServerJSON("ACTIVE", "", "img-1"),
		lists:  map[string]string{"cirros-renamed": `{"images": [` + rebuildImageJSON("img-1", "cirros-renamed", "") + `]}`},
	}
	r := f.start()
	s := rebuildSchema(t)
	state := rebuildTestModel(t, s, types.StringValue("img-1"), types.StringValue("cirros"))
	plan := rebuildTestModel(t, s, types.StringUnknown(), types.StringValue("cirros-renamed"))

	resp, got := rebuildUpdate(t, r, s, plan, state)
	if resp.Diagnostics.HasError() {
		t.Fatalf("update: %v", resp.Diagnostics)
	}
	if len(f.rebuilds) != 0 {
		t.Fatalf("rebuilt the instance onto the image it already runs")
	}
	if got.ImageID.ValueString() != "img-1" {
		t.Errorf("image_id = %s, want img-1", got.ImageID)
	}
}

// An update that does not touch the image never asks Glance: a newer image
// uploaded under the same name must not rebuild the instance.
func TestUpdateWithoutImageChangeDoesNotResolveImageName(t *testing.T) {
	rebuildFastPolls(t)
	f := &rebuildFake{t: t, before: rebuildServerJSON("ACTIVE", "", "img-1")}
	r := f.start()
	s := rebuildSchema(t)
	state := rebuildTestModel(t, s, types.StringValue("img-1"), types.StringValue("cirros"))
	plan := rebuildTestModel(t, s, types.StringUnknown(), types.StringValue("cirros"))
	plan.Metadata = types.MapValueMust(types.StringType, nil)

	resp, got := rebuildUpdate(t, r, s, plan, state)
	if resp.Diagnostics.HasError() {
		t.Fatalf("update: %v", resp.Diagnostics)
	}
	if f.glance != 0 || len(f.rebuilds) != 0 {
		t.Fatalf("glance calls=%d rebuilds=%d, want 0 and 0", f.glance, len(f.rebuilds))
	}
	if got.ImageID.ValueString() != "img-1" {
		t.Errorf("image_id = %s, want img-1", got.ImageID)
	}
}

// The update is refused, before any rebuild request, when Nova would refuse
// it or the result would not boot.
func TestUpdateRebuildRefusals(t *testing.T) {
	for _, tc := range []struct {
		name   string
		server string
		images map[string]string
		want   string
	}{
		{name: "paused", server: rebuildServerJSON("PAUSED", "", "img-1"), want: "power_state"},
		{name: "task in progress", server: rebuildServerJSON("ACTIVE", "image_uploading", "img-1"), want: "task"},
		{name: "rescued", server: rebuildServerJSON("RESCUE", "", "img-1"), want: "pcd_compute_instance_rescue"},
		{name: "volume-backed", server: rebuildServerJSON("ACTIVE", "", ""), want: "boots from a volume"},
		{name: "volume snapshot target", server: rebuildServerJSON("ACTIVE", "", "img-1"), want: "volume-backed instance",
			images: map[string]string{"img-2": rebuildImageJSON("img-2", "vm-snap", `, "block_device_mapping": "[{\"source_type\": \"snapshot\", \"snapshot_id\": \"s-1\"}]"`)}},
		{name: "missing target", server: rebuildServerJSON("ACTIVE", "", "img-1"), want: "does not exist"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rebuildFastPolls(t)
			f := &rebuildFake{t: t, before: tc.server, images: tc.images}
			r := f.start()
			s := rebuildSchema(t)
			state := rebuildTestModel(t, s, types.StringValue("img-1"), types.StringNull())
			plan := rebuildTestModel(t, s, types.StringValue("img-2"), types.StringNull())
			resp, _ := rebuildUpdate(t, r, s, plan, state)
			if !resp.Diagnostics.HasError() {
				t.Fatal("update succeeded; want it refused")
			}
			if len(f.rebuilds) != 0 {
				t.Fatal("a rebuild request was sent")
			}
			if detail := resp.Diagnostics.Errors()[0].Detail(); !strings.Contains(detail, tc.want) {
				t.Errorf("error %q does not mention %q", detail, tc.want)
			}
		})
	}
}

// Nova records the new image before the compute host rebuilds the instance,
// so a rebuild that fails there leaves the instance in ERROR already
// reporting the new image. The update fails and says how to recover, since
// nothing retries on its own: the next refresh records the new image, and
// the plan then shows no image change.
func TestUpdateRebuildEndingInError(t *testing.T) {
	rebuildFastPolls(t)
	f := &rebuildFake{t: t,
		before: rebuildServerJSON("ACTIVE", "", "img-1"),
		after: []string{
			rebuildServerJSON("REBUILD", "rebuilding", "img-2"),
			rebuildServerJSON("ERROR", "", "img-2"),
		},
		images: map[string]string{"img-2": rebuildImageJSON("img-2", "ubuntu", "")},
	}
	r := f.start()
	s := rebuildSchema(t)
	state := rebuildTestModel(t, s, types.StringValue("img-1"), types.StringNull())
	resp, _ := rebuildUpdate(t, r, s, rebuildTestModel(t, s, types.StringValue("img-2"), types.StringNull()), state)
	if !resp.Diagnostics.HasError() {
		t.Fatal("update succeeded although the rebuild ended in ERROR")
	}
	if len(f.rebuilds) != 1 || f.rebuilds[0]["rebuild"] == nil {
		t.Fatalf("server actions %v, want the one rebuild", f.rebuilds)
	}
	detail := resp.Diagnostics.Errors()[0].Detail()
	for _, want := range []string{`type = "HARD"`, "terraform apply -replace="} {
		if !strings.Contains(detail, want) {
			t.Errorf("error %q does not mention %q", detail, want)
		}
	}

	_, got := rebuildRead(t, r, s, state)
	if got.ImageID.ValueString() != "img-2" || got.Status.ValueString() != "ERROR" {
		t.Errorf("refresh after the failed rebuild: image_id=%s status=%s, want img-2 and ERROR", got.ImageID, got.Status)
	}
}

// A rebuild Nova rolls back leaves the server ACTIVE on its old image; the
// update must not record the new one.
func TestUpdateRebuildThatKeptTheOldImage(t *testing.T) {
	rebuildFastPolls(t)
	f := &rebuildFake{t: t,
		before: rebuildServerJSON("ACTIVE", "", "img-1"),
		after: []string{
			rebuildServerJSON("REBUILD", "rebuilding", ""),
			rebuildServerJSON("ACTIVE", "", "img-1"),
		},
		images: map[string]string{"img-2": rebuildImageJSON("img-2", "ubuntu", "")},
	}
	r := f.start()
	s := rebuildSchema(t)
	resp, _ := rebuildUpdate(t, r, s,
		rebuildTestModel(t, s, types.StringValue("img-2"), types.StringNull()),
		rebuildTestModel(t, s, types.StringValue("img-1"), types.StringNull()))
	if !resp.Diagnostics.HasError() {
		t.Fatal("update succeeded although the server still runs img-1")
	}
}

// A rebuild is how Nova recovers an instance in ERROR, and the power_state
// steps refuse ERROR, so the instance is rebuilt before them; the power step
// that follows starts from the status the rebuild left, not the ERROR read
// before it.
func TestUpdateRebuildsOutOfErrorWithPowerState(t *testing.T) {
	for _, tc := range []struct {
		powerState string
		after      []string
		actions    string
		status     string
	}{
		{powerState: "active", actions: "rebuild", status: "ACTIVE", after: []string{
			rebuildServerJSON("ERROR", "rebuilding", "img-1"),
			rebuildServerJSON("ACTIVE", "", "img-2"),
		}},
		{powerState: "shutoff", actions: "rebuild,os-stop", status: "SHUTOFF", after: []string{
			rebuildServerJSON("ERROR", "rebuilding", "img-1"),
			rebuildServerJSON("ACTIVE", "", "img-2"),
			rebuildServerJSON("ACTIVE", "powering-off", "img-2"),
			rebuildServerJSON("SHUTOFF", "", "img-2"),
		}},
	} {
		t.Run(tc.powerState, func(t *testing.T) {
			rebuildFastPolls(t)
			f := &rebuildFake{t: t,
				before: rebuildServerJSON("ERROR", "", "img-1"),
				after:  tc.after,
				images: map[string]string{"img-2": rebuildImageJSON("img-2", "ubuntu", "")},
			}
			r := f.start()
			s := rebuildSchema(t)
			state := rebuildTestModel(t, s, types.StringValue("img-1"), types.StringNull())
			state.Status = types.StringValue("ERROR")
			state.PowerState = types.StringValue(tc.powerState)
			plan := rebuildTestModel(t, s, types.StringValue("img-2"), types.StringNull())
			plan.Status = types.StringUnknown()
			plan.PowerState = types.StringValue(tc.powerState)

			resp, got := rebuildUpdate(t, r, s, plan, state)
			if resp.Diagnostics.HasError() {
				t.Fatalf("update: %v", resp.Diagnostics)
			}
			var actions []string
			for _, body := range f.rebuilds {
				for name := range body {
					actions = append(actions, name)
				}
			}
			if strings.Join(actions, ",") != tc.actions {
				t.Errorf("server actions %v, want %s", actions, tc.actions)
			}
			if got.ImageID.ValueString() != "img-2" || got.Status.ValueString() != tc.status {
				t.Errorf("state image_id=%s status=%s, want img-2 and %s", got.ImageID, got.Status, tc.status)
			}
		})
	}
}

// Read follows a rebuild done outside Terraform: image_id takes the new image
// and, when the configuration names its image, image_name its Glance name.
func TestReadRefreshesImageAfterOutsideRebuild(t *testing.T) {
	f := &rebuildFake{t: t,
		before: rebuildServerJSON("ACTIVE", "", "img-2"),
		images: map[string]string{"img-2": rebuildImageJSON("img-2", "ubuntu", "")},
	}
	r := f.start()
	s := rebuildSchema(t)
	resp, got := rebuildRead(t, r, s, rebuildTestModel(t, s, types.StringValue("img-1"), types.StringValue("cirros")))
	if resp.Diagnostics.HasError() {
		t.Fatalf("read: %v", resp.Diagnostics)
	}
	if got.ImageID.ValueString() != "img-2" || got.ImageName.ValueString() != "ubuntu" {
		t.Errorf("image_id=%s image_name=%s, want img-2 and ubuntu", got.ImageID, got.ImageName)
	}
}

// Read asks Glance nothing while the server runs the image in state, and a
// configuration that does not name its image keeps image_name null.
func TestReadLeavesImageNameAlone(t *testing.T) {
	f := &rebuildFake{t: t, before: rebuildServerJSON("ACTIVE", "", "img-1")}
	r := f.start()
	s := rebuildSchema(t)
	_, got := rebuildRead(t, r, s, rebuildTestModel(t, s, types.StringValue("img-1"), types.StringValue("cirros")))
	if f.glance != 0 || got.ImageName.ValueString() != "cirros" {
		t.Errorf("glance calls=%d image_name=%s, want 0 and cirros", f.glance, got.ImageName)
	}

	f2 := &rebuildFake{t: t, before: rebuildServerJSON("ACTIVE", "", "img-2")}
	r2 := f2.start()
	_, got = rebuildRead(t, r2, s, rebuildTestModel(t, s, types.StringValue("img-1"), types.StringNull()))
	if f2.glance != 0 || !got.ImageName.IsNull() || got.ImageID.ValueString() != "img-2" {
		t.Errorf("glance calls=%d image_name=%s image_id=%s, want 0, null and img-2", f2.glance, got.ImageName, got.ImageID)
	}
}

// A Glance error while following an outside rebuild is a warning, not a
// failed refresh, and the prior image stays in state so the next refresh
// asks again instead of losing the drift.
func TestReadKeepsPriorImageWhenGlanceFails(t *testing.T) {
	f := &rebuildFake{t: t,
		before: rebuildServerJSON("ACTIVE", "", "img-2"),
		images: map[string]string{"img-2": "500"},
	}
	r := f.start()
	s := rebuildSchema(t)
	resp, got := rebuildRead(t, r, s, rebuildTestModel(t, s, types.StringValue("img-1"), types.StringValue("cirros")))
	if resp.Diagnostics.HasError() || len(resp.Diagnostics.Warnings()) != 1 {
		t.Fatalf("diagnostics %v, want one warning and no error", resp.Diagnostics)
	}
	if got.ImageID.ValueString() != "img-1" || got.ImageName.ValueString() != "cirros" {
		t.Errorf("image_id=%s image_name=%s, want img-1 and cirros until Glance answers", got.ImageID, got.ImageName)
	}
}

// An import records the image the server runs, and a server that boots from
// a volume reads back image_id "".
func TestReadRecordsImageOnImport(t *testing.T) {
	s := rebuildSchema(t)
	imported := rebuildTestModel(t, s, types.StringNull(), types.StringNull())

	f := &rebuildFake{t: t, before: rebuildServerJSON("ACTIVE", "", "img-1")}
	_, got := rebuildRead(t, f.start(), s, imported)
	if got.ImageID.ValueString() != "img-1" {
		t.Errorf("image-backed import: image_id = %s, want img-1", got.ImageID)
	}

	f2 := &rebuildFake{t: t, before: rebuildServerJSON("ACTIVE", "", "")}
	_, got = rebuildRead(t, f2.start(), s, imported)
	if got.ImageID.IsNull() || got.ImageID.ValueString() != "" {
		t.Errorf("volume-backed import: image_id = %s, want \"\"", got.ImageID)
	}
}

// Glance can answer 200 without an image in the body. Reading a rebuild
// target then fails the update instead of crashing the provider, and
// following an outside rebuild warns and keeps the prior image.
func TestRebuildImageReadsWithoutObject(t *testing.T) {
	rebuildFastPolls(t)
	s := rebuildSchema(t)
	f := &rebuildFake{t: t,
		before: rebuildServerJSON("ACTIVE", "", "img-1"),
		images: map[string]string{"img-2": "null"},
	}
	resp, _ := rebuildUpdate(t, f.start(), s,
		rebuildTestModel(t, s, types.StringValue("img-2"), types.StringNull()),
		rebuildTestModel(t, s, types.StringValue("img-1"), types.StringNull()))
	if !resp.Diagnostics.HasError() {
		t.Fatal("update succeeded although Glance returned no image")
	}
	if len(f.rebuilds) != 0 {
		t.Fatal("a rebuild request was sent")
	}
	if detail := resp.Diagnostics.Errors()[0].Detail(); !strings.Contains(detail, clients.ErrNoObject.Error()) {
		t.Errorf("error %q does not report the missing object", detail)
	}

	f2 := &rebuildFake{t: t,
		before: rebuildServerJSON("ACTIVE", "", "img-2"),
		images: map[string]string{"img-2": "null"},
	}
	readResp, got := rebuildRead(t, f2.start(), s, rebuildTestModel(t, s, types.StringValue("img-1"), types.StringValue("cirros")))
	if readResp.Diagnostics.HasError() || len(readResp.Diagnostics.Warnings()) != 1 {
		t.Fatalf("diagnostics %v, want one warning and no error", readResp.Diagnostics)
	}
	if got.ImageID.ValueString() != "img-1" || got.ImageName.ValueString() != "cirros" {
		t.Errorf("image_id=%s image_name=%s, want img-1 and cirros until Glance answers", got.ImageID, got.ImageName)
	}
}
