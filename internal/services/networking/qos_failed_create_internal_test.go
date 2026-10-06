// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package networking

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

const (
	qosPolicyID   = "qos-1"
	qosPolicyName = "workload-qos"
	qosPolicyDesc = "Rate limits for the workload network"

	qosPoliciesPath = "/v2.0/qos/policies"
	qosPolicyPath   = qosPoliciesPath + "/" + qosPolicyID
)

// qosPolicyJSON is qos-1 as Neutron returns it, with the tag the tests
// configure.
const qosPolicyJSON = `{"id": "qos-1", "name": "workload-qos",
	"description": "Rate limits for the workload network", "shared": false, "is_default": false,
	"tenant_id": "proj-1", "project_id": "proj-1", "tags": ["workload"], "rules": [], "revision_number": 1,
	"created_at": "2026-10-02T00:00:00Z", "updated_at": "2026-10-02T00:00:00Z"}`

// qosPolicyRoutes answers every call qosPolicyResource makes for qos-1 the way
// Neutron does. Each test replaces the route whose failure it exercises.
func qosPolicyRoutes() neutronRoutes {
	return neutronRoutes{
		"POST " + qosPoliciesPath:        reply(http.StatusCreated, `{"policy": `+qosPolicyJSON+`}`),
		"GET " + qosPolicyPath:           reply(http.StatusOK, `{"policy": `+qosPolicyJSON+`}`),
		"PUT " + qosPolicyPath:           reply(http.StatusOK, `{"policy": `+qosPolicyJSON+`}`),
		"PUT " + qosPolicyPath + "/tags": reply(http.StatusOK, `{"tags": ["workload"]}`),
		"DELETE " + qosPolicyPath:        reply(http.StatusNoContent, ""),
	}
}

// qosPolicyPlan is the plan the framework computes for a new policy whose
// config sets name and description, plus tags when given. The Computed
// attributes the config leaves unset are unknown, and shared and is_default
// take their default of false.
func qosPolicyPlan(t *testing.T, r *qosPolicyResource, tags []string) tfsdk.Plan {
	t.Helper()
	planned := qosPolicyModel{
		ID:          types.StringUnknown(),
		Name:        types.StringValue(qosPolicyName),
		Description: types.StringValue(qosPolicyDesc),
		Shared:      types.BoolValue(false),
		IsDefault:   types.BoolValue(false),
		TenantID:    types.StringUnknown(),
		Tags:        types.SetUnknown(types.StringType),
		Region:      types.StringUnknown(),
	}
	if tags != nil {
		set, d := types.SetValueFrom(context.Background(), types.StringType, tags)
		if d.HasError() {
			t.Fatalf("building the tags: %v", d)
		}
		planned.Tags = set
	}
	return newPlan(t, schemaOf(t, r), &planned)
}

// qosPolicyRowWithoutID is the row provider v0.1.14 left in state when the
// read-back after a successful POST failed: Terraform saved the unknowns
// Create returned as null, the ID among them.
func qosPolicyRowWithoutID(t *testing.T, r *qosPolicyResource) tfsdk.State {
	t.Helper()
	return newState(t, schemaOf(t, r), &qosPolicyModel{
		ID:          types.StringNull(),
		Name:        types.StringValue(qosPolicyName),
		Description: types.StringValue(qosPolicyDesc),
		Shared:      types.BoolValue(false),
		IsDefault:   types.BoolValue(false),
		TenantID:    types.StringNull(),
		Tags:        types.SetNull(types.StringType),
		Region:      types.StringNull(),
	})
}

// qosPolicyRow returns the policy a state row holds.
func qosPolicyRow(t *testing.T, state tfsdk.State) qosPolicyModel {
	t.Helper()
	var m qosPolicyModel
	if d := state.Get(context.Background(), &m); d.HasError() {
		t.Fatalf("reading the state: %v", d)
	}
	return m
}

// A create whose every step succeeds must return the policy fully known, after
// setting the tags.
func TestQoSPolicyCreateFillsEveryAttribute(t *testing.T) {
	t.Parallel()
	neutron := newFakeNeutron(t, qosPolicyRoutes())
	r := &qosPolicyResource{config: neutron.config}

	resp := runCreate(r, qosPolicyPlan(t, r, []string{"workload"}))
	if resp.Diagnostics.HasError() {
		t.Fatalf("create: %v", resp.Diagnostics)
	}
	if !resp.State.Raw.IsFullyKnown() {
		t.Fatalf("create state holds unknown values, which Terraform refuses: %v", resp.State.Raw)
	}
	got := qosPolicyRow(t, resp.State)
	if got.ID.ValueString() != qosPolicyID || got.TenantID.ValueString() != "proj-1" ||
		got.Region.ValueString() != "region-one" {
		t.Fatalf("create state id=%s tenant_id=%s region=%s; want qos-1, proj-1, region-one",
			got.ID, got.TenantID, got.Region)
	}
	want := []string{
		"POST " + qosPoliciesPath,
		"PUT " + qosPolicyPath + "/tags",
		"GET " + qosPolicyPath,
	}
	if sent := neutron.received(); !slices.Equal(sent, want) {
		t.Fatalf("create sent %v, want %v", sent, want)
	}
}

