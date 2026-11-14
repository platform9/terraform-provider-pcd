// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package mors

import (
	"context"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

func instanceLeaseSchema(t *testing.T) schema.Schema {
	t.Helper()
	var sch resource.SchemaResponse
	(&instanceLeaseResource{}).Schema(context.Background(), resource.SchemaRequest{}, &sch)
	if sch.Diagnostics.HasError() {
		t.Fatalf("schema: %v", sch.Diagnostics)
	}
	return sch.Schema
}

// novaServer is Nova's answer for vm-1, owned by project.
func novaServer(project string) string {
	return fmt.Sprintf(`{"server": {"id": "vm-1", "name": "vm-1", "status": "ACTIVE", "tenant_id": %q}}`, project)
}

func policyOfMinutes(minutes int) string {
	return fmt.Sprintf(`{"vm_lease_policy": {"tenant_uuid": "proj-1", "expiry_mins": %d, "action": "power off"}}`, minutes)
}

// expiryIn returns a lease expiry d from now, in mors's format.
func expiryIn(d time.Duration) string {
	return time.Now().UTC().Add(d).Truncate(time.Second).Format(leaseExpiryLayout)
}

func leaseBody(project, expiry, action string) string {
	return fmt.Sprintf(`{"instance_uuid": "vm-1", "tenant_uuid": %q, "expiry": %q, "action": %q,
		"created_at": "2026-10-01T00:00:00Z", "created_by": "u1", "updated_at": null, "updated_by": null}`, project, expiry, action)
}

func plannedLease(project types.String, expiry, action string) instanceLeaseModel {
	return instanceLeaseModel{
		ID:         types.StringUnknown(),
		InstanceID: types.StringValue("vm-1"),
		ProjectID:  project,
		Expiry:     types.StringValue(expiry),
		Action:     types.StringValue(action),
	}
}

// mors writes exactly the text it returns; anything else would either be
// refused (422) or come back in another form and plan a change forever.
func TestInvalidLeaseExpiry(t *testing.T) {
	for in, wantErr := range map[string]bool{
		"2026-12-31T23:59:59Z":      false,
		"2027-02-28T00:00:00Z":      false,
		"2026-12-31T23:59:59":       true, // no zone
		"2026-12-31T23:59:59+00:00": true, // offset instead of Z
		"2026-12-31T23:59:59.5Z":    true, // fractional seconds
		"2026-12-31 23:59:59Z":      true, // space separator
		"2026-12-31T1:00:00Z":       true, // one-digit hour, which mors would echo as 01
		"2026-13-01T00:00:00Z":      true, // month 13
		"2027-02-29T00:00:00Z":      true, // not a leap year
		"":                          true,
	} {
		if got := invalidLeaseExpiry(in) != ""; got != wantErr {
			t.Errorf("invalidLeaseExpiry(%q) error = %v, want %v", in, got, wantErr)
		}
	}
}

// mors accepts a create with any future expiry but refuses every later update
// whose expiry is more than one policy period away, even an update of action
// alone. Create refuses such an expiry up front, as the PCD UI does.
func TestLeaseWithinPolicy(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		expiry  string
		minutes int64
		wantErr bool
	}{
		{"2026-10-06T13:00:00Z", 60, false},   // exactly one period
		{"2026-10-06T12:30:00Z", 60, false},   // inside it
		{"2026-10-06T13:00:01Z", 60, true},    // one second over
		{"2026-10-09T12:00:00Z", 4320, false}, // three days
		{"2026-10-09T12:00:01Z", 4320, true},
	} {
		if err := leaseWithinPolicy(tc.expiry, now, tc.minutes); (err != nil) != tc.wantErr {
			t.Errorf("leaseWithinPolicy(%s, %d min) = %v, wantErr %v", tc.expiry, tc.minutes, err, tc.wantErr)
		}
	}
}

// mors updates the VM named by the body's instance_uuid, not the one in the
// URL; both must name the same VM.
func TestInstanceLeaseBodyNamesTheURLInstance(t *testing.T) {
	f := newFakeMors(t, map[string][]fakeReply{"PUT /v1/tenant/proj-1/instance/vm-1": {{http.StatusOK, `{"success": true}`}}})
	if err := writeInstanceLease(context.Background(), f.client(), http.MethodPut, "proj-1", "vm-1", "2026-12-31T23:59:59Z", "delete"); err != nil {
		t.Fatalf("writeInstanceLease: %v", err)
	}
	want := map[string]any{"instance_uuid": "vm-1", "expiry": "2026-12-31T23:59:59Z", "action": "delete"}
	if got := f.bodyOf("PUT /v1/tenant/proj-1/instance/vm-1"); !reflect.DeepEqual(got, want) {
		t.Fatalf("body = %v\nwant  %v", got, want)
	}
}

