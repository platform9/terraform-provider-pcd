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
	"sync"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

// These tests build their client with fakeConfig, and their schema, plans,
// states and calls with schemaOf, newPlan, newState, runCreate, runRead and
// runUpdate, all from fake_nova_internal_test.go. volumeAttachFake keeps its
// own server instead of fakeNova's route table, because it models one
// attachment whose flag a PUT changes and records each request's body and
// microversion.

// volumeAttachCall is one request the fake Nova and Cinder saw.
type volumeAttachCall struct {
	method, path, microversion string
	body                       map[string]any
}

// volumeAttachFake answers the compute and block-storage calls
// pcd_compute_volume_attach makes, records every request, and serves the
// attachment's delete_on_termination from deleteOnTermination (nil leaves the
// field out, as Nova does below microversion 2.79). A PUT updates it.
type volumeAttachFake struct {
	t                   *testing.T
	mu                  sync.Mutex
	calls               []volumeAttachCall
	deleteOnTermination *bool
}

func (f *volumeAttachFake) handler(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	call := volumeAttachCall{method: r.Method, path: r.URL.Path, microversion: r.Header.Get("X-OpenStack-Nova-API-Version")}
	if raw, _ := io.ReadAll(r.Body); len(raw) > 0 {
		if err := json.Unmarshal(raw, &call.body); err != nil {
			f.t.Errorf("%s %s: body is not JSON: %s", r.Method, r.URL.Path, raw)
		}
	}
	f.calls = append(f.calls, call)

	attachment := func() map[string]any {
		a := map[string]any{"id": "vol-1", "volumeId": "vol-1", "serverId": "srv-1", "device": "/dev/vdb"}
		if f.deleteOnTermination != nil {
			a["delete_on_termination"] = *f.deleteOnTermination
		}
		return map[string]any{"volumeAttachment": a}
	}
	switch r.Method + " " + r.URL.Path {
	case "POST /servers/srv-1/os-volume_attachments":
		att, _ := call.body["volumeAttachment"].(map[string]any)
		v, _ := att["delete_on_termination"].(bool)
		f.deleteOnTermination = &v
		_ = json.NewEncoder(w).Encode(attachment())
	case "GET /servers/srv-1/os-volume_attachments/vol-1":
		_ = json.NewEncoder(w).Encode(attachment())
	case "PUT /servers/srv-1/os-volume_attachments/vol-1":
		att, _ := call.body["volumeAttachment"].(map[string]any)
		v, _ := att["delete_on_termination"].(bool)
		f.deleteOnTermination = &v
		w.WriteHeader(http.StatusAccepted)
	case "GET /volumes/vol-1":
		fmt.Fprint(w, `{"volume": {"id": "vol-1", "status": "in-use"}}`)
	default:
		f.t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusNotImplemented)
	}
}

// requests returns the recorded calls whose method and path match.
func (f *volumeAttachFake) requests(method, path string) []volumeAttachCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []volumeAttachCall
	for _, c := range f.calls {
		if c.method == method && c.path == path {
			out = append(out, c)
		}
	}
	return out
}

// newVolumeAttachFake starts a fake for one test and returns it with a
// resource whose clients reach it (fakeConfig gives them a transport of their
// own).
func newVolumeAttachFake(t *testing.T, deleteOnTermination *bool) (*volumeAttachFake, *volumeAttachResource) {
	t.Helper()
	f := &volumeAttachFake{t: t, deleteOnTermination: deleteOnTermination}
	srv := httptest.NewServer(http.HandlerFunc(f.handler))
	t.Cleanup(srv.Close)
	return f, &volumeAttachResource{config: fakeConfig(srv.URL)}
}

// attachedModel is the state of srv-1's attachment of vol-1.
func attachedModel(deleteOnTermination types.Bool) volumeAttachModel {
	return volumeAttachModel{
		ID:                  types.StringValue("vol-1"),
		InstanceID:          types.StringValue("srv-1"),
		VolumeID:            types.StringValue("vol-1"),
		Device:              types.StringValue("/dev/vdb"),
		Region:              types.StringValue("region-one"),
		DeleteOnTermination: deleteOnTermination,
	}
}

func attachFlag(b bool) *bool { return &b }