// Neutron keeps a policy whose read-back fails. Create reported a 404 there,
// but any other error let it write the plan's unknowns, which Terraform saved
// as null, ID included, so the policy was orphaned and the next refresh read
// the collection URL and crashed. Create must return the error with the
// policy's ID in state (Terraform then taints it), and a refresh must fill in
// what the read-back would have.
func TestQoSPolicyCreateKeepsStateWhenReadBackFails(t *testing.T) {
	t.Parallel()
	routes := qosPolicyRoutes()
	routes["GET "+qosPolicyPath] = badGatewayFirst(routes["GET "+qosPolicyPath])
	r := &qosPolicyResource{config: newFakeNeutron(t, routes).config}

	createResp := runCreate(r, qosPolicyPlan(t, r, nil))
	if !createResp.Diagnostics.HasError() {
		t.Fatal("create succeeded; want the 502 on the read-back reported")
	}
	if createResp.State.Raw.IsNull() {
		t.Fatal("create returned no state: Terraform forgets qos-1 and the next apply creates a second policy")
	}
	if !createResp.State.Raw.IsFullyKnown() {
		t.Fatalf("create state holds unknown values, which Terraform saves as null: %v", createResp.State.Raw)
	}
	if got := qosPolicyRow(t, createResp.State); got.ID.ValueString() != qosPolicyID {
		t.Fatalf("create state id = %s, want qos-1: a row without one names nothing a destroy could delete", got.ID)
	}

	// The replacing apply, or a destroy, refreshes the tainted policy first.
	readResp := runRead(r, createResp.State)
	if readResp.Diagnostics.HasError() {
		t.Fatalf("refresh of the recorded policy: %v", readResp.Diagnostics)
	}
	if readResp.State.Raw.IsNull() {
		t.Fatal("refresh dropped qos-1 from state")
	}
	got := qosPolicyRow(t, readResp.State)
	if got.ID.ValueString() != qosPolicyID || got.TenantID.ValueString() != "proj-1" ||
		got.Region.ValueString() != "region-one" || got.Tags.IsNull() {
		t.Fatalf("refreshed id=%s tenant_id=%s region=%s tags=%s; want qos-1, proj-1, region-one, [workload]",
			got.ID, got.TenantID, got.Region, got.Tags)
	}
}

// A failed tags PUT used to return before Create set any state, so Terraform
// forgot the policy and the next apply created another.
func TestQoSPolicyCreateKeepsStateWhenTagsFail(t *testing.T) {
	t.Parallel()
	routes := qosPolicyRoutes()
	routes["PUT "+qosPolicyPath+"/tags"] = badGateway
	r := &qosPolicyResource{config: newFakeNeutron(t, routes).config}

	resp := runCreate(r, qosPolicyPlan(t, r, []string{"workload"}))
	if !resp.Diagnostics.HasError() {
		t.Fatal("create succeeded; want the 502 on the tags PUT reported")
	}
	if resp.State.Raw.IsNull() {
		t.Fatal("create returned no state: Terraform forgets qos-1 and the next apply creates a second policy")
	}
	if !resp.State.Raw.IsFullyKnown() {
		t.Fatalf("create state holds unknown values, which Terraform saves as null: %v", resp.State.Raw)
	}
	if got := qosPolicyRow(t, resp.State); got.ID.ValueString() != qosPolicyID {
		t.Fatalf("create state id = %s, want qos-1", got.ID)
	}
}

// A read-back that finds the policy gone right after the POST must be
// reported, with nothing left in state: Create records the policy before the
// read-back, so the 404 has to take that row out again.
func TestQoSPolicyCreateDropsStateWhenReadBack404s(t *testing.T) {
	t.Parallel()
	routes := qosPolicyRoutes()
	routes["GET "+qosPolicyPath] = reply(http.StatusNotFound,
		`{"NeutronError": {"type": "QosPolicyNotFound", "message": "QoS policy qos-1 could not be found.", "detail": ""}}`)
	r := &qosPolicyResource{config: newFakeNeutron(t, routes).config}

	resp := runCreate(r, qosPolicyPlan(t, r, nil))
	if !resp.Diagnostics.HasError() {
		t.Fatal("create succeeded; want the 404 read-back reported")
	}
	if !resp.State.Raw.IsNull() {
		t.Fatalf("create left a row for a policy the read-back found gone: %v", resp.State.Raw)
	}
}

