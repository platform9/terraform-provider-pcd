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

	"github.com/gophercloud/gophercloud/v2"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

// fakeOwnerNova serves one ACTIVE server srv-1 owned by owner and records
// every server action with its microversion header. changeOwner moves the
// owner unless refuse holds an error body to answer 403 with.
type fakeOwnerNova struct {
	t       *testing.T
	mu      sync.Mutex
	owner   string
	refuse  string
	actions []string
}

func newFakeOwnerNova(t *testing.T, owner string) (*fakeOwnerNova, *httptest.Server) {
	f := &fakeOwnerNova{t: t, owner: owner}
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)
	return f, srv
}

func (f *fakeOwnerNova) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	switch r.Method + " " + r.URL.Path {
	case "POST /servers":
		w.WriteHeader(http.StatusAccepted)
		fmt.Fprint(w, `{"server": {"id": "srv-1"}}`)
	case "GET /servers/srv-1":
		fmt.Fprintf(w, `{"server": {"id": "srv-1", "name": "vm-1", "status": "ACTIVE", "user_id": %q, "metadata": {}, "addresses": {}}}`, f.owner)
	case "POST /servers/srv-1/action":
		raw, _ := io.ReadAll(r.Body)
		f.actions = append(f.actions, string(raw)+" @"+r.Header.Get("OpenStack-API-Version"))
		if f.refuse != "" {
			w.WriteHeader(http.StatusForbidden)
			fmt.Fprint(w, f.refuse)
			return
		}
		var body struct {
			ChangeOwner struct {
				UserID string `json:"user_id"`
			} `json:"changeOwner"`
		}
		if err := json.Unmarshal(raw, &body); err != nil || body.ChangeOwner.UserID == "" {
			f.t.Errorf("unexpected server action %s", raw)
		}
		f.owner = body.ChangeOwner.UserID
		w.WriteHeader(http.StatusAccepted)
	default:
		f.t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusNotImplemented)
	}
}

func (f *fakeOwnerNova) actionLog() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.actions...)
}

// ownerComputeClient is a bare compute client for the fake; Type makes
// gophercloud send microversion headers the way the real client does, and
// the transport is its own, as fakeConfig gives its clients.
func ownerComputeClient(url string) *gophercloud.ServiceClient {
	return &gophercloud.ServiceClient{
		ProviderClient: &gophercloud.ProviderClient{HTTPClient: http.Client{Transport: &http.Transport{}}},
		Endpoint:       url + "/",
		Type:           "compute",
	}
}

func ownerTestResource(url string) *instanceResource {
	return &instanceResource{config: fakeConfig(url)}
}

func instanceSchema(ctx context.Context) resource.SchemaResponse {
	var sch resource.SchemaResponse
	(&instanceResource{}).Schema(ctx, resource.SchemaRequest{}, &sch)
	return sch
}

func stateUserID(t *testing.T, s tfsdk.State) string {
	t.Helper()
	var v types.String
	if d := s.GetAttribute(context.Background(), path.Root("user_id"), &v); d.HasError() {
		t.Fatal(d)
	}
	return v.ValueString()
}

func TestChangeInstanceOwner(t *testing.T) {
	ctx := context.Background()
	t.Run("sends changeOwner at the default microversion", func(t *testing.T) {
		f, srv := newFakeOwnerNova(t, "u1")
		if err := changeInstanceOwner(ctx, ownerComputeClient(srv.URL), "srv-1", "u2"); err != nil {
			t.Fatal(err)
		}
		if got := f.actionLog(); len(got) != 1 || got[0] != `{"changeOwner":{"user_id":"u2"}} @` {
			t.Fatalf("actions = %q; want one changeOwner with no microversion header, as the PCD UI sends", got)
		}
	})
	t.Run("does nothing for the current owner", func(t *testing.T) {
		f, srv := newFakeOwnerNova(t, "u2")
		if err := changeInstanceOwner(ctx, ownerComputeClient(srv.URL), "srv-1", "u2"); err != nil {
			t.Fatal(err)
		}
		if got := f.actionLog(); len(got) != 0 {
			t.Fatalf("actions = %q; Nova re-runs the quota check for a same-owner call, so none may be sent", got)
		}
	})
	t.Run("returns Nova's refusal", func(t *testing.T) {
		f, srv := newFakeOwnerNova(t, "u1")
		f.refuse = `{"forbidden": {"code": 403, "message": "User u2 is not eligible to own instances in project p1"}}`
		err := changeInstanceOwner(ctx, ownerComputeClient(srv.URL), "srv-1", "u2")
		if err == nil || !strings.Contains(err.Error(), "is not eligible to own instances") {
			t.Fatalf("err = %v", err)
		}
	})
}