// The attach request carries delete_on_termination only when it is true:
// gophercloud drops false (omitempty), and Nova's default is false, so an
// unset or false value attaches exactly as before. Sending false explicitly
// is not needed and would fail on a Nova below microversion 2.79.
func TestVolumeAttachCreateOptsDeleteOnTermination(t *testing.T) {
	for _, tc := range []struct {
		name     string
		value    types.Bool
		wantSent bool
	}{
		{name: "true", value: types.BoolValue(true), wantSent: true},
		{name: "false", value: types.BoolValue(false)},
		{name: "unset", value: types.BoolUnknown()},
		{name: "null", value: types.BoolNull()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := attachedModel(tc.value)
			m.Device = types.StringUnknown()
			body, err := volumeAttachCreateOpts(&m).ToVolumeAttachmentCreateMap()
			if err != nil {
				t.Fatal(err)
			}
			att := body["volumeAttachment"].(map[string]any)
			got, sent := att["delete_on_termination"]
			if sent != tc.wantSent {
				t.Fatalf("delete_on_termination sent = %v, want %v (body %v)", sent, tc.wantSent, att)
			}
			if sent && got != true {
				t.Fatalf("delete_on_termination = %v, want true", got)
			}
			if att["volumeId"] != "vol-1" {
				t.Fatalf("volumeId = %v, want vol-1", att["volumeId"])
			}
		})
	}
}

// Create speaks compute 2.85 whether or not the flag is set. That records the
// flag Nova applied, and it also lifts Nova's multiattach gate: below 2.60 Nova
// refuses to attach a multiattach volume at all.
func TestVolumeAttachCreatePinsTheMicroversion(t *testing.T) {
	for _, tc := range []struct {
		name    string
		planned types.Bool
		want    bool
	}{
		{name: "set to true", planned: types.BoolValue(true), want: true},
		{name: "unset", planned: types.BoolUnknown(), want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, r := newVolumeAttachFake(t, nil)
			s := schemaOf(t, r)
			planned := volumeAttachModel{
				ID:                  types.StringUnknown(),
				InstanceID:          types.StringValue("srv-1"),
				VolumeID:            types.StringValue("vol-1"),
				Device:              types.StringUnknown(),
				Region:              types.StringUnknown(),
				DeleteOnTermination: tc.planned,
			}
			resp := runCreate(r, newPlan(t, s, &planned))
			if resp.Diagnostics.HasError() {
				t.Fatalf("create: %v", resp.Diagnostics)
			}
			posts := f.requests("POST", "/servers/srv-1/os-volume_attachments")
			if len(posts) != 1 {
				t.Fatalf("attach requests = %d, want 1", len(posts))
			}
			if posts[0].microversion != computeMicroversionVolumeAttachUpdate {
				t.Fatalf("attach sent compute microversion %q, want %q: without it Nova ignores the flag and refuses multiattach volumes",
					posts[0].microversion, computeMicroversionVolumeAttachUpdate)
			}
			var got volumeAttachModel
			if d := resp.State.Get(context.Background(), &got); d.HasError() {
				t.Fatalf("reading the state: %v", d)
			}
			if !resp.State.Raw.IsFullyKnown() {
				t.Fatalf("create state holds unknown values: %v", resp.State.Raw)
			}
			if got.DeleteOnTermination.ValueBool() != tc.want || got.ID.ValueString() != "vol-1" {
				t.Fatalf("state delete_on_termination=%s id=%s, want %v and vol-1", got.DeleteOnTermination, got.ID, tc.want)
			}
		})
	}
}

// Toggling the flag is an in-place PUT, not a detach and re-attach. The path
// and the body name the same volume: a different volumeId in the body would be
// a swap-volume request, which Nova reserves for admins and for Cinder
// migrations.
func TestVolumeAttachUpdateTogglesTheFlagInPlace(t *testing.T) {
	f, r := newVolumeAttachFake(t, attachFlag(true))
	s := schemaOf(t, r)
	prior := attachedModel(types.BoolValue(true))
	planned := attachedModel(types.BoolValue(false))
	resp := runUpdate(r, newPlan(t, s, &planned), newState(t, s, &prior))
	if resp.Diagnostics.HasError() {
		t.Fatalf("update: %v", resp.Diagnostics)
	}

	puts := f.requests("PUT", "/servers/srv-1/os-volume_attachments/vol-1")
	if len(puts) != 1 {
		t.Fatalf("PUT requests = %d, want exactly 1", len(puts))
	}
	if puts[0].microversion != computeMicroversionVolumeAttachUpdate {
		t.Fatalf("PUT sent compute microversion %q, want %q (below 2.85 a PUT is always a swap-volume)",
			puts[0].microversion, computeMicroversionVolumeAttachUpdate)
	}
	want := map[string]any{"volumeAttachment": map[string]any{"volumeId": "vol-1", "delete_on_termination": false}}
	if fmt.Sprint(puts[0].body) != fmt.Sprint(want) {
		t.Fatalf("PUT body = %v, want %v", puts[0].body, want)
	}
	if n := len(f.requests("POST", "/servers/srv-1/os-volume_attachments")) + len(f.requests("DELETE", "/servers/srv-1/os-volume_attachments/vol-1")); n != 0 {
		t.Fatalf("update detached or re-attached the volume (%d calls); the flag must change in place", n)
	}
	var got volumeAttachModel
	if d := resp.State.Get(context.Background(), &got); d.HasError() {
		t.Fatalf("reading the state: %v", d)
	}
	if got.DeleteOnTermination.ValueBool() {
		t.Fatal("state delete_on_termination = true after the update, want false")
	}
}