// The row v0.1.14 left has no ID. Read used to send GET for an empty ID, which
// reaches the collection URL; Neutron answers that with the list, and readInto
// dereferenced the nil policy gophercloud decoded from it. Read must drop the
// row with a warning that names the policy, without sending anything.
func TestQoSPolicyReadDropsARowWithNoID(t *testing.T) {
	t.Parallel()
	routes := qosPolicyRoutes()
	routes["GET "+qosPoliciesPath+"/"] = reply(http.StatusOK, `{"policies": [`+qosPolicyJSON+`]}`)
	neutron := newFakeNeutron(t, routes)
	r := &qosPolicyResource{config: neutron.config}

	resp := runRead(r, qosPolicyRowWithoutID(t, r))
	if sent := neutron.received(); len(sent) != 0 {
		t.Fatalf("refresh of a row with no ID sent %v; it names nothing to read", sent)
	}
	if resp.Diagnostics.HasError() || resp.Diagnostics.WarningsCount() != 1 {
		t.Fatalf("diagnostics = %v, want exactly one warning", resp.Diagnostics)
	}
	if detail := resp.Diagnostics[0].Detail(); !strings.Contains(detail, `"workload-qos"`) {
		t.Fatalf("warning detail %q does not name the policy to look for", detail)
	}
	if !resp.State.Raw.IsNull() {
		t.Fatalf("refresh kept a row with no ID: %v", resp.State.Raw)
	}
}

// A 200 for the policy's own URL whose body holds no policy object makes
// gophercloud return no policy and no error. readInto must report that as an
// error, and not as not-found: that would drop a real policy from state.
func TestQoSPolicyReadIntoRefusesAnAnswerWithoutThePolicy(t *testing.T) {
	t.Parallel()
	routes := qosPolicyRoutes()
	routes["GET "+qosPolicyPath] = reply(http.StatusOK, `{"policies": [`+qosPolicyJSON+`]}`)
	r := &qosPolicyResource{config: newFakeNeutron(t, routes).config}
	client, err := r.config.NetworkV2Client()
	if err != nil {
		t.Fatalf("building the fake Neutron client: %v", err)
	}

	m := qosPolicyModel{Region: types.StringNull()}
	notFound, diags := r.readInto(context.Background(), client, qosPolicyID, &m)
	if notFound {
		t.Fatal("readInto reported qos-1 not found; the next refresh would drop a policy that exists")
	}
	if !diags.HasError() {
		t.Fatalf("readInto accepted an answer without the policy: diagnostics %v", diags)
	}
}

// An update whose read-back fails must return the error and keep the prior
// state, so the next plan compares against what was last read and retries the
// update. Update used to write the plan, which nothing had read back.
func TestQoSPolicyUpdateKeepsStateWhenReadBackFails(t *testing.T) {
	t.Parallel()
	routes := qosPolicyRoutes()
	routes["GET "+qosPolicyPath] = badGateway
	r := &qosPolicyResource{config: newFakeNeutron(t, routes).config}
	s := schemaOf(t, r)

	tags, d := types.SetValueFrom(context.Background(), types.StringType, []string{"workload"})
	if d.HasError() {
		t.Fatalf("building the tags: %v", d)
	}
	prior := qosPolicyModel{
		ID:          types.StringValue(qosPolicyID),
		Name:        types.StringValue(qosPolicyName),
		Description: types.StringValue(qosPolicyDesc),
		Shared:      types.BoolValue(false),
		IsDefault:   types.BoolValue(false),
		TenantID:    types.StringValue("proj-1"),
		Tags:        tags,
		Region:      types.StringValue("region-one"),
	}
	// A rename. Every Computed attribute keeps its prior value in the plan.
	planned := prior
	planned.Name = types.StringValue(qosPolicyName + "-renamed")
	priorState := newState(t, s, &prior)

	resp := runUpdate(r, newPlan(t, s, &planned), priorState)
	if !resp.Diagnostics.HasError() {
		t.Fatal("update succeeded; want the 502 on the read-back reported")
	}
	if !resp.State.Raw.Equal(priorState.Raw) {
		t.Fatalf("update state = %v, want the prior state %v", resp.State.Raw, priorState.Raw)
	}
}

// terraform destroy -refresh=false reaches Delete without Read dropping the
// v0.1.14 row first. Delete used to send DELETE for an empty ID, which reaches
// the collection URL, and Neutron refuses that. Delete must warn and send
// nothing, so Terraform forgets the row.
func TestQoSPolicyDeleteSkipsARowWithNoID(t *testing.T) {
	t.Parallel()
	routes := qosPolicyRoutes()
	routes["DELETE "+qosPoliciesPath+"/"] = reply(http.StatusMethodNotAllowed, "")
	neutron := newFakeNeutron(t, routes)
	r := &qosPolicyResource{config: neutron.config}

	resp := runDelete(r, qosPolicyRowWithoutID(t, r))
	if sent := neutron.received(); len(sent) != 0 {
		t.Fatalf("delete of a row with no ID sent %v; it names nothing to delete", sent)
	}
	if resp.Diagnostics.HasError() || resp.Diagnostics.WarningsCount() != 1 {
		t.Fatalf("diagnostics = %v, want exactly one warning", resp.Diagnostics)
	}
}
