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

	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

// These tests reuse the package's shared fake plumbing in
// fake_nova_internal_test.go: fakeConfig (a client with a transport of its
// own), schemaOf, newPlan, newState, runCreate, runRead and runDelete.

// flavorAccessNova is a fake Nova for the flavor access resource. Each field
// is the status and body one endpoint answers; posts records every action body
// so a test can assert what was sent, or that nothing was.
type flavorAccessNova struct {
	mu         sync.Mutex
	flavorGet  string // body for GET /flavors/f1; "" answers 404
	actionCode int    // status for POST /flavors/f1/action
	listCode   int    // status for GET /flavors/f1/os-flavor-access
	listBody   string
	posts      []map[string]any
}

func (n *flavorAccessNova) serve(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.mu.Lock()
		defer n.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch r.Method + " " + r.URL.Path {
		case "GET /flavors/f1":
			if n.flavorGet == "" {
				w.WriteHeader(http.StatusNotFound)
				fmt.Fprint(w, `{"itemNotFound": {"code": 404, "message": "Flavor f1 could not be found."}}`)
				return
			}
			fmt.Fprint(w, n.flavorGet)
		case "POST /flavors/f1/action":
			raw, _ := io.ReadAll(r.Body)
			var body map[string]any
			if err := json.Unmarshal(raw, &body); err != nil {
				t.Errorf("action body is not JSON: %s", raw)
			}
			n.posts = append(n.posts, body)
			w.WriteHeader(n.actionCode)
			if n.actionCode == http.StatusOK {
				fmt.Fprint(w, `{"flavor_access": [{"flavor_id": "f1", "tenant_id": "p1"}]}`)
				return
			}
			fmt.Fprintf(w, `{"error": {"code": %d, "message": "refused"}}`, n.actionCode)
		case "GET /flavors/f1/os-flavor-access":
			w.WriteHeader(n.listCode)
			fmt.Fprint(w, n.listBody)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotImplemented)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func (n *flavorAccessNova) actionBodies() []map[string]any {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]map[string]any(nil), n.posts...)
}

const flavorAccessPrivateFlavor = `{"flavor": {"id": "f1", "name": "gpu", "os-flavor-access:is_public": false}}`

// flavorAccessCreate runs Create with the plan a first apply produces.
func flavorAccessCreate(t *testing.T, r *flavorAccessResource) resource.CreateResponse {
	t.Helper()
	return runCreate(r, newPlan(t, schemaOf(t, r), &flavorAccessModel{
		ID:       types.StringUnknown(),
		FlavorID: types.StringValue("f1"),
		TenantID: types.StringValue("p1"),
		Region:   types.StringUnknown(),
	}))
}

// flavorAccessState is the state Create writes for f1/p1.
func flavorAccessState(t *testing.T) tfsdk.State {
	t.Helper()
	return newState(t, schemaOf(t, &flavorAccessResource{}), &flavorAccessModel{
		ID:       types.StringValue("f1/p1"),
		FlavorID: types.StringValue("f1"),
		TenantID: types.StringValue("p1"),
		Region:   types.StringValue("region-one"),
	})
}

// The grant goes out as exactly the body the PCD UI sends, and state carries
// the composite ID an import would use.
func TestFlavorAccessCreateGrants(t *testing.T) {
	nova := &flavorAccessNova{flavorGet: flavorAccessPrivateFlavor, actionCode: http.StatusOK}
	srv := nova.serve(t)
	r := &flavorAccessResource{config: fakeConfig(srv.URL)}

	resp := flavorAccessCreate(t, r)
	if resp.Diagnostics.HasError() {
		t.Fatalf("create: %v", resp.Diagnostics)
	}
	posts := nova.actionBodies()
	if len(posts) != 1 {
		t.Fatalf("POST /flavors/f1/action sent %d times, want 1", len(posts))
	}
	grant, _ := posts[0]["addTenantAccess"].(map[string]any)
	if len(posts[0]) != 1 || grant["tenant"] != "p1" {
		t.Fatalf("action body = %v, want {\"addTenantAccess\": {\"tenant\": \"p1\"}}", posts[0])
	}
	var got flavorAccessModel
	if d := resp.State.Get(context.Background(), &got); d.HasError() {
		t.Fatalf("reading state: %v", d)
	}
	if got.ID.ValueString() != "f1/p1" || got.Region.ValueString() != "region-one" {
		t.Fatalf("state id=%s region=%s, want f1/p1 and region-one", got.ID, got.Region)
	}
}

