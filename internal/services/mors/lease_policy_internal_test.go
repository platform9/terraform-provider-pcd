// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package mors

import (
	"context"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"

	"github.com/platform9/terraform-provider-pcd/internal/clients"
)

// config returns a clients.Config whose catalog resolves every service type to
// the fake. EndpointOverrides stays empty so the catalog path is the one used.
// Like client, it gives the provider client a transport of its own.
func (f *fakeMors) config() *clients.Config {
	return &clients.Config{
		Region: "region-one",
		Provider: &gophercloud.ProviderClient{
			HTTPClient:      http.Client{Transport: &http.Transport{}},
			EndpointLocator: func(gophercloud.EndpointOpts) (string, error) { return f.srv.URL + "/", nil },
		},
	}
}

// keys lists the requests received, in order.
func (f *fakeMors) keys() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.calls))
	for _, c := range f.calls {
		out = append(out, c.key)
	}
	return out
}

// nullValue is the empty state or plan value for the schema.
func nullValue(ctx context.Context, s schema.Schema) tftypes.Value {
	return tftypes.NewValue(s.Type().TerraformType(ctx), nil)
}

// planOf builds a plan holding model.
func planOf(ctx context.Context, t *testing.T, s schema.Schema, model any) tfsdk.Plan {
	t.Helper()
	plan := tfsdk.Plan{Schema: s, Raw: nullValue(ctx, s)}
	if d := plan.Set(ctx, model); d.HasError() {
		t.Fatalf("building the plan: %v", d)
	}
	return plan
}

// stateOf builds a state holding model.
func stateOf(ctx context.Context, t *testing.T, s schema.Schema, model any) tfsdk.State {
	t.Helper()
	state := tfsdk.State{Schema: s, Raw: nullValue(ctx, s)}
	if d := state.Set(ctx, model); d.HasError() {
		t.Fatalf("building the state: %v", d)
	}
	return state
}

// stringValidatorErrors runs an attribute's validators against v.
func stringValidatorErrors(ctx context.Context, attr schema.StringAttribute, v types.String) bool {
	for _, val := range attr.Validators {
		resp := &validator.StringResponse{}
		val.ValidateString(ctx, validator.StringRequest{Path: path.Root("attr"), ConfigValue: v}, resp)
		if resp.Diagnostics.HasError() {
			return true
		}
	}
	return false
}

func leasePolicySchema(t *testing.T) schema.Schema {
	t.Helper()
	var sch resource.SchemaResponse
	(&leasePolicyResource{}).Schema(context.Background(), resource.SchemaRequest{}, &sch)
	if sch.Diagnostics.HasError() {
		t.Fatalf("schema: %v", sch.Diagnostics)
	}
	return sch.Schema
}

const policyBody = `{"vm_lease_policy": {"tenant_uuid": "proj-1", "expiry_mins": 4320, "action": "power off",
	"created_at": "2026-10-01T00:00:00Z", "created_by": "u1", "updated_at": null, "updated_by": null}}`

// mors writes the project named by the body's tenant_uuid, not the one in the
// URL. A body built from anything but the URL's project would put a policy on
// some other project, so both must carry the same ID, on POST and on PUT.
func TestLeasePolicyBodyNamesTheURLProject(t *testing.T) {
	for _, method := range []string{http.MethodPost, http.MethodPut} {
		t.Run(method, func(t *testing.T) {
			f := newFakeMors(t, map[string][]fakeReply{method + " /v1/tenant/proj-1": {{http.StatusOK, `{"success": true}`}}})
			if err := writeLeasePolicy(context.Background(), f.client(), method, "proj-1", 4320, "delete"); err != nil {
				t.Fatalf("writeLeasePolicy: %v", err)
			}
			want := map[string]any{"vm_lease_policy": map[string]any{
				"tenant_uuid": "proj-1", "expiry_mins": float64(4320), "action": "delete",
			}}
			if got := f.bodyOf(method + " /v1/tenant/proj-1"); !reflect.DeepEqual(got, want) {
				t.Fatalf("body = %v\nwant  %v", got, want)
			}
		})
	}
}