func TestInstanceLeaseLifecycle(t *testing.T) {
	ctx := context.Background()
	first, second := expiryIn(24*time.Hour), expiryIn(48*time.Hour)
	f := newFakeMors(t, map[string][]fakeReply{
		"GET /servers/vm-1":                    {{http.StatusOK, novaServer("proj-1")}},
		"GET /v1/tenant/proj-1":                {{http.StatusOK, policyOfMinutes(525600)}},
		"POST /v1/tenant/proj-1/instance/vm-1": {{http.StatusOK, `{"success": true}`}},
		"GET /v1/tenant/proj-1/instance/vm-1": {
			{http.StatusOK, leaseBody("proj-1", first, "power off")},
			{http.StatusOK, leaseBody("proj-1", second, "power off")},
		},
		"PUT /v1/tenant/proj-1/instance/vm-1":    {{http.StatusOK, `{"success": true}`}},
		"DELETE /v1/tenant/proj-1/instance/vm-1": {{http.StatusOK, `{"success": true}`}},
	})
	r := &instanceLeaseResource{config: f.config()}
	s := instanceLeaseSchema(t)

	// project_id omitted: Create reads it from the VM.
	planned := plannedLease(types.StringUnknown(), first, "power off")
	createResp := resource.CreateResponse{State: tfsdk.State{Schema: s, Raw: nullValue(ctx, s)}}
	r.Create(ctx, resource.CreateRequest{Plan: planOf(ctx, t, s, &planned)}, &createResp)
	if createResp.Diagnostics.HasError() {
		t.Fatalf("create: %v", createResp.Diagnostics)
	}
	var created instanceLeaseModel
	createResp.State.Get(ctx, &created)
	if created.ProjectID.ValueString() != "proj-1" || created.ID.ValueString() != "proj-1/vm-1" {
		t.Fatalf("project_id=%s id=%s, want proj-1 and proj-1/vm-1", created.ProjectID, created.ID)
	}
	if body := f.bodyOf("POST /v1/tenant/proj-1/instance/vm-1"); body["instance_uuid"] != "vm-1" || body["expiry"] != first {
		t.Fatalf("POST body = %v", body)
	}

	readResp := resource.ReadResponse{State: createResp.State}
	r.Read(ctx, resource.ReadRequest{State: createResp.State}, &readResp)
	var read instanceLeaseModel
	readResp.State.Get(ctx, &read)
	if readResp.Diagnostics.HasError() || read.Expiry.ValueString() != first {
		t.Fatalf("read: diags %v, expiry %s; want %s unchanged", readResp.Diagnostics, read.Expiry, first)
	}

	updated := read
	updated.Expiry = types.StringValue(second)
	updateResp := resource.UpdateResponse{State: readResp.State}
	r.Update(ctx, resource.UpdateRequest{Plan: planOf(ctx, t, s, &updated), State: readResp.State}, &updateResp)
	if updateResp.Diagnostics.HasError() {
		t.Fatalf("update: %v", updateResp.Diagnostics)
	}

	deleteResp := resource.DeleteResponse{State: updateResp.State}
	r.Delete(ctx, resource.DeleteRequest{State: updateResp.State}, &deleteResp)
	if deleteResp.Diagnostics.HasError() {
		t.Fatalf("delete: %v", deleteResp.Diagnostics)
	}

	want := []string{
		"GET /servers/vm-1", "GET /v1/tenant/proj-1", "POST /v1/tenant/proj-1/instance/vm-1",
		"GET /v1/tenant/proj-1/instance/vm-1", "PUT /v1/tenant/proj-1/instance/vm-1",
		"GET /v1/tenant/proj-1/instance/vm-1", "DELETE /v1/tenant/proj-1/instance/vm-1",
	}
	if got := f.keys(); !reflect.DeepEqual(got, want) {
		t.Fatalf("requests = %v\nwant       %v", got, want)
	}
}

