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
	secgroupID   = "sg-1"
	secgroupName = "workload-secgroup"
	secgroupDesc = "SSH and ICMP for the workload instance"

	secgroupsPath = "/v2.0/security-groups"
	secgroupPath  = secgroupsPath + "/" + secgroupID
)

// secgroupJSON is sg-1 as Neutron returns it, with the two egress rules Neutron
// adds to every new group and the tag the tests configure.
const secgroupJSON = `{"id": "sg-1", "name": "workload-secgroup",
	"description": "SSH and ICMP for the workload instance", "stateful": true,
	"tenant_id": "proj-1", "project_id": "proj-1", "tags": ["workload"], "revision_number": 1,
	"created_at": "2026-10-02T00:00:00Z", "updated_at": "2026-10-02T00:00:00Z",
	"security_group_rules": [
		{"id": "rule-egress-v4", "direction": "egress", "ethertype": "IPv4", "security_group_id": "sg-1", "tenant_id": "proj-1", "project_id": "proj-1"},
		{"id": "rule-egress-v6", "direction": "egress", "ethertype": "IPv6", "security_group_id": "sg-1", "tenant_id": "proj-1", "project_id": "proj-1"}]}`

// secgroupRoutes answers every call secgroupResource makes for sg-1 the way
// Neutron does. Each test replaces the route whose failure it exercises.
func secgroupRoutes() neutronRoutes {
	return neutronRoutes{
		"POST " + secgroupsPath:                            reply(http.StatusCreated, `{"security_group": `+secgroupJSON+`}`),
		"GET " + secgroupPath:                              reply(http.StatusOK, `{"security_group": `+secgroupJSON+`}`),
		"PUT " + secgroupPath:                              reply(http.StatusOK, `{"security_group": `+secgroupJSON+`}`),
		"PUT " + secgroupPath + "/tags":                    reply(http.StatusOK, `{"tags": ["workload"]}`),
		"DELETE /v2.0/security-group-rules/rule-egress-v4": reply(http.StatusNoContent, ""),
		"DELETE /v2.0/security-group-rules/rule-egress-v6": reply(http.StatusNoContent, ""),
		"DELETE " + secgroupPath:                           reply(http.StatusNoContent, ""),
	}
}

