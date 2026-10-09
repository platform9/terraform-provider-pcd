// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package compute

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

// rescueFake plays Nova. GET /servers/srv-1 answers from servers in order and
// repeats the last entry; POST /servers/srv-1/action answers actionStatus.
type rescueFake struct {
	t            *testing.T
	mu           sync.Mutex
	servers      []string
	gets         int
	actionStatus int
	actions      []string // request bodies
	versions     []string // X-OpenStack-Nova-API-Version of each action
}

func (f *rescueFake) start() *instanceRescueResource {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch r.Method + " " + r.URL.Path {
		case "GET /servers/srv-1":
			i := f.gets
			if i >= len(f.servers) {
				i = len(f.servers) - 1
			}
			f.gets++
			if f.servers[i] == "404" {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			fmt.Fprint(w, f.servers[i])
		case "POST /servers/srv-1/action":
			b, _ := io.ReadAll(r.Body)
			f.actions = append(f.actions, string(b))
			f.versions = append(f.versions, r.Header.Get("X-OpenStack-Nova-API-Version"))
			w.WriteHeader(f.actionStatus)
			fmt.Fprint(w, `{"adminPass": "generated-secret"}`)
		default:
			f.t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotImplemented)
		}
	}))
	f.t.Cleanup(srv.Close)
	shortPolls(f.t)
	return &instanceRescueResource{config: fakeConfig(srv.URL)}
}

// rescueServerJSON renders GET /servers/srv-1, with task "" sent as null.
func rescueServerJSON(status, task string) string {
	taskJSON := "null"
	if task != "" {
		taskJSON = fmt.Sprintf("%q", task)
	}
	return fmt.Sprintf(`{"server": {"id": "srv-1", "status": %q, "OS-EXT-STS:task_state": %s, "fault": {"message": "Driver Error"}}}`, status, taskJSON)
}

func rescueModel(image types.String) instanceRescueModel {
	return instanceRescueModel{ID: types.StringValue("srv-1"), InstanceID: types.StringValue("srv-1"), RescueImageID: image, Region: types.StringValue("region-one")}
}

func rescueTFState(t *testing.T, m instanceRescueModel) tfsdk.State {
	t.Helper()
	return newState(t, schemaOf(t, &instanceRescueResource{}), &m)
}

// rescueCreate runs Create for instance srv-1 the way Terraform does
// (runCreate, fake_nova_internal_test.go).
func rescueCreate(t *testing.T, r *instanceRescueResource, image types.String) resource.CreateResponse {
	t.Helper()
	planned := instanceRescueModel{ID: types.StringUnknown(), InstanceID: types.StringValue("srv-1"), RescueImageID: image, Region: types.StringUnknown()}
	return runCreate(r, newPlan(t, schemaOf(t, r), &planned))
}

// Create sends the rescue image at 2.87, keeps no password, and waits past
// the rescuing task. A VM rescued out of ERROR shows ERROR until the task is
// done, which must not fail the wait.
func TestRescueCreate(t *testing.T) {
	f := &rescueFake{t: t, actionStatus: http.StatusOK, servers: []string{
		rescueServerJSON("ERROR", "rescuing"),
		rescueServerJSON("RESCUE", ""),
	}}
	resp := rescueCreate(t, f.start(), types.StringValue("img-rescue"))
	if resp.Diagnostics.HasError() {
		t.Fatalf("create: %v", resp.Diagnostics)
	}
	if len(f.actions) != 1 || f.actions[0] != `{"rescue":{"rescue_image_ref":"img-rescue"}}` {
		t.Errorf("rescue requests %v", f.actions)
	}
	if f.versions[0] != "2.87" {
		t.Errorf("rescue microversion %q, want 2.87 (below it Nova refuses a volume-backed instance)", f.versions[0])
	}
	if strings.Contains(fmt.Sprint(resp.State.Raw), "generated-secret") {
		t.Error("the rescue password reached state")
	}
	var got instanceRescueModel
	resp.State.Get(context.Background(), &got)
	if got.ID.ValueString() != "srv-1" || got.Region.ValueString() != "region-one" {
		t.Errorf("state id=%s region=%s", got.ID, got.Region)
	}
}

// Without rescue_image_id the body is empty, and Nova uses the instance's
// own image.
func TestRescueCreateWithoutImage(t *testing.T) {
	f := &rescueFake{t: t, actionStatus: http.StatusOK, servers: []string{rescueServerJSON("RESCUE", "")}}
	if resp := rescueCreate(t, f.start(), types.StringNull()); resp.Diagnostics.HasError() {
		t.Fatalf("create: %v", resp.Diagnostics)
	}
	if f.actions[0] != `{"rescue":{}}` {
		t.Errorf("rescue body %s, want {\"rescue\":{}}", f.actions[0])
	}
}

// A rescue Nova abandons before the driver runs leaves the instance as it
// was, with no task; create must report it instead of waiting for the
// timeout. Nova's 409 is reported as is.
func TestRescueCreateFailures(t *testing.T) {
	f := &rescueFake{t: t, actionStatus: http.StatusOK, servers: []string{
		rescueServerJSON("ACTIVE", "rescuing"),
		rescueServerJSON("ACTIVE", ""),
	}}
	resp := rescueCreate(t, f.start(), types.StringNull())
	if !resp.Diagnostics.HasError() || !strings.Contains(resp.Diagnostics.Errors()[0].Detail(), "without rescuing") {
		t.Errorf("diagnostics %v, want the failed rescue reported", resp.Diagnostics)
	}

	f2 := &rescueFake{t: t, actionStatus: http.StatusConflict}
	resp = rescueCreate(t, f2.start(), types.StringNull())
	if !resp.Diagnostics.HasError() || !resp.State.Raw.IsNull() {
		t.Errorf("409: error=%v state null=%v, want an error and no state", resp.Diagnostics.HasError(), resp.State.Raw.IsNull())
	}
}

