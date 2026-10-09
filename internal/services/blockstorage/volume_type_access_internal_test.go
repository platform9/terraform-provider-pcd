// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package blockstorage

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

	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

// typeAccessCinder is a fake Cinder for the volume type access resource:
// actionCode answers POST /types/vt-1/action (recording each body), and
// listCode/listBody answer GET /types/vt-1/os-volume-type-access. The tests
// reach it through the package's shared fakeConfig
// (failed_create_internal_test.go), which gives each client a transport of
// its own.
type typeAccessCinder struct {
	mu         sync.Mutex
	actionCode int
	listCode   int
	listBody   string
	posts      []map[string]any
}

func (c *typeAccessCinder) serve(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c.mu.Lock()
		defer c.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch r.Method + " " + r.URL.Path {
		case "POST /types/vt-1/action":
			raw, _ := io.ReadAll(r.Body)
			var body map[string]any
			if err := json.Unmarshal(raw, &body); err != nil {
				t.Errorf("action body is not JSON: %s", raw)
			}
			c.posts = append(c.posts, body)
			w.WriteHeader(c.actionCode)
			if c.actionCode >= 400 {
				fmt.Fprintf(w, `{"badRequest": {"code": %d, "message": "Type access modification is not applicable to public volume type."}}`, c.actionCode)
			}
		case "GET /types/vt-1/os-volume-type-access":
			w.WriteHeader(c.listCode)
			fmt.Fprint(w, c.listBody)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotImplemented)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func (c *typeAccessCinder) actionBodies() []map[string]any {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]map[string]any(nil), c.posts...)
}

func volumeTypeAccessSchema(ctx context.Context, t *testing.T) schema.Schema {
	t.Helper()
	var sch resource.SchemaResponse
	(&volumeTypeAccessResource{}).Schema(ctx, resource.SchemaRequest{}, &sch)
	if sch.Diagnostics.HasError() {
		t.Fatalf("schema: %v", sch.Diagnostics)
	}
	return sch.Schema
}

func volumeTypeAccessState(ctx context.Context, t *testing.T) tfsdk.State {
	t.Helper()
	s := volumeTypeAccessSchema(ctx, t)
	st := tfsdk.State{Schema: s, Raw: tftypes.NewValue(s.Type().TerraformType(ctx), nil)}
	if d := st.Set(ctx, &volumeTypeAccessModel{
		ID:           types.StringValue("vt-1/p-1"),
		VolumeTypeID: types.StringValue("vt-1"),
		ProjectID:    types.StringValue("p-1"),
		Region:       types.StringValue("region-one"),
	}); d.HasError() {
		t.Fatalf("building the state: %v", d)
	}
	return st
}

func volumeTypeAccessCreate(ctx context.Context, t *testing.T, r *volumeTypeAccessResource) resource.CreateResponse {
	t.Helper()
	s := volumeTypeAccessSchema(ctx, t)
	plan := tfsdk.Plan{Schema: s, Raw: tftypes.NewValue(s.Type().TerraformType(ctx), nil)}
	if d := plan.Set(ctx, &volumeTypeAccessModel{
		ID:           types.StringUnknown(),
		VolumeTypeID: types.StringValue("vt-1"),
		ProjectID:    types.StringValue("p-1"),
		Region:       types.StringUnknown(),
	}); d.HasError() {
		t.Fatalf("building the plan: %v", d)
	}
	resp := resource.CreateResponse{State: tfsdk.State{Schema: s, Raw: tftypes.NewValue(s.Type().TerraformType(ctx), nil)}}
	r.Create(ctx, resource.CreateRequest{Plan: plan}, &resp)
	return resp
}

// Create sends the UI's grant body; an existing grant (409) is adopted; a
// public type (400) fails with the provider's hint and no state.
func TestVolumeTypeAccessCreate(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name        string
		code        int
		wantErr     string
		wantStateID string
	}{
		{name: "granted", code: http.StatusAccepted, wantStateID: "vt-1/p-1"},
		{name: "already granted", code: http.StatusConflict, wantStateID: "vt-1/p-1"},
		{name: "public type", code: http.StatusBadRequest, wantErr: "Volume type access refused"},
		{name: "forbidden", code: http.StatusForbidden, wantErr: "blockstorage: granting volume type access"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cinder := &typeAccessCinder{actionCode: tc.code}
			srv := cinder.serve(t)
			r := &volumeTypeAccessResource{config: fakeConfig(srv.URL)}
			resp := volumeTypeAccessCreate(ctx, t, r)

			posts := cinder.actionBodies()
			if len(posts) != 1 {
				t.Fatalf("POST /types/vt-1/action sent %d times, want 1", len(posts))
			}
			if grant, _ := posts[0]["addProjectAccess"].(map[string]any); grant["project"] != "p-1" {
				t.Fatalf("action body = %v, want {\"addProjectAccess\": {\"project\": \"p-1\"}}", posts[0])
			}
			if tc.wantErr != "" {
				if !resp.Diagnostics.HasError() || resp.Diagnostics.Errors()[0].Summary() != tc.wantErr {
					t.Fatalf("diagnostics = %v, want an error %q", resp.Diagnostics, tc.wantErr)
				}
				if !resp.State.Raw.IsNull() {
					t.Fatal("a refused grant left state behind")
				}
				return
			}
			if resp.Diagnostics.HasError() {
				t.Fatalf("create: %v", resp.Diagnostics)
			}
			var got volumeTypeAccessModel
			if d := resp.State.Get(ctx, &got); d.HasError() {
				t.Fatal(d)
			}
			if got.ID.ValueString() != tc.wantStateID || got.Region.ValueString() != "region-one" {
				t.Fatalf("state id=%s region=%s, want %s and region-one", got.ID, got.Region, tc.wantStateID)
			}
		})
	}
}