// mors files a lease under whatever project the URL names, and its scheduler
// then acts on the VM as the mors service user. The provider must refuse a
// project the VM does not belong to before mors sees the request.
func TestInstanceLeaseCreateRefusesAnotherProjectsVM(t *testing.T) {
	ctx := context.Background()
	f := newFakeMors(t, map[string][]fakeReply{"GET /servers/vm-1": {{http.StatusOK, novaServer("proj-1")}}})
	r := &instanceLeaseResource{config: f.config()}
	s := instanceLeaseSchema(t)
	planned := plannedLease(types.StringValue("proj-2"), expiryIn(time.Hour), "power off")
	resp := resource.CreateResponse{State: tfsdk.State{Schema: s, Raw: nullValue(ctx, s)}}
	r.Create(ctx, resource.CreateRequest{Plan: planOf(ctx, t, s, &planned)}, &resp)
	if !resp.Diagnostics.HasError() || !strings.Contains(resp.Diagnostics[0].Detail(), "belongs to project proj-1, not proj-2") ||
		!strings.HasSuffix(resp.Diagnostics[0].Detail(), "Set project_id to the VM's project, or omit it.") {
		t.Fatalf("diagnostics = %v, want the project mismatch refused with what to do", resp.Diagnostics)
	}
	if got := f.keys(); !reflect.DeepEqual(got, []string{"GET /servers/vm-1"}) {
		t.Fatalf("requests = %v; mors must not be called", got)
	}
}

// Without a project policy mors never looks at a VM's lease, so a lease
// created there would silently do nothing. The PCD UI hides Customize Lease
// in that case; Create refuses it.
func TestInstanceLeaseCreateNeedsAPolicy(t *testing.T) {
	ctx := context.Background()
	f := newFakeMors(t, map[string][]fakeReply{
		"GET /servers/vm-1":     {{http.StatusOK, novaServer("proj-1")}},
		"GET /v1/tenant/proj-1": {{http.StatusNotFound, `{"success": false}`}},
	})
	r := &instanceLeaseResource{config: f.config()}
	s := instanceLeaseSchema(t)
	planned := plannedLease(types.StringValue("proj-1"), expiryIn(time.Hour), "power off")
	resp := resource.CreateResponse{State: tfsdk.State{Schema: s, Raw: nullValue(ctx, s)}}
	r.Create(ctx, resource.CreateRequest{Plan: planOf(ctx, t, s, &planned)}, &resp)
	if !resp.Diagnostics.HasError() || !strings.Contains(resp.Diagnostics[0].Detail(), "has no lease policy") {
		t.Fatalf("diagnostics = %v, want the missing policy reported", resp.Diagnostics)
	}
	for _, k := range f.keys() {
		if strings.HasPrefix(k, "POST") {
			t.Fatalf("a lease was written although the project has no policy: %v", f.keys())
		}
	}
}

func TestInstanceLeaseCreateRefusesAnExpiryBeyondThePolicy(t *testing.T) {
	ctx := context.Background()
	f := newFakeMors(t, map[string][]fakeReply{
		"GET /servers/vm-1":     {{http.StatusOK, novaServer("proj-1")}},
		"GET /v1/tenant/proj-1": {{http.StatusOK, policyOfMinutes(60)}},
	})
	r := &instanceLeaseResource{config: f.config()}
	s := instanceLeaseSchema(t)
	planned := plannedLease(types.StringUnknown(), expiryIn(2*time.Hour), "power off")
	resp := resource.CreateResponse{State: tfsdk.State{Schema: s, Raw: nullValue(ctx, s)}}
	r.Create(ctx, resource.CreateRequest{Plan: planOf(ctx, t, s, &planned)}, &resp)
	if !resp.Diagnostics.HasError() {
		t.Fatal("an expiry two hours out was accepted under a 60-minute policy; every later update of it would fail")
	}
	for _, k := range f.keys() {
		if strings.HasPrefix(k, "POST") {
			t.Fatalf("the lease was written: %v", f.keys())
		}
	}
}

// An update through another project's URL moves a lease (mors rewrites its
// tenant_uuid). Read must report the lease's own project so the move shows up
// as a change.
func TestInstanceLeaseReadFollowsTheLeasesProject(t *testing.T) {
	ctx := context.Background()
	expiry := expiryIn(time.Hour)
	f := newFakeMors(t, map[string][]fakeReply{
		"GET /v1/tenant/proj-1/instance/vm-1": {{http.StatusOK, leaseBody("proj-2", expiry, "delete")}},
	})
	r := &instanceLeaseResource{config: f.config()}
	s := instanceLeaseSchema(t)
	prior := instanceLeaseModel{ID: types.StringValue("proj-1/vm-1"), InstanceID: types.StringValue("vm-1"),
		ProjectID: types.StringValue("proj-1"), Expiry: types.StringValue(expiry), Action: types.StringValue("power off")}
	resp := resource.ReadResponse{State: stateOf(ctx, t, s, &prior)}
	r.Read(ctx, resource.ReadRequest{State: stateOf(ctx, t, s, &prior)}, &resp)
	var got instanceLeaseModel
	resp.State.Get(ctx, &got)
	if got.ProjectID.ValueString() != "proj-2" || got.ID.ValueString() != "proj-2/vm-1" || got.Action.ValueString() != "delete" {
		t.Fatalf("read project_id=%s id=%s action=%s, want proj-2, proj-2/vm-1, delete", got.ProjectID, got.ID, got.Action)
	}
}