func TestLeasePolicyLifecycle(t *testing.T) {
	ctx := context.Background()
	f := newFakeMors(t, map[string][]fakeReply{
		"POST /v1/tenant/proj-1": {{http.StatusOK, `{"success": true}`}},
		"PUT /v1/tenant/proj-1":  {{http.StatusOK, `{"success": true}`}},
		"GET /v1/tenant/proj-1": {
			{http.StatusOK, policyBody},
			{http.StatusOK, strings.Replace(strings.Replace(policyBody, "4320", "60", 1), `"power off"`, `"delete"`, 1)},
		},
		"DELETE /v1/tenant/proj-1": {{http.StatusOK, `{"success": true}`}},
	})
	r := &leasePolicyResource{config: f.config()}
	s := leasePolicySchema(t)

	planned := leasePolicyModel{
		ID:            types.StringUnknown(),
		ProjectID:     types.StringValue("proj-1"),
		ExpiryMinutes: types.Int64Value(4320),
		Action:        types.StringValue("power off"),
	}
	createResp := resource.CreateResponse{State: tfsdk.State{Schema: s, Raw: nullValue(ctx, s)}}
	r.Create(ctx, resource.CreateRequest{Plan: planOf(ctx, t, s, &planned)}, &createResp)
	if createResp.Diagnostics.HasError() {
		t.Fatalf("create: %v", createResp.Diagnostics)
	}
	var created leasePolicyModel
	createResp.State.Get(ctx, &created)
	if created.ID.ValueString() != "proj-1" {
		t.Fatalf("id = %s, want the project ID", created.ID)
	}

	readResp := resource.ReadResponse{State: createResp.State}
	r.Read(ctx, resource.ReadRequest{State: createResp.State}, &readResp)
	if readResp.Diagnostics.HasError() {
		t.Fatalf("read: %v", readResp.Diagnostics)
	}
	var read leasePolicyModel
	readResp.State.Get(ctx, &read)
	if read.ExpiryMinutes.ValueInt64() != 4320 || read.Action.ValueString() != "power off" {
		t.Fatalf("read expiry_minutes=%d action=%s, want 4320 and power off", read.ExpiryMinutes.ValueInt64(), read.Action.ValueString())
	}

	updated := read
	updated.ExpiryMinutes = types.Int64Value(60)
	updated.Action = types.StringValue("delete")
	updateResp := resource.UpdateResponse{State: readResp.State}
	r.Update(ctx, resource.UpdateRequest{Plan: planOf(ctx, t, s, &updated), State: readResp.State}, &updateResp)
	if updateResp.Diagnostics.HasError() {
		t.Fatalf("update: %v", updateResp.Diagnostics)
	}
	if body := f.bodyOf("PUT /v1/tenant/proj-1"); body["vm_lease_policy"].(map[string]any)["action"] != "delete" {
		t.Fatalf("PUT body = %v; mors answers a PUT without action with a 500", body)
	}

	deleteResp := resource.DeleteResponse{State: updateResp.State}
	r.Delete(ctx, resource.DeleteRequest{State: updateResp.State}, &deleteResp)
	if deleteResp.Diagnostics.HasError() {
		t.Fatalf("delete: %v", deleteResp.Diagnostics)
	}

	want := []string{
		"POST /v1/tenant/proj-1", "GET /v1/tenant/proj-1", "PUT /v1/tenant/proj-1",
		"GET /v1/tenant/proj-1", "DELETE /v1/tenant/proj-1",
	}
	if got := f.keys(); !reflect.DeepEqual(got, want) {
		t.Fatalf("requests = %v\nwant       %v", got, want)
	}
}

// A policy made in the PCD UI already exists; mors answers 409. The user needs
// to be told to import it, not just shown the status.
func TestLeasePolicyCreateNamesTheImport(t *testing.T) {
	ctx := context.Background()
	f := newFakeMors(t, map[string][]fakeReply{
		"POST /v1/tenant/proj-1": {{http.StatusConflict, `{"error": "Already exists"}`}},
	})
	r := &leasePolicyResource{config: f.config()}
	s := leasePolicySchema(t)
	planned := leasePolicyModel{ID: types.StringUnknown(), ProjectID: types.StringValue("proj-1"),
		ExpiryMinutes: types.Int64Value(60), Action: types.StringValue("power off")}
	resp := resource.CreateResponse{State: tfsdk.State{Schema: s, Raw: nullValue(ctx, s)}}
	r.Create(ctx, resource.CreateRequest{Plan: planOf(ctx, t, s, &planned)}, &resp)
	if !resp.Diagnostics.HasError() || !strings.Contains(resp.Diagnostics[0].Detail(), "terraform import") {
		t.Fatalf("diagnostics = %v, want an error that says to import the policy", resp.Diagnostics)
	}
	if !resp.State.Raw.IsNull() {
		t.Fatal("a refused create left state behind")
	}
}