// Read keeps a listed grant and drops one that was revoked, whose type was
// deleted, or whose type turned public (Cinder's list answers 404 for both);
// any other error keeps state. Upstream errored on a missing grant instead.
func TestVolumeTypeAccessRead(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name        string
		code        int
		body        string
		wantRemoved bool
		wantErr     bool
	}{
		{name: "listed", code: http.StatusOK, body: `{"volume_type_access": [{"volume_type_id": "vt-1", "project_id": "p-1"}]}`},
		{name: "revoked in the UI", code: http.StatusOK, body: `{"volume_type_access": [{"volume_type_id": "vt-1", "project_id": "p-2"}]}`, wantRemoved: true},
		{name: "type deleted or public", code: http.StatusNotFound, body: `{"itemNotFound": {"code": 404}}`, wantRemoved: true},
		{name: "forbidden", code: http.StatusForbidden, body: `{"forbidden": {"code": 403}}`, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cinder := &typeAccessCinder{listCode: tc.code, listBody: tc.body}
			srv := cinder.serve(t)
			r := &volumeTypeAccessResource{config: fakeConfig(srv.URL)}
			st := volumeTypeAccessState(ctx, t)
			resp := resource.ReadResponse{State: st}
			r.Read(ctx, resource.ReadRequest{State: st}, &resp)
			if got := resp.Diagnostics.HasError(); got != tc.wantErr {
				t.Fatalf("read error = %v, want %v: %v", got, tc.wantErr, resp.Diagnostics)
			}
			if got := resp.State.Raw.IsNull(); got != tc.wantRemoved {
				t.Fatalf("removed from state = %v, want %v", got, tc.wantRemoved)
			}
		})
	}
}

// Delete revokes with the UI's body. A grant or type already gone (404) and a
// type that turned public (400) both count as done, so a configuration that
// flips is_public and drops the grant in one apply never gets stuck.
func TestVolumeTypeAccessDelete(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name     string
		code     int
		wantErr  bool
		wantWarn bool
	}{
		{name: "revoked", code: http.StatusAccepted},
		{name: "already gone", code: http.StatusNotFound},
		{name: "type now public", code: http.StatusBadRequest, wantWarn: true},
		{name: "forbidden", code: http.StatusForbidden, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cinder := &typeAccessCinder{actionCode: tc.code}
			srv := cinder.serve(t)
			r := &volumeTypeAccessResource{config: fakeConfig(srv.URL)}
			st := volumeTypeAccessState(ctx, t)
			resp := resource.DeleteResponse{State: st}
			r.Delete(ctx, resource.DeleteRequest{State: st}, &resp)
			if got := resp.Diagnostics.HasError(); got != tc.wantErr {
				t.Fatalf("delete error = %v, want %v: %v", got, tc.wantErr, resp.Diagnostics)
			}
			if got := resp.Diagnostics.WarningsCount() > 0; got != tc.wantWarn {
				t.Fatalf("delete warning = %v, want %v", got, tc.wantWarn)
			}
			posts := cinder.actionBodies()
			if len(posts) != 1 {
				t.Fatalf("POST /types/vt-1/action sent %d times, want 1", len(posts))
			}
			if revoke, _ := posts[0]["removeProjectAccess"].(map[string]any); revoke["project"] != "p-1" {
				t.Fatalf("action body = %v, want {\"removeProjectAccess\": {\"project\": \"p-1\"}}", posts[0])
			}
		})
	}
}

func TestVolumeTypeAccessImportState(t *testing.T) {
	ctx := context.Background()
	s := volumeTypeAccessSchema(ctx, t)
	resp := resource.ImportStateResponse{State: tfsdk.State{Schema: s, Raw: tftypes.NewValue(s.Type().TerraformType(ctx), nil)}}
	(&volumeTypeAccessResource{}).ImportState(ctx, resource.ImportStateRequest{ID: "vt-1/p-1"}, &resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("import vt-1/p-1: %v", resp.Diagnostics)
	}
	var got volumeTypeAccessModel
	if d := resp.State.Get(ctx, &got); d.HasError() {
		t.Fatal(d)
	}
	if got.VolumeTypeID.ValueString() != "vt-1" || got.ProjectID.ValueString() != "p-1" {
		t.Fatalf("imported volume_type_id=%s project_id=%s, want vt-1, p-1", got.VolumeTypeID, got.ProjectID)
	}
	for _, bad := range []string{"vt-1", "vt-1/", "/p-1", ""} {
		resp := resource.ImportStateResponse{State: tfsdk.State{Schema: s, Raw: tftypes.NewValue(s.Type().TerraformType(ctx), nil)}}
		(&volumeTypeAccessResource{}).ImportState(ctx, resource.ImportStateRequest{ID: bad}, &resp)
		if !resp.Diagnostics.HasError() || !strings.Contains(resp.Diagnostics.Errors()[0].Detail(), "<volume_type_id>/<project_id>") {
			t.Errorf("import %q: diagnostics %v, want an error naming <volume_type_id>/<project_id>", bad, resp.Diagnostics)
		}
	}
}