func TestInstanceLeaseReadDropsALeaseThatIsGone(t *testing.T) {
	ctx := context.Background()
	f := newFakeMors(t, map[string][]fakeReply{
		"GET /v1/tenant/proj-1/instance/vm-1": {{http.StatusNotFound, `{"error": "Not found"}`}},
	})
	r := &instanceLeaseResource{config: f.config()}
	s := instanceLeaseSchema(t)
	prior := instanceLeaseModel{ID: types.StringValue("proj-1/vm-1"), InstanceID: types.StringValue("vm-1"),
		ProjectID: types.StringValue("proj-1"), Expiry: types.StringValue(expiryIn(time.Hour)), Action: types.StringValue("power off")}
	resp := resource.ReadResponse{State: stateOf(ctx, t, s, &prior)}
	r.Read(ctx, resource.ReadRequest{State: stateOf(ctx, t, s, &prior)}, &resp)
	if resp.Diagnostics.HasError() || !resp.State.Raw.IsNull() {
		t.Fatalf("diags %v, state null %v; want the lease removed from state", resp.Diagnostics, resp.State.Raw.IsNull())
	}
}

// A 200 that holds no lease must fail the read, not save expiry "" and
// action "" over the state, which would plan a change forever.
func TestInstanceLeaseReadOfAnAnswerWithoutALease(t *testing.T) {
	ctx := context.Background()
	expiry := expiryIn(time.Hour)
	f := newFakeMors(t, map[string][]fakeReply{
		"GET /v1/tenant/proj-1/instance/vm-1": {{http.StatusOK, `{}`}},
	})
	r := &instanceLeaseResource{config: f.config()}
	s := instanceLeaseSchema(t)
	prior := instanceLeaseModel{ID: types.StringValue("proj-1/vm-1"), InstanceID: types.StringValue("vm-1"),
		ProjectID: types.StringValue("proj-1"), Expiry: types.StringValue(expiry), Action: types.StringValue("power off")}
	resp := resource.ReadResponse{State: stateOf(ctx, t, s, &prior)}
	r.Read(ctx, resource.ReadRequest{State: stateOf(ctx, t, s, &prior)}, &resp)
	if !resp.Diagnostics.HasError() {
		t.Fatal("read accepted an answer without a lease")
	}
	var kept instanceLeaseModel
	resp.State.Get(ctx, &kept)
	if kept.Expiry.ValueString() != expiry || kept.Action.ValueString() != "power off" {
		t.Fatalf("state = %+v; want the prior state kept", kept)
	}
}

// Nova's Extract never returns nil, so a 200 without the server would give
// project "" and file the lease under /v1/tenant//. Create must refuse it
// before mors sees a request.
func TestInstanceLeaseCreateRefusesAnAnswerWithoutTheServer(t *testing.T) {
	ctx := context.Background()
	f := newFakeMors(t, map[string][]fakeReply{"GET /servers/vm-1": {{http.StatusOK, `{}`}}})
	r := &instanceLeaseResource{config: f.config()}
	s := instanceLeaseSchema(t)
	planned := plannedLease(types.StringUnknown(), expiryIn(time.Hour), "power off")
	resp := resource.CreateResponse{State: tfsdk.State{Schema: s, Raw: nullValue(ctx, s)}}
	r.Create(ctx, resource.CreateRequest{Plan: planOf(ctx, t, s, &planned)}, &resp)
	if !resp.Diagnostics.HasError() || !resp.State.Raw.IsNull() || resp.Diagnostics[0].Summary() != "mors: reading the instance" {
		t.Fatalf("create: diags %v, state null %v; want the instance read refused and no state", resp.Diagnostics, resp.State.Raw.IsNull())
	}
	if got := f.keys(); !reflect.DeepEqual(got, []string{"GET /servers/vm-1"}) {
		t.Fatalf("requests = %v; mors must not be called", got)
	}
}