// An unset delete_on_termination is unmanaged: an update for another reason
// (a region change) must not write the flag, so a value set in the PCD UI
// survives. A configured value that already matches Nova is not sent either.
func TestVolumeAttachUpdateLeavesTheFlagAloneWhenNothingChanges(t *testing.T) {
	for _, tc := range []struct {
		name    string
		planned types.Bool
	}{
		{name: "unset", planned: types.BoolUnknown()},
		{name: "already true", planned: types.BoolValue(true)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, r := newVolumeAttachFake(t, attachFlag(true))
			s := schemaOf(t, r)
			prior := attachedModel(types.BoolValue(true))
			prior.Region = types.StringValue("region-zero")
			planned := attachedModel(tc.planned)
			resp := runUpdate(r, newPlan(t, s, &planned), newState(t, s, &prior))
			if resp.Diagnostics.HasError() {
				t.Fatalf("update: %v", resp.Diagnostics)
			}
			if n := len(f.requests("PUT", "/servers/srv-1/os-volume_attachments/vol-1")); n != 0 {
				t.Fatalf("PUT requests = %d, want 0", n)
			}
			var got volumeAttachModel
			if d := resp.State.Get(context.Background(), &got); d.HasError() {
				t.Fatalf("reading the state: %v", d)
			}
			if !got.DeleteOnTermination.ValueBool() || got.Region.ValueString() != "region-one" {
				t.Fatalf("state delete_on_termination=%s region=%s, want true and region-one", got.DeleteOnTermination, got.Region)
			}
		})
	}
}

// Read reports the flag Nova holds, so a change made in the PCD UI shows as
// drift. A response without the field (a Nova below 2.79) reads as false,
// Nova's behavior there.
func TestVolumeAttachReadReportsTheFlag(t *testing.T) {
	for _, tc := range []struct {
		name   string
		served *bool
		want   bool
	}{
		{name: "true", served: attachFlag(true), want: true},
		{name: "false", served: attachFlag(false), want: false},
		{name: "absent", served: nil, want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, r := newVolumeAttachFake(t, tc.served)
			s := schemaOf(t, r)
			prior := attachedModel(types.BoolNull())
			resp := runRead(r, newState(t, s, &prior))
			if resp.Diagnostics.HasError() {
				t.Fatalf("read: %v", resp.Diagnostics)
			}
			gets := f.requests("GET", "/servers/srv-1/os-volume_attachments/vol-1")
			if len(gets) != 1 || gets[0].microversion != computeMicroversionVolumeAttachUpdate {
				t.Fatalf("read requests %+v, want one GET at %s", gets, computeMicroversionVolumeAttachUpdate)
			}
			var got volumeAttachModel
			if d := resp.State.Get(context.Background(), &got); d.HasError() {
				t.Fatalf("reading the state: %v", d)
			}
			if got.DeleteOnTermination.IsNull() || got.DeleteOnTermination.ValueBool() != tc.want {
				t.Fatalf("delete_on_termination = %s, want %v", got.DeleteOnTermination, tc.want)
			}
		})
	}
}

// delete_on_termination updates in place and is unmanaged when unset: no
// plan modifier (RequiresReplace would detach the volume, UseStateForUnknown
// would write a stale value back), and no default (a default would revert a
// flag set in the PCD UI on the first apply after an upgrade).
func TestVolumeAttachDeleteOnTerminationSchema(t *testing.T) {
	attr, ok := schemaOf(t, &volumeAttachResource{}).Attributes["delete_on_termination"].(schema.BoolAttribute)
	if !ok {
		t.Fatal("delete_on_termination is missing or not a bool")
	}
	if !attr.Optional || !attr.Computed || attr.Required {
		t.Fatalf("delete_on_termination optional=%v computed=%v required=%v, want optional and computed", attr.Optional, attr.Computed, attr.Required)
	}
	if attr.Default != nil {
		t.Fatal("delete_on_termination has a default; unset must mean unmanaged")
	}
	if len(attr.PlanModifiers) != 0 {
		t.Fatalf("delete_on_termination has %d plan modifiers, want none", len(attr.PlanModifiers))
	}
}