// A project_id unknown at plan skips the schema's length check and can reach
// apply as "". /v1/tenant/ is mors's list route, so an empty project ID must be
// refused before any request (the fake fails the test on any).
func TestLeasePolicyCreateRefusesAnEmptyProjectID(t *testing.T) {
	ctx := context.Background()
	f := newFakeMors(t, map[string][]fakeReply{})
	r := &leasePolicyResource{config: f.config()}
	s := leasePolicySchema(t)
	planned := leasePolicyModel{ID: types.StringUnknown(), ProjectID: types.StringValue(""),
		ExpiryMinutes: types.Int64Value(60), Action: types.StringValue("power off")}
	resp := resource.CreateResponse{State: tfsdk.State{Schema: s, Raw: nullValue(ctx, s)}}
	r.Create(ctx, resource.CreateRequest{Plan: planOf(ctx, t, s, &planned)}, &resp)
	if !resp.Diagnostics.HasError() || !resp.State.Raw.IsNull() {
		t.Fatalf("create: diags %v, state null %v; want an error and no state", resp.Diagnostics, resp.State.Raw.IsNull())
	}
	if got := f.keys(); len(got) != 0 {
		t.Fatalf("requests = %v; an empty project ID must not reach mors", got)
	}
}

// mors answers a PUT for a project with no policy with 200 and does nothing.
// Update must notice instead of recording an update that never happened.
func TestLeasePolicyUpdateOfAPolicyDeletedOutsideTerraform(t *testing.T) {
	ctx := context.Background()
	f := newFakeMors(t, map[string][]fakeReply{
		"PUT /v1/tenant/proj-1": {{http.StatusOK, `{"success": true}`}},
		"GET /v1/tenant/proj-1": {{http.StatusNotFound, `{"success": false}`}},
	})
	r := &leasePolicyResource{config: f.config()}
	s := leasePolicySchema(t)
	prior := leasePolicyModel{ID: types.StringValue("proj-1"), ProjectID: types.StringValue("proj-1"),
		ExpiryMinutes: types.Int64Value(60), Action: types.StringValue("power off")}
	planned := prior
	planned.ExpiryMinutes = types.Int64Value(120)
	resp := resource.UpdateResponse{State: stateOf(ctx, t, s, &prior)}
	r.Update(ctx, resource.UpdateRequest{Plan: planOf(ctx, t, s, &planned), State: stateOf(ctx, t, s, &prior)}, &resp)
	if !resp.Diagnostics.HasError() {
		t.Fatal("update reported success over a policy mors does not hold")
	}
}

func TestLeasePolicyReadDropsAPolicyThatIsGone(t *testing.T) {
	ctx := context.Background()
	f := newFakeMors(t, map[string][]fakeReply{
		"GET /v1/tenant/proj-1": {{http.StatusNotFound, `{"success": false}`}},
	})
	r := &leasePolicyResource{config: f.config()}
	s := leasePolicySchema(t)
	prior := leasePolicyModel{ID: types.StringValue("proj-1"), ProjectID: types.StringValue("proj-1"),
		ExpiryMinutes: types.Int64Value(60), Action: types.StringValue("power off")}
	resp := resource.ReadResponse{State: stateOf(ctx, t, s, &prior)}
	r.Read(ctx, resource.ReadRequest{State: stateOf(ctx, t, s, &prior)}, &resp)
	if resp.Diagnostics.HasError() || !resp.State.Raw.IsNull() {
		t.Fatalf("read of a deleted policy: diags %v, state null %v; want it removed from state", resp.Diagnostics, resp.State.Raw.IsNull())
	}
}

// GET /v1/tenant/ answers 200 {}, and so might a proxy or a changed API: a 200
// that holds no policy must fail the read, not save expiry_minutes 0 and
// action "" over the state.
func TestLeasePolicyReadOfAnAnswerWithoutAPolicy(t *testing.T) {
	ctx := context.Background()
	f := newFakeMors(t, map[string][]fakeReply{
		"GET /v1/tenant/proj-1": {{http.StatusOK, `{}`}},
	})
	r := &leasePolicyResource{config: f.config()}
	s := leasePolicySchema(t)
	prior := leasePolicyModel{ID: types.StringValue("proj-1"), ProjectID: types.StringValue("proj-1"),
		ExpiryMinutes: types.Int64Value(60), Action: types.StringValue("power off")}
	resp := resource.ReadResponse{State: stateOf(ctx, t, s, &prior)}
	r.Read(ctx, resource.ReadRequest{State: stateOf(ctx, t, s, &prior)}, &resp)
	if !resp.Diagnostics.HasError() {
		t.Fatal("read accepted an answer without a policy")
	}
	var kept leasePolicyModel
	resp.State.Get(ctx, &kept)
	if kept.ExpiryMinutes.ValueInt64() != 60 || kept.Action.ValueString() != "power off" {
		t.Fatalf("state = %+v; want the prior state kept", kept)
	}
}