// An instance_id unknown at plan skips the schema's length check and can reach
// apply as "". /servers/ is Nova's list route, so an empty instance ID must be
// refused before any request (the fake fails the test on any).
func TestInstanceLeaseRefusesAnEmptyInstanceID(t *testing.T) {
	ctx := context.Background()
	f := newFakeMors(t, map[string][]fakeReply{})
	r := &instanceLeaseResource{config: f.config()}
	s := instanceLeaseSchema(t)
	planned := plannedLease(types.StringUnknown(), expiryIn(time.Hour), "power off")
	planned.InstanceID = types.StringValue("")
	resp := resource.CreateResponse{State: tfsdk.State{Schema: s, Raw: nullValue(ctx, s)}}
	r.Create(ctx, resource.CreateRequest{Plan: planOf(ctx, t, s, &planned)}, &resp)
	if !resp.Diagnostics.HasError() || !resp.State.Raw.IsNull() {
		t.Fatalf("create: diags %v, state null %v; want an error and no state", resp.Diagnostics, resp.State.Raw.IsNull())
	}
	// Delete of a row with an empty part has nothing to delete either.
	row := instanceLeaseModel{ID: types.StringValue("proj-1/"), InstanceID: types.StringValue(""),
		ProjectID: types.StringValue("proj-1"), Expiry: types.StringValue(expiryIn(time.Hour)), Action: types.StringValue("power off")}
	deleteResp := resource.DeleteResponse{State: stateOf(ctx, t, s, &row)}
	r.Delete(ctx, resource.DeleteRequest{State: stateOf(ctx, t, s, &row)}, &deleteResp)
	if deleteResp.Diagnostics.HasError() {
		t.Fatalf("delete: %v", deleteResp.Diagnostics)
	}
	if got := f.keys(); len(got) != 0 {
		t.Fatalf("requests = %v; an empty instance ID must not reach Nova or mors", got)
	}
}

func TestInstanceLeaseUpdateOfALeaseRemovedOutsideTerraform(t *testing.T) {
	ctx := context.Background()
	f := newFakeMors(t, map[string][]fakeReply{
		"PUT /v1/tenant/proj-1/instance/vm-1": {{http.StatusOK, `{"success": true}`}},
		"GET /v1/tenant/proj-1/instance/vm-1": {{http.StatusNotFound, `{"error": "Not found"}`}},
	})
	r := &instanceLeaseResource{config: f.config()}
	s := instanceLeaseSchema(t)
	prior := instanceLeaseModel{ID: types.StringValue("proj-1/vm-1"), InstanceID: types.StringValue("vm-1"),
		ProjectID: types.StringValue("proj-1"), Expiry: types.StringValue(expiryIn(time.Hour)), Action: types.StringValue("power off")}
	planned := prior
	planned.Expiry = types.StringValue(expiryIn(2 * time.Hour))
	resp := resource.UpdateResponse{State: stateOf(ctx, t, s, &prior)}
	r.Update(ctx, resource.UpdateRequest{Plan: planOf(ctx, t, s, &planned), State: stateOf(ctx, t, s, &prior)}, &resp)
	if !resp.Diagnostics.HasError() {
		t.Fatal("update reported success over a lease mors does not hold")
	}
}

func TestInstanceLeaseImportState(t *testing.T) {
	ctx := context.Background()
	s := instanceLeaseSchema(t)
	resp := resource.ImportStateResponse{State: tfsdk.State{Schema: s, Raw: nullValue(ctx, s)}}
	(&instanceLeaseResource{}).ImportState(ctx, resource.ImportStateRequest{ID: "proj-1/vm-1"}, &resp)
	var got instanceLeaseModel
	resp.State.Get(ctx, &got)
	if resp.Diagnostics.HasError() || got.ProjectID.ValueString() != "proj-1" || got.InstanceID.ValueString() != "vm-1" || got.ID.ValueString() != "proj-1/vm-1" {
		t.Fatalf("import: diags %v, project_id %s, instance_id %s, id %s", resp.Diagnostics, got.ProjectID, got.InstanceID, got.ID)
	}
	for _, id := range []string{"", "vm-1", "/vm-1", "proj-1/"} {
		bad := resource.ImportStateResponse{State: tfsdk.State{Schema: s, Raw: nullValue(ctx, s)}}
		(&instanceLeaseResource{}).ImportState(ctx, resource.ImportStateRequest{ID: id}, &bad)
		if !bad.Diagnostics.HasError() || !bad.State.Raw.IsNull() {
			t.Errorf("import id %q: diags %v, state null %v; want <project_id>/<instance_id> enforced and no state",
				id, bad.Diagnostics, bad.State.Raw.IsNull())
		}
	}
}