// Create boots as the caller, then hands the instance to the configured owner.
func TestInstanceCreateChangesOwner(t *testing.T) {
	ctx := context.Background()
	f, srv := newFakeOwnerNova(t, "admin")
	r := ownerTestResource(srv.URL)
	sch := instanceSchema(ctx)
	plan := tfsdk.Plan{Schema: sch.Schema, Raw: instanceValue(t, map[string]any{
		"name": "vm-1", "image_id": "img-1", "flavor_id": "flv-1", "user_id": "u2"})}
	resp := resource.CreateResponse{State: tfsdk.State{Schema: sch.Schema, Raw: tftypes.NewValue(sch.Schema.Type().TerraformType(ctx), nil)}}
	r.Create(ctx, resource.CreateRequest{Plan: plan}, &resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("create: %v", resp.Diagnostics)
	}
	if got := stateUserID(t, resp.State); got != "u2" {
		t.Fatalf("user_id = %s, want u2", got)
	}
	if got := f.actionLog(); len(got) != 1 {
		t.Fatalf("actions = %q; want one changeOwner", got)
	}
}

// A refused owner change fails Create with the instance in state, owned by
// whoever really owns it, so the plan after the taint is accurate.
func TestInstanceCreateOwnerRefusedKeepsRealOwner(t *testing.T) {
	ctx := context.Background()
	f, srv := newFakeOwnerNova(t, "admin")
	f.refuse = `{"forbidden": {"code": 403, "message": "Quota exceeded for instances"}}`
	r := ownerTestResource(srv.URL)
	sch := instanceSchema(ctx)
	plan := tfsdk.Plan{Schema: sch.Schema, Raw: instanceValue(t, map[string]any{
		"name": "vm-1", "image_id": "img-1", "flavor_id": "flv-1", "user_id": "u2"})}
	resp := resource.CreateResponse{State: tfsdk.State{Schema: sch.Schema, Raw: tftypes.NewValue(sch.Schema.Type().TerraformType(ctx), nil)}}
	r.Create(ctx, resource.CreateRequest{Plan: plan}, &resp)
	if !resp.Diagnostics.HasError() || !strings.Contains(resp.Diagnostics[0].Detail(), "Quota exceeded") {
		t.Fatalf("diags = %v", resp.Diagnostics)
	}
	if resp.State.Raw.IsNull() || !resp.State.Raw.IsFullyKnown() {
		t.Fatalf("state = %v; want srv-1 recorded with no unknown values, or Terraform loses it", resp.State.Raw)
	}
	if got := stateUserID(t, resp.State); got != "admin" {
		t.Fatalf("user_id in state = %s; want admin, the owner Nova still has", got)
	}
}

// Update changes the owner in place when the configured owner differs from
// the one Nova reports, whatever state says, and leaves an
// omitted owner alone.
func TestInstanceUpdateOwner(t *testing.T) {
	ctx := context.Background()
	sch := instanceSchema(ctx)
	base := map[string]any{"id": "srv-1", "name": "vm-1", "flavor_id": "flv-1", "user_id": "u1"}
	with := func(owner any) map[string]any {
		m := map[string]any{}
		for bk, bv := range base {
			m[bk] = bv
		}
		m["user_id"] = owner
		return m
	}
	for _, tc := range []struct {
		name        string
		live        string
		planned     map[string]any
		wantActions int
		wantOwner   string
	}{
		{"changed", "u1", with("u2"), 1, "u2"},
		{"unchanged", "u1", base, 0, "u1"},
		// A saved plan, or -refresh=false: state still says u1, Nova says u3.
		{"plan equals state but live differs", "u3", base, 1, "u1"},
		// Omitted: the plan of another change carries user_id as unknown.
		{"omitted", "u3", with(types.StringUnknown()), 0, "u3"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, srv := newFakeOwnerNova(t, tc.live)
			r := ownerTestResource(srv.URL)
			state := tfsdk.State{Schema: sch.Schema, Raw: instanceValue(t, base)}
			plan := tfsdk.Plan{Schema: sch.Schema, Raw: instanceValue(t, tc.planned)}
			resp := resource.UpdateResponse{State: state}
			r.Update(ctx, resource.UpdateRequest{Plan: plan, State: state}, &resp)
			if resp.Diagnostics.HasError() {
				t.Fatalf("update: %v", resp.Diagnostics)
			}
			if got := f.actionLog(); len(got) != tc.wantActions {
				t.Fatalf("actions = %q, want %d", got, tc.wantActions)
			}
			if got := stateUserID(t, resp.State); got != tc.wantOwner {
				t.Fatalf("user_id = %s, want %s", got, tc.wantOwner)
			}
		})
	}
}

// user_id has no plan modifier: the owner can change outside
// Terraform, and a value carried over from state would fail the apply with
// "Provider produced inconsistent result" after such a change.
func TestInstanceUserIDSchema(t *testing.T) {
	sch := instanceSchema(context.Background())
	a, ok := sch.Schema.Attributes["user_id"].(schema.StringAttribute)
	if !ok {
		t.Fatalf("user_id is %T, want a string attribute", sch.Schema.Attributes["user_id"])
	}
	if !a.Optional || !a.Computed || a.Default != nil || len(a.PlanModifiers) != 0 {
		t.Fatalf("user_id: optional=%v computed=%v default=%v plan modifiers=%d; want Optional and Computed with no default and no plan modifier",
			a.Optional, a.Computed, a.Default, len(a.PlanModifiers))
	}
}