// mors validates neither expiry_mins nor action: it stores 0, negative values
// and any action string, and treats every action but "delete" as a power off.
// The schema is the only guard.
func TestLeasePolicyValidators(t *testing.T) {
	ctx := context.Background()
	s := leasePolicySchema(t)
	action := s.Attributes["action"].(schema.StringAttribute)
	for v, wantErr := range map[string]bool{"power off": false, "delete": false, "shutdown": true, "Power Off": true, "": true} {
		if got := stringValidatorErrors(ctx, action, types.StringValue(v)); got != wantErr {
			t.Errorf("action %q: error %v, want %v", v, got, wantErr)
		}
	}
	minutes := s.Attributes["expiry_minutes"].(schema.Int64Attribute)
	for v, wantErr := range map[int64]bool{1: false, 4320: false, 0: true, -5: true} {
		failed := false
		for _, val := range minutes.Validators {
			resp := &validator.Int64Response{}
			val.ValidateInt64(ctx, validator.Int64Request{Path: path.Root("expiry_minutes"), ConfigValue: types.Int64Value(v)}, resp)
			failed = failed || resp.Diagnostics.HasError()
		}
		if failed != wantErr {
			t.Errorf("expiry_minutes %d: error %v, want %v", v, failed, wantErr)
		}
	}
}

func TestLeasePolicyImportState(t *testing.T) {
	ctx := context.Background()
	s := leasePolicySchema(t)
	resp := resource.ImportStateResponse{State: tfsdk.State{Schema: s, Raw: nullValue(ctx, s)}}
	(&leasePolicyResource{}).ImportState(ctx, resource.ImportStateRequest{ID: "proj-1"}, &resp)
	var got leasePolicyModel
	resp.State.Get(ctx, &got)
	if resp.Diagnostics.HasError() || got.ID.ValueString() != "proj-1" || got.ProjectID.ValueString() != "proj-1" {
		t.Fatalf("import: diags %v, id %s, project_id %s; want both proj-1", resp.Diagnostics, got.ID, got.ProjectID)
	}
	empty := resource.ImportStateResponse{State: tfsdk.State{Schema: s, Raw: nullValue(ctx, s)}}
	(&leasePolicyResource{}).ImportState(ctx, resource.ImportStateRequest{ID: ""}, &empty)
	if !empty.Diagnostics.HasError() || !empty.State.Raw.IsNull() {
		t.Fatalf("empty import ID: diags %v, state null %v; want an error and no state", empty.Diagnostics, empty.State.Raw.IsNull())
	}
}

// A policy changed in the PCD UI must show as drift: Read stores what mors
// holds, not the prior state, or the plan reports no change while mors acts
// on the project's VMs under another duration and action.
func TestLeasePolicyReadReportsDrift(t *testing.T) {
	ctx := context.Background()
	f := newFakeMors(t, map[string][]fakeReply{
		"GET /v1/tenant/proj-1": {{http.StatusOK, `{"vm_lease_policy": {"tenant_uuid": "proj-1", "expiry_mins": 60, "action": "delete"}}`}},
	})
	r := &leasePolicyResource{config: f.config()}
	s := leasePolicySchema(t)
	prior := leasePolicyModel{ID: types.StringValue("proj-1"), ProjectID: types.StringValue("proj-1"),
		ExpiryMinutes: types.Int64Value(4320), Action: types.StringValue("power off")}
	resp := resource.ReadResponse{State: stateOf(ctx, t, s, &prior)}
	r.Read(ctx, resource.ReadRequest{State: stateOf(ctx, t, s, &prior)}, &resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("read: %v", resp.Diagnostics)
	}
	var got leasePolicyModel
	resp.State.Get(ctx, &got)
	if got.ExpiryMinutes.ValueInt64() != 60 || got.Action.ValueString() != "delete" {
		t.Fatalf("read expiry_minutes=%d action=%s, want mors's 60 and delete", got.ExpiryMinutes.ValueInt64(), got.Action.ValueString())
	}
}

