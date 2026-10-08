// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package compute

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/action"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

// rebootFake is a Nova that answers a reboot and then replays the given GET
// replies (status, task state), repeating the last one.
type rebootFake struct {
	mu      sync.Mutex
	bodies  []map[string]map[string]string
	replies []novaState
	gets    int
	code    int // the reboot's HTTP status; 202 when zero
}

func newRebootAction(t *testing.T, f *rebootFake) *instanceRebootAction {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		switch r.Method + " " + r.URL.Path {
		case "POST /servers/srv-1/action":
			var body map[string]map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			f.bodies = append(f.bodies, body)
			if f.code != 0 {
				w.WriteHeader(f.code)
				fmt.Fprint(w, `{"conflictingRequest": {"code": 409, "message": "Cannot 'reboot' instance while it is in vm_state stopped"}}`)
				return
			}
			w.WriteHeader(http.StatusAccepted)
		case "GET /servers/srv-1":
			s := f.replies[min(f.gets, len(f.replies)-1)]
			f.gets++
			task := "null"
			if s.task != "" {
				task = fmt.Sprintf("%q", s.task)
			}
			fmt.Fprintf(w, `{"server": {"id": "srv-1", "status": %q, "OS-EXT-STS:task_state": %s,
				"fault": {"code": 500, "message": "hard reboot failed"}}}`, s.status, task)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotImplemented)
		}
	}))
	t.Cleanup(srv.Close)
	return &instanceRebootAction{config: fakeConfig(srv.URL)}
}

// invokeReboot runs Invoke as the framework does, with a config built from
// the action's own schema; rebootType nil means type is not set.
func invokeReboot(t *testing.T, a *instanceRebootAction, rebootType any) (*action.InvokeResponse, []string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	var sch action.SchemaResponse
	a.Schema(ctx, action.SchemaRequest{}, &sch)
	if sch.Diagnostics.HasError() {
		t.Fatalf("schema: %v", sch.Diagnostics)
	}
	raw := tftypes.NewValue(sch.Schema.Type().TerraformType(ctx), map[string]tftypes.Value{
		"instance_id": tftypes.NewValue(tftypes.String, "srv-1"),
		"type":        tftypes.NewValue(tftypes.String, rebootType),
		"region":      tftypes.NewValue(tftypes.String, nil),
	})
	var progress []string
	resp := &action.InvokeResponse{SendProgress: func(e action.InvokeProgressEvent) { progress = append(progress, e.Message) }}
	a.Invoke(ctx, action.InvokeRequest{Config: tfsdk.Config{Schema: sch.Schema, Raw: raw}}, resp)
	return resp, progress
}

func TestInstanceRebootActionMetadata(t *testing.T) {
	var resp action.MetadataResponse
	(&instanceRebootAction{}).Metadata(context.Background(), action.MetadataRequest{ProviderTypeName: "pcd"}, &resp)
	if resp.TypeName != "pcd_compute_instance_reboot" {
		t.Fatalf("type name = %q", resp.TypeName)
	}
}

// The reboot type defaults to SOFT, the UI's plain Reboot; the action waits
// out Nova's reboot task and reports progress before the request, before the
// wait and after it.
func TestInstanceRebootActionSendsTheRequestedType(t *testing.T) {
	for _, tc := range []struct {
		name       string
		rebootType any
		want       string
	}{
		{"default", nil, "SOFT"},
		{"soft", "SOFT", "SOFT"},
		{"hard", "HARD", "HARD"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			shortPolls(t)
			f := &rebootFake{replies: []novaState{{"REBOOT", "rebooting"}, {"ACTIVE", ""}}}
			resp, progress := invokeReboot(t, newRebootAction(t, f), tc.rebootType)
			if resp.Diagnostics.HasError() {
				t.Fatalf("invoke: %v", resp.Diagnostics)
			}
			if len(f.bodies) != 1 || f.bodies[0]["reboot"]["type"] != tc.want {
				t.Fatalf("reboot bodies = %v, want one {\"reboot\":{\"type\":%q}}", f.bodies, tc.want)
			}
			if f.gets < 2 {
				t.Fatalf("returned after %d GETs; want it to wait out the reboot task", f.gets)
			}
			if len(progress) == 0 {
				t.Fatal("no progress reported")
			}
		})
	}
}

// Hard reboot is the way out of ERROR, and Nova keeps reporting ERROR until
// the reboot is done. The action must wait for the task, not fail on the
// first ERROR it sees.
func TestInstanceRebootActionRecoversAnInstanceInError(t *testing.T) {
	shortPolls(t)
	f := &rebootFake{replies: []novaState{{"ERROR", "reboot_started_hard"}, {"ERROR", "reboot_started_hard"}, {"ACTIVE", ""}}}
	resp, _ := invokeReboot(t, newRebootAction(t, f), "HARD")
	if resp.Diagnostics.HasError() {
		t.Fatalf("hard reboot of an ERROR instance failed: %v", resp.Diagnostics)
	}
}

// A reboot that ends in ERROR or in another status than ACTIVE, or that
// Nova refuses, fails the action with the reason.
func TestInstanceRebootActionReportsFailure(t *testing.T) {
	shortPolls(t)
	f := &rebootFake{replies: []novaState{{"HARD_REBOOT", "reboot_started_hard"}, {"ERROR", ""}}}
	resp, _ := invokeReboot(t, newRebootAction(t, f), "HARD")
	if !resp.Diagnostics.HasError() || resp.Diagnostics.Errors()[0].Summary() != "compute: waiting for instance to reboot" {
		t.Fatalf("diagnostics = %v, want the ERROR reported", resp.Diagnostics)
	}

	stopped := &rebootFake{replies: []novaState{{"REBOOT", "rebooting"}, {"SHUTOFF", ""}}}
	resp, _ = invokeReboot(t, newRebootAction(t, stopped), "SOFT")
	if !resp.Diagnostics.HasError() || resp.Diagnostics.Errors()[0].Summary() != "compute: waiting for instance to reboot" {
		t.Fatalf("diagnostics = %v, want the stopped instance reported", resp.Diagnostics)
	}
	if stopped.gets != 2 {
		t.Fatalf("%d GETs; a settled SHUTOFF must end the wait at once", stopped.gets)
	}

	refused := &rebootFake{code: http.StatusConflict, replies: []novaState{{"SHUTOFF", ""}}}
	resp, _ = invokeReboot(t, newRebootAction(t, refused), "SOFT")
	if !resp.Diagnostics.HasError() || resp.Diagnostics.Errors()[0].Summary() != "compute: rebooting instance" {
		t.Fatalf("diagnostics = %v, want Nova's refusal reported", resp.Diagnostics)
	}
	if refused.gets != 0 {
		t.Fatalf("polled %d times after Nova refused the reboot", refused.gets)
	}
}