// A grant made before Terraform (in the UI, say) answers 409; it is adopted
// rather than failing the apply, which would leave no way forward but import.
func TestFlavorAccessCreateAdoptsAnExistingGrant(t *testing.T) {
	nova := &flavorAccessNova{flavorGet: flavorAccessPrivateFlavor, actionCode: http.StatusConflict}
	srv := nova.serve(t)
	r := &flavorAccessResource{config: fakeConfig(srv.URL)}

	resp := flavorAccessCreate(t, r)
	if resp.Diagnostics.HasError() {
		t.Fatalf("create with an existing grant: %v", resp.Diagnostics)
	}
	if resp.State.Raw.IsNull() {
		t.Fatal("create adopted nothing: the existing grant is not in state")
	}
}

// A public flavor is refused before any grant is sent: Nova at the base
// microversion would accept it, and every later refresh would then lose it.
func TestFlavorAccessCreateRefusesAPublicFlavor(t *testing.T) {
	nova := &flavorAccessNova{
		flavorGet:  `{"flavor": {"id": "f1", "name": "m1.small", "os-flavor-access:is_public": true}}`,
		actionCode: http.StatusOK,
	}
	srv := nova.serve(t)
	r := &flavorAccessResource{config: fakeConfig(srv.URL)}

	resp := flavorAccessCreate(t, r)
	if !resp.Diagnostics.HasError() {
		t.Fatal("create on a public flavor succeeded; the plan would never converge")
	}
	if summary := resp.Diagnostics.Errors()[0].Summary(); summary != "Flavor is public" {
		t.Fatalf("error summary = %q, want %q (acceptance tests match it)", summary, "Flavor is public")
	}
	if n := len(nova.actionBodies()); n != 0 {
		t.Fatalf("sent %d grant requests for a public flavor, want 0", n)
	}
	if !resp.State.Raw.IsNull() {
		t.Fatal("a refused grant left state behind")
	}
}

// Nova's 400 (unknown project), a missing flavor, and a 200 for the flavor
// whose body holds no flavor object surface as errors with no state and, for
// the last two, no grant sent.
func TestFlavorAccessCreateSurfacesErrors(t *testing.T) {
	for _, tc := range []struct {
		name      string
		nova      *flavorAccessNova
		wantPosts int
	}{
		{name: "unknown project", nova: &flavorAccessNova{flavorGet: flavorAccessPrivateFlavor, actionCode: http.StatusBadRequest}, wantPosts: 1},
		{name: "flavor missing", nova: &flavorAccessNova{actionCode: http.StatusOK}},
		{name: "answer without the flavor", nova: &flavorAccessNova{flavorGet: `{"flavors": []}`, actionCode: http.StatusOK}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := tc.nova.serve(t)
			r := &flavorAccessResource{config: fakeConfig(srv.URL)}
			resp := flavorAccessCreate(t, r)
			if !resp.Diagnostics.HasError() {
				t.Fatal("create succeeded; want the error surfaced")
			}
			if !resp.State.Raw.IsNull() {
				t.Fatal("a failed grant left state behind")
			}
			if n := len(tc.nova.actionBodies()); n != tc.wantPosts {
				t.Fatalf("sent %d grant requests, want %d", n, tc.wantPosts)
			}
		})
	}
}