// The schema must carry both validators: mors stores any action string and
// answers a malformed expiry only at apply time.
func TestInstanceLeaseSchemaValidators(t *testing.T) {
	ctx := context.Background()
	s := instanceLeaseSchema(t)
	expiry := s.Attributes["expiry"].(schema.StringAttribute)
	if !stringValidatorErrors(ctx, expiry, types.StringValue("2026-12-31T23:59:59")) {
		t.Error("expiry without a zone passed validation")
	}
	if stringValidatorErrors(ctx, expiry, types.StringValue("2026-12-31T23:59:59Z")) {
		t.Error("a well-formed expiry failed validation")
	}
	if stringValidatorErrors(ctx, expiry, types.StringUnknown()) {
		t.Error("an unknown expiry failed validation; it is known only at apply")
	}
	if !stringValidatorErrors(ctx, s.Attributes["action"].(schema.StringAttribute), types.StringValue("shutdown")) {
		t.Error("action shutdown passed validation")
	}
}

// A lease changed through the PCD UI's Customize Lease must show as drift:
// Read stores what mors holds, not the prior state.
func TestInstanceLeaseReadReportsDrift(t *testing.T) {
	ctx := context.Background()
	prior, changed := expiryIn(time.Hour), expiryIn(3*time.Hour)
	f := newFakeMors(t, map[string][]fakeReply{
		"GET /v1/tenant/proj-1/instance/vm-1": {{http.StatusOK, leaseBody("proj-1", changed, "delete")}},
	})
	r := &instanceLeaseResource{config: f.config()}
	s := instanceLeaseSchema(t)
	row := instanceLeaseModel{ID: types.StringValue("proj-1/vm-1"), InstanceID: types.StringValue("vm-1"),
		ProjectID: types.StringValue("proj-1"), Expiry: types.StringValue(prior), Action: types.StringValue("power off")}
	resp := resource.ReadResponse{State: stateOf(ctx, t, s, &row)}
	r.Read(ctx, resource.ReadRequest{State: stateOf(ctx, t, s, &row)}, &resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("read: %v", resp.Diagnostics)
	}
	var got instanceLeaseModel
	resp.State.Get(ctx, &got)
	if got.Expiry.ValueString() != changed || got.Action.ValueString() != "delete" {
		t.Fatalf("read expiry=%s action=%s, want mors's %s and delete", got.Expiry, got.Action, changed)
	}
}

// A 404 on DELETE means there is nothing left to delete: destroy must succeed.
func TestInstanceLeaseDeleteOfALeaseAlreadyGone(t *testing.T) {
	ctx := context.Background()
	f := newFakeMors(t, map[string][]fakeReply{
		"DELETE /v1/tenant/proj-1/instance/vm-1": {{http.StatusNotFound, `{"error": "Not found"}`}},
	})
	r := &instanceLeaseResource{config: f.config()}
	s := instanceLeaseSchema(t)
	row := instanceLeaseModel{ID: types.StringValue("proj-1/vm-1"), InstanceID: types.StringValue("vm-1"),
		ProjectID: types.StringValue("proj-1"), Expiry: types.StringValue(expiryIn(time.Hour)), Action: types.StringValue("power off")}
	resp := resource.DeleteResponse{State: stateOf(ctx, t, s, &row)}
	r.Delete(ctx, resource.DeleteRequest{State: stateOf(ctx, t, s, &row)}, &resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("delete of a lease already gone: %v", resp.Diagnostics)
	}
	if got := f.keys(); !reflect.DeepEqual(got, []string{"DELETE /v1/tenant/proj-1/instance/vm-1"}) {
		t.Fatalf("requests = %v", got)
	}
}

// A VM that already has a lease (set in the PCD UI, or by an apply whose
// answer was lost) makes mors answer 409. The user needs the import command.
func TestInstanceLeaseCreateNamesTheImport(t *testing.T) {
	ctx := context.Background()
	f := newFakeMors(t, map[string][]fakeReply{
		"GET /servers/vm-1":                    {{http.StatusOK, novaServer("proj-1")}},
		"GET /v1/tenant/proj-1":                {{http.StatusOK, policyOfMinutes(525600)}},
		"POST /v1/tenant/proj-1/instance/vm-1": {{http.StatusConflict, `{"error": "Already exists"}`}},
	})
	r := &instanceLeaseResource{config: f.config()}
	s := instanceLeaseSchema(t)
	planned := plannedLease(types.StringValue("proj-1"), expiryIn(time.Hour), "power off")
	resp := resource.CreateResponse{State: tfsdk.State{Schema: s, Raw: nullValue(ctx, s)}}
	r.Create(ctx, resource.CreateRequest{Plan: planOf(ctx, t, s, &planned)}, &resp)
	if !resp.Diagnostics.HasError() || !strings.Contains(resp.Diagnostics[0].Detail(), "terraform import") ||
		!strings.Contains(resp.Diagnostics[0].Detail(), "proj-1/vm-1") {
		t.Fatalf("diagnostics = %v, want an error that says to import proj-1/vm-1", resp.Diagnostics)
	}
	if !resp.State.Raw.IsNull() {
		t.Fatal("a refused create left state behind")
	}
}