// secgroupPlan is the plan the framework computes for a new group whose config
// sets name and description, plus tags and delete_default_rules when given. The
// Computed attributes the config leaves unset are unknown, and
// delete_default_rules takes its default of false.
func secgroupPlan(t *testing.T, r *secgroupResource, tags []string, deleteDefaultRules bool) tfsdk.Plan {
	t.Helper()
	planned := secgroupModel{
		ID:                 types.StringUnknown(),
		Name:               types.StringValue(secgroupName),
		Description:        types.StringValue(secgroupDesc),
		Stateful:           types.BoolUnknown(),
		DeleteDefaultRules: types.BoolValue(deleteDefaultRules),
		TenantID:           types.StringUnknown(),
		Tags:               types.SetUnknown(types.StringType),
		Region:             types.StringUnknown(),
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

// secgroupRowWithoutID is the row provider v0.1.14 left in state when the
// read-back after a successful POST failed: Terraform saved the unknowns
// Create returned as null, the ID among them.
func secgroupRowWithoutID(t *testing.T, r *secgroupResource) tfsdk.State {
	t.Helper()
	return newState(t, schemaOf(t, r), &secgroupModel{
		ID:                 types.StringNull(),
		Name:               types.StringValue(secgroupName),
		Description:        types.StringValue(secgroupDesc),
		Stateful:           types.BoolNull(),
		DeleteDefaultRules: types.BoolValue(false),
		TenantID:           types.StringNull(),
		Tags:               types.SetNull(types.StringType),
		Region:             types.StringNull(),
	})
}

func secgroupRow(t *testing.T, state tfsdk.State) secgroupModel {
	t.Helper()
	var m secgroupModel
	if d := state.Get(context.Background(), &m); d.HasError() {
		t.Fatalf("reading the state: %v", d)
	}
	return m
}

// A create whose every step succeeds must return the group fully known, after
// deleting the default rules and setting the tags.
func TestSecgroupCreateFillsEveryAttribute(t *testing.T) {
	t.Parallel()
	neutron := newFakeNeutron(t, secgroupRoutes())
	r := &secgroupResource{config: neutron.config}

	resp := runCreate(r, secgroupPlan(t, r, []string{"workload"}, true))
	if resp.Diagnostics.HasError() {
		t.Fatalf("create: %v", resp.Diagnostics)
	}
	if !resp.State.Raw.IsFullyKnown() {
		t.Fatalf("create state holds unknown values, which Terraform refuses: %v", resp.State.Raw)
	}
	got := secgroupRow(t, resp.State)
	if got.ID.ValueString() != secgroupID || got.TenantID.ValueString() != "proj-1" ||
		!got.Stateful.ValueBool() || got.Region.ValueString() != "region-one" {
		t.Fatalf("create state id=%s tenant_id=%s stateful=%s region=%s; want sg-1, proj-1, true, region-one",
			got.ID, got.TenantID, got.Stateful, got.Region)
	}
	want := []string{
		"POST " + secgroupsPath,
		"DELETE /v2.0/security-group-rules/rule-egress-v4",
		"DELETE /v2.0/security-group-rules/rule-egress-v6",
		"PUT " + secgroupPath + "/tags",
		"GET " + secgroupPath,
	}
	if sent := neutron.received(); !slices.Equal(sent, want) {
		t.Fatalf("create sent %v, want %v", sent, want)
	}
}

// Neutron keeps a group whose read-back fails. Create used to write the plan's
// unknowns, which Terraform saved as null, ID included, so the group was
// orphaned and the next refresh read the collection URL and crashed. Create
// must return the error with the group's ID in state (Terraform then taints
// it), and a refresh must fill in what the read-back would have.
func TestSecgroupCreateKeepsStateWhenReadBackFails(t *testing.T) {
	t.Parallel()
	routes := secgroupRoutes()
	routes["GET "+secgroupPath] = badGatewayFirst(routes["GET "+secgroupPath])
	r := &secgroupResource{config: newFakeNeutron(t, routes).config}

	createResp := runCreate(r, secgroupPlan(t, r, nil, false))
	if !createResp.Diagnostics.HasError() {
		t.Fatal("create succeeded; want the 502 on the read-back reported")
	}
	if createResp.State.Raw.IsNull() {
		t.Fatal("create returned no state: Terraform forgets sg-1 and the next apply creates a second group")
	}
	if !createResp.State.Raw.IsFullyKnown() {
		t.Fatalf("create state holds unknown values, which Terraform saves as null: %v", createResp.State.Raw)
	}
	if got := secgroupRow(t, createResp.State); got.ID.ValueString() != secgroupID {
		t.Fatalf("create state id = %s, want sg-1: a row without one names nothing a destroy could delete", got.ID)
	}

	// The replacing apply, or a destroy, refreshes the tainted group first.
	readResp := runRead(r, createResp.State)
	if readResp.Diagnostics.HasError() {
		t.Fatalf("refresh of the recorded group: %v", readResp.Diagnostics)
	}
	if readResp.State.Raw.IsNull() {
		t.Fatal("refresh dropped sg-1 from state")
	}
	got := secgroupRow(t, readResp.State)
	if got.ID.ValueString() != secgroupID || got.TenantID.ValueString() != "proj-1" ||
		!got.Stateful.ValueBool() || got.Region.ValueString() != "region-one" {
		t.Fatalf("refreshed id=%s tenant_id=%s stateful=%s region=%s; want sg-1, proj-1, true, region-one",
			got.ID, got.TenantID, got.Stateful, got.Region)
	}
}

// A failed tags PUT used to return before Create set any state, so Terraform
// forgot the group and the next apply created another.
func TestSecgroupCreateKeepsStateWhenTagsFail(t *testing.T) {
	t.Parallel()
	routes := secgroupRoutes()
	routes["PUT "+secgroupPath+"/tags"] = badGateway
	r := &secgroupResource{config: newFakeNeutron(t, routes).config}

	resp := runCreate(r, secgroupPlan(t, r, []string{"workload"}, false))
	if !resp.Diagnostics.HasError() {
		t.Fatal("create succeeded; want the 502 on the tags PUT reported")
	}
	if resp.State.Raw.IsNull() {
		t.Fatal("create returned no state: Terraform forgets sg-1 and the next apply creates a second group")
	}
	if !resp.State.Raw.IsFullyKnown() {
		t.Fatalf("create state holds unknown values, which Terraform saves as null: %v", resp.State.Raw)
	}
	if got := secgroupRow(t, resp.State); got.ID.ValueString() != secgroupID {
		t.Fatalf("create state id = %s, want sg-1", got.ID)
	}
}

// A failed default-rule DELETE used to return before Create set any state, so
// Terraform forgot the group, with one of its default rules already gone.
func TestSecgroupCreateKeepsStateWhenDefaultRuleDeleteFails(t *testing.T) {
	t.Parallel()
	routes := secgroupRoutes()
	routes["DELETE /v2.0/security-group-rules/rule-egress-v6"] = badGateway
	r := &secgroupResource{config: newFakeNeutron(t, routes).config}

	resp := runCreate(r, secgroupPlan(t, r, nil, true))
	if !resp.Diagnostics.HasError() {
		t.Fatal("create succeeded; want the 502 on the default rule DELETE reported")
	}
	if resp.State.Raw.IsNull() {
		t.Fatal("create returned no state: Terraform forgets sg-1 and the next apply creates a second group")
	}
	if !resp.State.Raw.IsFullyKnown() {
		t.Fatalf("create state holds unknown values, which Terraform saves as null: %v", resp.State.Raw)
	}
	got := secgroupRow(t, resp.State)
	if got.ID.ValueString() != secgroupID || !got.DeleteDefaultRules.ValueBool() {
		t.Fatalf("create state id=%s delete_default_rules=%s; want sg-1, true", got.ID, got.DeleteDefaultRules)
	}
}

// A read-back that finds the group gone right after the POST used to be
// swallowed: Create reported nothing and Terraform saved a row with no ID.
// Create must report it and leave nothing in state.
func TestSecgroupCreateDropsStateWhenReadBack404s(t *testing.T) {
	t.Parallel()
	routes := secgroupRoutes()
	routes["GET "+secgroupPath] = reply(http.StatusNotFound,
		`{"NeutronError": {"type": "SecurityGroupNotFound", "message": "Security group sg-1 does not exist", "detail": ""}}`)
	r := &secgroupResource{config: newFakeNeutron(t, routes).config}

	resp := runCreate(r, secgroupPlan(t, r, nil, false))
	if !resp.Diagnostics.HasError() {
		t.Fatal("create succeeded; want the 404 read-back reported")
	}
	if !resp.State.Raw.IsNull() {
		t.Fatalf("create left a row for a group the read-back found gone: %v", resp.State.Raw)
	}
}

// The row v0.1.14 left has no ID. Read used to send GET for an empty ID, which
// reaches the collection URL; Neutron answers that with the list, and readInto
// dereferenced the nil group gophercloud decoded from it. Read must drop the
// row with a warning that names the group, without sending anything.
func TestSecgroupReadDropsARowWithNoID(t *testing.T) {
	t.Parallel()
	routes := secgroupRoutes()
	routes["GET "+secgroupsPath+"/"] = reply(http.StatusOK, `{"security_groups": [`+secgroupJSON+`]}`)
	neutron := newFakeNeutron(t, routes)
	r := &secgroupResource{config: neutron.config}

	resp := runRead(r, secgroupRowWithoutID(t, r))
	if sent := neutron.received(); len(sent) != 0 {
		t.Fatalf("refresh of a row with no ID sent %v; it names nothing to read", sent)
	}
	if resp.Diagnostics.HasError() || resp.Diagnostics.WarningsCount() != 1 {
		t.Fatalf("diagnostics = %v, want exactly one warning", resp.Diagnostics)
	}
	if detail := resp.Diagnostics[0].Detail(); !strings.Contains(detail, `"workload-secgroup"`) {
		t.Fatalf("warning detail %q does not name the group to look for", detail)
	}
	if !resp.State.Raw.IsNull() {
		t.Fatalf("refresh kept a row with no ID: %v", resp.State.Raw)
	}
}

// A 200 for the group's own URL whose body holds no security_group object
// makes gophercloud return no group and no error. readInto must report that as
// an error, and not as not-found: that would drop a real group from state.
func TestSecgroupReadIntoRefusesAnAnswerWithoutTheGroup(t *testing.T) {
	t.Parallel()
	routes := secgroupRoutes()
	routes["GET "+secgroupPath] = reply(http.StatusOK, `{"security_groups": [`+secgroupJSON+`]}`)
	r := &secgroupResource{config: newFakeNeutron(t, routes).config}
	client, err := r.config.NetworkV2Client()
	if err != nil {
		t.Fatalf("building the fake Neutron client: %v", err)
	}

	m := secgroupModel{Region: types.StringNull()}
	notFound, diags := r.readInto(context.Background(), client, secgroupID, &m)
	if notFound {
		t.Fatal("readInto reported sg-1 not found; the next refresh would drop a group that exists")
	}
	if !diags.HasError() {
		t.Fatalf("readInto accepted an answer without the group: diagnostics %v", diags)
	}
}

// An update whose read-back fails must return the error and keep the prior
// state, which the next plan compares against. Update used to write the plan,
// including its unknown stateful, which Terraform refuses.
func TestSecgroupUpdateKeepsStateWhenReadBackFails(t *testing.T) {
	t.Parallel()
	routes := secgroupRoutes()
	routes["GET "+secgroupPath] = badGateway
	r := &secgroupResource{config: newFakeNeutron(t, routes).config}
	s := schemaOf(t, r)

	tags, d := types.SetValueFrom(context.Background(), types.StringType, []string{"workload"})
	if d.HasError() {
		t.Fatalf("building the tags: %v", d)
	}
	prior := secgroupModel{
		ID:                 types.StringValue(secgroupID),
		Name:               types.StringValue(secgroupName),
		Description:        types.StringValue(secgroupDesc),
		Stateful:           types.BoolValue(true),
		DeleteDefaultRules: types.BoolValue(false),
		TenantID:           types.StringValue("proj-1"),
		Tags:               tags,
		Region:             types.StringValue("region-one"),
	}
	// A rename. stateful has no plan modifier, so a config that leaves it unset
	// plans it unknown.
	planned := prior
	planned.Name = types.StringValue(secgroupName + "-renamed")
	planned.Stateful = types.BoolUnknown()
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
func TestSecgroupDeleteSkipsARowWithNoID(t *testing.T) {
	t.Parallel()
	routes := secgroupRoutes()
	routes["DELETE "+secgroupsPath+"/"] = reply(http.StatusMethodNotAllowed, "")
	neutron := newFakeNeutron(t, routes)
	r := &secgroupResource{config: neutron.config}

	resp := runDelete(r, secgroupRowWithoutID(t, r))
	if sent := neutron.received(); len(sent) != 0 {
		t.Fatalf("delete of a row with no ID sent %v; it names nothing to delete", sent)
	}
	if resp.Diagnostics.HasError() || resp.Diagnostics.WarningsCount() != 1 {
		t.Fatalf("diagnostics = %v, want exactly one warning", resp.Diagnostics)
	}
}