// Read keeps a listed grant, drops one that was revoked or whose flavor is
// gone, and surfaces every other error instead of dropping the grant.
func TestFlavorAccessRead(t *testing.T) {
	for _, tc := range []struct {
		name        string
		code        int
		body        string
		wantRemoved bool
		wantErr     bool
	}{
		{name: "listed", code: http.StatusOK, body: `{"flavor_access": [{"flavor_id": "f1", "tenant_id": "p0"}, {"flavor_id": "f1", "tenant_id": "p1"}]}`},
		{name: "revoked in the UI", code: http.StatusOK, body: `{"flavor_access": [{"flavor_id": "f1", "tenant_id": "p0"}]}`, wantRemoved: true},
		{name: "empty list", code: http.StatusOK, body: `{"flavor_access": []}`, wantRemoved: true},
		{name: "flavor deleted", code: http.StatusNotFound, body: `{"itemNotFound": {"code": 404}}`, wantRemoved: true},
		{name: "forbidden", code: http.StatusForbidden, body: `{"forbidden": {"code": 403}}`, wantErr: true},
		{name: "server error", code: http.StatusInternalServerError, body: `{"computeFault": {"code": 500}}`, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			nova := &flavorAccessNova{listCode: tc.code, listBody: tc.body}
			srv := nova.serve(t)
			r := &flavorAccessResource{config: fakeConfig(srv.URL)}
			resp := runRead(r, flavorAccessState(t))
			if got := resp.Diagnostics.HasError(); got != tc.wantErr {
				t.Fatalf("read error = %v, want %v: %v", got, tc.wantErr, resp.Diagnostics)
			}
			if tc.wantErr {
				if resp.State.Raw.IsNull() {
					t.Fatal("an API error dropped the grant from state; the next plan would re-grant it")
				}
				return
			}
			if got := resp.State.Raw.IsNull(); got != tc.wantRemoved {
				t.Fatalf("removed from state = %v, want %v", got, tc.wantRemoved)
			}
		})
	}
}

// Delete revokes with the UI's body, and a grant or flavor already gone is
// success.
func TestFlavorAccessDelete(t *testing.T) {
	for _, tc := range []struct {
		name    string
		code    int
		wantErr bool
	}{
		{name: "revoked", code: http.StatusOK},
		{name: "already gone", code: http.StatusNotFound},
		{name: "forbidden", code: http.StatusForbidden, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			nova := &flavorAccessNova{actionCode: tc.code}
			srv := nova.serve(t)
			r := &flavorAccessResource{config: fakeConfig(srv.URL)}
			resp := runDelete(r, flavorAccessState(t))
			if got := resp.Diagnostics.HasError(); got != tc.wantErr {
				t.Fatalf("delete error = %v, want %v: %v", got, tc.wantErr, resp.Diagnostics)
			}
			posts := nova.actionBodies()
			if len(posts) != 1 {
				t.Fatalf("POST /flavors/f1/action sent %d times, want 1", len(posts))
			}
			revoke, _ := posts[0]["removeTenantAccess"].(map[string]any)
			if revoke["tenant"] != "p1" {
				t.Fatalf("action body = %v, want {\"removeTenantAccess\": {\"tenant\": \"p1\"}}", posts[0])
			}
		})
	}
}

func TestFlavorAccessImportState(t *testing.T) {
	ctx := context.Background()
	s := schemaOf(t, &flavorAccessResource{})
	resp := resource.ImportStateResponse{State: tfsdk.State{Schema: s, Raw: tftypes.NewValue(s.Type().TerraformType(ctx), nil)}}
	(&flavorAccessResource{}).ImportState(ctx, resource.ImportStateRequest{ID: "f1/p1"}, &resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("import f1/p1: %v", resp.Diagnostics)
	}
	var got flavorAccessModel
	if d := resp.State.Get(ctx, &got); d.HasError() {
		t.Fatal(d)
	}
	if got.ID.ValueString() != "f1/p1" || got.FlavorID.ValueString() != "f1" || got.TenantID.ValueString() != "p1" {
		t.Fatalf("imported id=%s flavor_id=%s tenant_id=%s, want f1/p1, f1, p1", got.ID, got.FlavorID, got.TenantID)
	}

	for _, bad := range []string{"f1", "/p1", "f1/", ""} {
		resp := resource.ImportStateResponse{State: tfsdk.State{Schema: s, Raw: tftypes.NewValue(s.Type().TerraformType(ctx), nil)}}
		(&flavorAccessResource{}).ImportState(ctx, resource.ImportStateRequest{ID: bad}, &resp)
		if !resp.Diagnostics.HasError() {
			t.Errorf("import %q accepted; want an error naming <flavor_id>/<tenant_id>", bad)
			continue
		}
		if !strings.Contains(resp.Diagnostics.Errors()[0].Detail(), "<flavor_id>/<tenant_id>") {
			t.Errorf("import %q error detail %q does not name the expected form", bad, resp.Diagnostics.Errors()[0].Detail())
		}
	}
}