// diagPath returns the attribute path of d, or "" when it has none.
func diagPath(d diag.Diagnostic) string {
	if wp, ok := d.(diag.DiagnosticWithPath); ok {
		return wp.Path().String()
	}
	return ""
}

// Nova's 404 means the VM does not exist or is not visible: the error belongs
// on instance_id, where the user fixes it.
func TestInstanceLeaseCreateOfAnInstanceNovaDoesNotHave(t *testing.T) {
	ctx := context.Background()
	f := newFakeMors(t, map[string][]fakeReply{
		"GET /servers/vm-1": {{http.StatusNotFound, `{"itemNotFound": {"code": 404, "message": "Instance vm-1 could not be found."}}`}},
	})
	r := &instanceLeaseResource{config: f.config()}
	s := instanceLeaseSchema(t)
	planned := plannedLease(types.StringUnknown(), expiryIn(time.Hour), "power off")
	resp := resource.CreateResponse{State: tfsdk.State{Schema: s, Raw: nullValue(ctx, s)}}
	r.Create(ctx, resource.CreateRequest{Plan: planOf(ctx, t, s, &planned)}, &resp)
	if !resp.Diagnostics.HasError() || diagPath(resp.Diagnostics[0]) != "instance_id" {
		t.Fatalf("diagnostics = %v, want an error on instance_id", resp.Diagnostics)
	}
	if got := f.keys(); !reflect.DeepEqual(got, []string{"GET /servers/vm-1"}) {
		t.Fatalf("requests = %v; mors must not be called", got)
	}
}

// A Nova answer for another server proves nothing about instance_id's owner.
func TestInstanceLeaseCreateRefusesAnAnswerForAnotherServer(t *testing.T) {
	ctx := context.Background()
	f := newFakeMors(t, map[string][]fakeReply{
		"GET /servers/vm-1": {{http.StatusOK, `{"server": {"id": "vm-2", "status": "ACTIVE", "tenant_id": "proj-1"}}`}},
	})
	r := &instanceLeaseResource{config: f.config()}
	s := instanceLeaseSchema(t)
	planned := plannedLease(types.StringUnknown(), expiryIn(time.Hour), "power off")
	resp := resource.CreateResponse{State: tfsdk.State{Schema: s, Raw: nullValue(ctx, s)}}
	r.Create(ctx, resource.CreateRequest{Plan: planOf(ctx, t, s, &planned)}, &resp)
	if !resp.Diagnostics.HasError() || !resp.State.Raw.IsNull() || diagPath(resp.Diagnostics[0]) != "instance_id" ||
		!strings.HasSuffix(resp.Diagnostics[0].Detail(), "Set instance_id to the VM's ID as Nova returns it (vm-2).") {
		t.Fatalf("create: diags %v, state null %v; want an error on instance_id naming Nova's ID, and no state",
			resp.Diagnostics, resp.State.Raw.IsNull())
	}
	if got := f.keys(); !reflect.DeepEqual(got, []string{"GET /servers/vm-1"}) {
		t.Fatalf("requests = %v; mors must not be called", got)
	}
}

// Update's read-back is what proves a PUT landed. A 200 that holds no lease
// proves nothing: it must fail the update and keep the prior state.
func TestInstanceLeaseUpdateReadBackWithoutALease(t *testing.T) {
	ctx := context.Background()
	f := newFakeMors(t, map[string][]fakeReply{
		"PUT /v1/tenant/proj-1/instance/vm-1": {{http.StatusOK, `{"success": true}`}},
		"GET /v1/tenant/proj-1/instance/vm-1": {{http.StatusOK, `{}`}},
	})
	r := &instanceLeaseResource{config: f.config()}
	s := instanceLeaseSchema(t)
	first := expiryIn(time.Hour)
	prior := instanceLeaseModel{ID: types.StringValue("proj-1/vm-1"), InstanceID: types.StringValue("vm-1"),
		ProjectID: types.StringValue("proj-1"), Expiry: types.StringValue(first), Action: types.StringValue("power off")}
	planned := prior
	planned.Expiry = types.StringValue(expiryIn(2 * time.Hour))
	resp := resource.UpdateResponse{State: stateOf(ctx, t, s, &prior)}
	r.Update(ctx, resource.UpdateRequest{Plan: planOf(ctx, t, s, &planned), State: stateOf(ctx, t, s, &prior)}, &resp)
	if !resp.Diagnostics.HasError() {
		t.Fatal("update accepted a read-back without a lease")
	}
	var kept instanceLeaseModel
	resp.State.Get(ctx, &kept)
	if kept.Expiry.ValueString() != first {
		t.Fatalf("state expiry = %s; want the prior %s kept", kept.Expiry, first)
	}
}