// mors answers DELETE with 200, but a 404 from anything in between also means
// there is nothing left to delete: destroy must succeed.
func TestLeasePolicyDeleteOfAPolicyAlreadyGone(t *testing.T) {
	ctx := context.Background()
	f := newFakeMors(t, map[string][]fakeReply{
		"DELETE /v1/tenant/proj-1": {{http.StatusNotFound, `{"success": false}`}},
	})
	r := &leasePolicyResource{config: f.config()}
	s := leasePolicySchema(t)
	prior := leasePolicyModel{ID: types.StringValue("proj-1"), ProjectID: types.StringValue("proj-1"),
		ExpiryMinutes: types.Int64Value(60), Action: types.StringValue("power off")}
	resp := resource.DeleteResponse{State: stateOf(ctx, t, s, &prior)}
	r.Delete(ctx, resource.DeleteRequest{State: stateOf(ctx, t, s, &prior)}, &resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("delete of a policy already gone: %v", resp.Diagnostics)
	}
	if got := f.keys(); !reflect.DeepEqual(got, []string{"DELETE /v1/tenant/proj-1"}) {
		t.Fatalf("requests = %v", got)
	}
}

// Update's read-back is what proves a PUT landed. A 200 that holds no policy
// proves nothing: it must fail the update and keep the prior state.
func TestLeasePolicyUpdateReadBackWithoutAPolicy(t *testing.T) {
	ctx := context.Background()
	f := newFakeMors(t, map[string][]fakeReply{
		"PUT /v1/tenant/proj-1": {{http.StatusOK, `{"success": true}`}},
		"GET /v1/tenant/proj-1": {{http.StatusOK, `{}`}},
	})
	r := &leasePolicyResource{config: f.config()}
	s := leasePolicySchema(t)
	prior := leasePolicyModel{ID: types.StringValue("proj-1"), ProjectID: types.StringValue("proj-1"),
		ExpiryMinutes: types.Int64Value(60), Action: types.StringValue("power off")}
	planned := prior
	planned.ExpiryMinutes = types.Int64Value(120)
	resp := resource.UpdateResponse{State: stateOf(ctx, t, s, &prior)}
	r.Update(ctx, resource.UpdateRequest{Plan: planOf(ctx, t, s, &planned), State: stateOf(ctx, t, s, &prior)}, &resp)
	if !resp.Diagnostics.HasError() {
		t.Fatal("update accepted a read-back without a policy")
	}
	var kept leasePolicyModel
	resp.State.Get(ctx, &kept)
	if kept.ExpiryMinutes.ValueInt64() != 60 {
		t.Fatalf("state expiry_minutes = %d; want the prior 60 kept", kept.ExpiryMinutes.ValueInt64())
	}
}

// badIDs go into the URL path unescaped and would address another route or
// another object; "" is mors's list route.
var badIDs = []string{"", "a/b", "a?b", "a#b", "a b", "..", "a%2Fb"}

// Every request is refused for a malformed project_id, and none reaches mors
// (the fake fails the test on any). Delete has nothing it could delete.
func TestLeasePolicyRefusesMalformedProjectIDs(t *testing.T) {
	ctx := context.Background()
	s := leasePolicySchema(t)
	for _, id := range badIDs {
		t.Run(id, func(t *testing.T) {
			f := newFakeMors(t, map[string][]fakeReply{})
			r := &leasePolicyResource{config: f.config()}
			row := leasePolicyModel{ID: types.StringValue(id), ProjectID: types.StringValue(id),
				ExpiryMinutes: types.Int64Value(60), Action: types.StringValue("power off")}

			createResp := resource.CreateResponse{State: tfsdk.State{Schema: s, Raw: nullValue(ctx, s)}}
			r.Create(ctx, resource.CreateRequest{Plan: planOf(ctx, t, s, &row)}, &createResp)
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
				t.Errorf("requests = %v; project_id %q must not reach mors", got, id)
			}
			if id != "" && !stringValidatorErrors(ctx, s.Attributes["project_id"].(schema.StringAttribute), types.StringValue(id)) {
				t.Errorf("project_id %q passed the schema's validators", id)
			}
		})
	}
	if stringValidatorErrors(ctx, s.Attributes["project_id"].(schema.StringAttribute), types.StringValue("0123abcdef_-XYZ")) {
		t.Error("a well-formed project_id failed the schema's validators")
	}
}

func TestLeasePolicyImportRefusesMalformedIDs(t *testing.T) {
	ctx := context.Background()
	s := leasePolicySchema(t)
	for _, id := range append([]string{"proj/a/b", "proj-1/"}, badIDs...) {
		resp := resource.ImportStateResponse{State: tfsdk.State{Schema: s, Raw: nullValue(ctx, s)}}
		(&leasePolicyResource{}).ImportState(ctx, resource.ImportStateRequest{ID: id}, &resp)
		if !resp.Diagnostics.HasError() || !resp.State.Raw.IsNull() {
			t.Errorf("import id %q: diags %v, state null %v; want an error and no state", id, resp.Diagnostics, resp.State.Raw.IsNull())
		}
	}
}