// Read keeps the resource while the instance is rescued or mid-task, and
// drops it once the instance has left rescue mode or is gone.
func TestRescueRead(t *testing.T) {
	for _, tc := range []struct {
		server string
		keep   bool
	}{
		{rescueServerJSON("RESCUE", ""), true},
		{rescueServerJSON("ACTIVE", "rescuing"), true},
		{rescueServerJSON("RESCUE", "unrescuing"), true},
		{rescueServerJSON("ACTIVE", ""), false},
		{rescueServerJSON("SHUTOFF", ""), false},
		{"404", false},
	} {
		f := &rescueFake{t: t, servers: []string{tc.server}}
		resp := runRead(f.start(), rescueTFState(t, rescueModel(types.StringNull())))
		if resp.Diagnostics.HasError() {
			t.Fatalf("read %s: %v", tc.server, resp.Diagnostics)
		}
		if kept := !resp.State.Raw.IsNull(); kept != tc.keep {
			t.Errorf("server %s: kept=%v, want %v", tc.server, kept, tc.keep)
		}
	}
}

// Delete unrescues at the base microversion and waits for ACTIVE; an
// instance already out of rescue mode or gone needs nothing.
func TestRescueDelete(t *testing.T) {
	f := &rescueFake{t: t, actionStatus: http.StatusAccepted, servers: []string{
		rescueServerJSON("RESCUE", "unrescuing"),
		rescueServerJSON("ACTIVE", ""),
	}}
	st := rescueTFState(t, rescueModel(types.StringNull()))
	resp := runDelete(f.start(), st)
	if resp.Diagnostics.HasError() {
		t.Fatalf("delete: %v", resp.Diagnostics)
	}
	if f.actions[0] != `{"unrescue":null}` || f.versions[0] != "" {
		t.Errorf("unrescue body %s microversion %q", f.actions[0], f.versions[0])
	}
	if f.gets < 2 {
		t.Error("delete did not wait for the unrescue")
	}

	for _, tc := range []struct {
		name   string
		status int
		server string
	}{
		{"already unrescued", http.StatusConflict, rescueServerJSON("ACTIVE", "")},
		{"instance gone", http.StatusNotFound, "404"},
	} {
		f := &rescueFake{t: t, actionStatus: tc.status, servers: []string{tc.server}}
		if resp := runDelete(f.start(), st); resp.Diagnostics.HasError() {
			t.Errorf("%s: %v", tc.name, resp.Diagnostics)
		}
	}
}

// An unrescue that Nova abandons leaves the instance in RESCUE with no task.
func TestRescueDeleteThatStaysRescued(t *testing.T) {
	f := &rescueFake{t: t, actionStatus: http.StatusAccepted, servers: []string{
		rescueServerJSON("RESCUE", "unrescuing"),
		rescueServerJSON("RESCUE", ""),
	}}
	resp := runDelete(f.start(), rescueTFState(t, rescueModel(types.StringNull())))
	if !resp.Diagnostics.HasError() {
		t.Fatal("delete succeeded although the instance is still rescued")
	}
}

// A state row with no ID names no instance. Read drops it with a warning and
// Delete forgets it, and neither sends a request.
func TestRescueRowWithoutID(t *testing.T) {
	f := &rescueFake{t: t, actionStatus: http.StatusAccepted, servers: []string{rescueServerJSON("RESCUE", "")}}
	r := f.start()
	row := rescueModel(types.StringNull())
	row.ID, row.InstanceID = types.StringValue(""), types.StringValue("")
	read := runRead(r, rescueTFState(t, row))
	if read.Diagnostics.HasError() || len(read.Diagnostics.Warnings()) != 1 || !read.State.Raw.IsNull() {
		t.Errorf("read: diagnostics %v, state removed %v; want one warning and the row removed", read.Diagnostics, read.State.Raw.IsNull())
	}
	del := runDelete(r, rescueTFState(t, row))
	if del.Diagnostics.HasError() || len(del.Diagnostics.Warnings()) != 1 {
		t.Errorf("delete: diagnostics %v, want one warning", del.Diagnostics)
	}
	if f.gets != 0 || len(f.actions) != 0 {
		t.Errorf("%d reads and %d actions, want none", f.gets, len(f.actions))
	}
}

// Import takes the instance ID for both id and instance_id.
func TestRescueImportState(t *testing.T) {
	ctx := context.Background()
	var sch resource.SchemaResponse
	(&instanceRescueResource{}).Schema(ctx, resource.SchemaRequest{}, &sch)
	resp := resource.ImportStateResponse{State: tfsdk.State{Schema: sch.Schema, Raw: tftypes.NewValue(sch.Schema.Type().TerraformType(ctx), nil)}}
	(&instanceRescueResource{}).ImportState(ctx, resource.ImportStateRequest{ID: "srv-1"}, &resp)
	if resp.Diagnostics.HasError() {
		t.Fatal(resp.Diagnostics)
	}
	var got instanceRescueModel
	resp.State.Get(ctx, &got)
	if got.ID.ValueString() != "srv-1" || got.InstanceID.ValueString() != "srv-1" || !got.RescueImageID.IsNull() {
		t.Errorf("imported %+v", got)
	}
}