// Every request is refused for a malformed project_id or instance_id, and
// none reaches Nova or mors (the fake fails the test on any). Delete has
// nothing it could delete.
func TestInstanceLeaseRefusesMalformedIDs(t *testing.T) {
	ctx := context.Background()
	s := instanceLeaseSchema(t)
	type ids struct{ project, instance string }
	var cases []ids
	for _, bad := range badIDs {
		cases = append(cases, ids{bad, "vm-1"}, ids{"proj-1", bad})
	}
	for _, tc := range cases {
		t.Run(tc.project+"|"+tc.instance, func(t *testing.T) {
			f := newFakeMors(t, map[string][]fakeReply{})
			r := &instanceLeaseResource{config: f.config()}
			row := instanceLeaseModel{ID: types.StringValue(tc.project + "/" + tc.instance), InstanceID: types.StringValue(tc.instance),
				ProjectID: types.StringValue(tc.project), Expiry: types.StringValue(expiryIn(time.Hour)), Action: types.StringValue("power off")}

			planned := row
			planned.ID = types.StringUnknown()
			createResp := resource.CreateResponse{State: tfsdk.State{Schema: s, Raw: nullValue(ctx, s)}}
			r.Create(ctx, resource.CreateRequest{Plan: planOf(ctx, t, s, &planned)}, &createResp)
			if !createResp.Diagnostics.HasError() || !createResp.State.Raw.IsNull() {
				t.Errorf("create: diags %v, state null %v; want an error and no state", createResp.Diagnostics, createResp.State.Raw.IsNull())
			}
			readResp := resource.ReadResponse{State: stateOf(ctx, t, s, &row)}
			r.Read(ctx, resource.ReadRequest{State: stateOf(ctx, t, s, &row)}, &readResp)
			if !readResp.Diagnostics.HasError() {
				t.Error("read: no error")
			}
			updateResp := resource.UpdateResponse{State: stateOf(ctx, t, s, &row)}
			r.Update(ctx, resource.UpdateRequest{Plan: planOf(ctx, t, s, &row), State: stateOf(ctx, t, s, &row)}, &updateResp)
			if !updateResp.Diagnostics.HasError() {
				t.Error("update: no error")
			}
			deleteResp := resource.DeleteResponse{State: stateOf(ctx, t, s, &row)}
			r.Delete(ctx, resource.DeleteRequest{State: stateOf(ctx, t, s, &row)}, &deleteResp)
			if deleteResp.Diagnostics.HasError() {
				t.Errorf("delete: %v", deleteResp.Diagnostics)
			}
			if got := f.keys(); len(got) != 0 {
				t.Errorf("requests = %v; project_id %q, instance_id %q must not reach Nova or mors", got, tc.project, tc.instance)
			}
		})
	}
	for _, attr := range []string{"project_id", "instance_id"} {
		a := s.Attributes[attr].(schema.StringAttribute)
		for _, bad := range badIDs {
			if !stringValidatorErrors(ctx, a, types.StringValue(bad)) {
				t.Errorf("%s %q passed the schema's validators", attr, bad)
			}
		}
		if stringValidatorErrors(ctx, a, types.StringValue("0123abcdef_-XYZ")) {
			t.Errorf("a well-formed %s failed the schema's validators", attr)
		}
	}
}

func TestInstanceLeaseImportRefusesMalformedIDs(t *testing.T) {
	ctx := context.Background()
	s := instanceLeaseSchema(t)
	for _, id := range []string{"proj/a/b", "proj-1/vm?x", "p#1/vm-1", "proj-1/vm 1", "proj-1/vm-1/"} {
		resp := resource.ImportStateResponse{State: tfsdk.State{Schema: s, Raw: nullValue(ctx, s)}}
		(&instanceLeaseResource{}).ImportState(ctx, resource.ImportStateRequest{ID: id}, &resp)
		if !resp.Diagnostics.HasError() || !resp.State.Raw.IsNull() {
			t.Errorf("import id %q: diags %v, state null %v; want an error and no state", id, resp.Diagnostics, resp.State.Raw.IsNull())
		}
	}
}
