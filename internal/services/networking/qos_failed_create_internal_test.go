// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package networking

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/platform9/terraform-provider-pcd/internal/clients"
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

// The three QoS rule resources share one shape: a POST under the policy, then
// a read-back, both addressing the rule by its policy's ID and its own.
// qosRuleKind describes one of them, so each test below runs against all three.
type qosRuleKind struct {
	// name is the kind as Neutron spells it: the rule's JSON key is name +
	// "_rule", and its collection under the policy is name + "_rules".
	name string
	// what is the noun the resource's warnings use for the rule.
	what string
	// id is the rule's ID, and json the rule as Neutron returns it.
	id, json string
	// newResource returns the resource, reaching Neutron through config.
	newResource func(config *clients.Config) resource.Resource
	// planned is the model the framework plans for a new rule whose config sets
	// only the Required attributes, so the Computed ones are unknown.
	planned func() any
	// created is the rule's model once read back, and changed the model an
	// update plans for a new rate or mark. withoutID is the row provider
	// v0.1.14 left when the read-back failed: Terraform saved the unknowns
	// Create returned as null, the ID among them.
	created, changed, withoutID func() any
	// readInto calls the resource's readInto for the rule.
	readInto func(config *clients.Config, client *gophercloud.ServiceClient) (notFound bool, diags diag.Diagnostics)
}

// qosRuleKinds returns the three rule kinds, each with its rule under qos-1.
func qosRuleKinds() []qosRuleKind {
	return []qosRuleKind{
		{
			name: "bandwidth_limit",
			what: "QoS bandwidth limit rule",
			id:   "bw-limit-1",
			json: `{"id": "bw-limit-1", "max_kbps": 10000, "max_burst_kbps": 0, "direction": "egress"}`,
			newResource: func(config *clients.Config) resource.Resource {
				return &qosBandwidthLimitRuleResource{config: config}
			},
			planned: func() any {
				return &qosBandwidthLimitRuleModel{ID: types.StringUnknown(), QoSPolicyID: types.StringValue(qosPolicyID),
					MaxKBps: types.Int64Value(10000), MaxBurstKBps: types.Int64Unknown(),
					Direction: types.StringUnknown(), Region: types.StringUnknown()}
			},
			created: func() any {
				return &qosBandwidthLimitRuleModel{ID: types.StringValue("bw-limit-1"), QoSPolicyID: types.StringValue(qosPolicyID),
					MaxKBps: types.Int64Value(10000), MaxBurstKBps: types.Int64Value(0),
					Direction: types.StringValue("egress"), Region: types.StringValue("region-one")}
			},
			changed: func() any {
				return &qosBandwidthLimitRuleModel{ID: types.StringValue("bw-limit-1"), QoSPolicyID: types.StringValue(qosPolicyID),
					MaxKBps: types.Int64Value(20000), MaxBurstKBps: types.Int64Value(0),
					Direction: types.StringValue("egress"), Region: types.StringValue("region-one")}
			},
			withoutID: func() any {
				return &qosBandwidthLimitRuleModel{ID: types.StringNull(), QoSPolicyID: types.StringValue(qosPolicyID),
					MaxKBps: types.Int64Value(10000), MaxBurstKBps: types.Int64Null(),
					Direction: types.StringNull(), Region: types.StringNull()}
			},
			readInto: func(config *clients.Config, client *gophercloud.ServiceClient) (bool, diag.Diagnostics) {
				m := qosBandwidthLimitRuleModel{Region: types.StringNull()}
				return (&qosBandwidthLimitRuleResource{config: config}).readInto(context.Background(), client, qosPolicyID, "bw-limit-1", &m)
			},
		},
		{
			name: "dscp_marking",
			what: "QoS DSCP marking rule",
			id:   "dscp-1",
			json: `{"id": "dscp-1", "dscp_mark": 26}`,
			newResource: func(config *clients.Config) resource.Resource {
				return &qosDSCPMarkingRuleResource{config: config}
			},
			planned: func() any {
				return &qosDSCPMarkingRuleModel{ID: types.StringUnknown(), QoSPolicyID: types.StringValue(qosPolicyID),
					DSCPMark: types.Int64Value(26), Region: types.StringUnknown()}
			},
			created: func() any {
				return &qosDSCPMarkingRuleModel{ID: types.StringValue("dscp-1"), QoSPolicyID: types.StringValue(qosPolicyID),
					DSCPMark: types.Int64Value(26), Region: types.StringValue("region-one")}
			},
			changed: func() any {
				return &qosDSCPMarkingRuleModel{ID: types.StringValue("dscp-1"), QoSPolicyID: types.StringValue(qosPolicyID),
					DSCPMark: types.Int64Value(34), Region: types.StringValue("region-one")}
			},
			withoutID: func() any {
				return &qosDSCPMarkingRuleModel{ID: types.StringNull(), QoSPolicyID: types.StringValue(qosPolicyID),
					DSCPMark: types.Int64Value(26), Region: types.StringNull()}
			},
			readInto: func(config *clients.Config, client *gophercloud.ServiceClient) (bool, diag.Diagnostics) {
				m := qosDSCPMarkingRuleModel{Region: types.StringNull()}
				return (&qosDSCPMarkingRuleResource{config: config}).readInto(context.Background(), client, qosPolicyID, "dscp-1", &m)
			},
		},
		{
			name: "minimum_bandwidth",
			what: "QoS minimum bandwidth rule",
			id:   "min-bw-1",
			json: `{"id": "min-bw-1", "min_kbps": 1000, "direction": "egress"}`,
			newResource: func(config *clients.Config) resource.Resource {
				return &qosMinimumBandwidthRuleResource{config: config}
			},
			planned: func() any {
				return &qosMinimumBandwidthRuleModel{ID: types.StringUnknown(), QoSPolicyID: types.StringValue(qosPolicyID),
					MinKBps: types.Int64Value(1000), Direction: types.StringUnknown(), Region: types.StringUnknown()}
			},
			created: func() any {
				return &qosMinimumBandwidthRuleModel{ID: types.StringValue("min-bw-1"), QoSPolicyID: types.StringValue(qosPolicyID),
					MinKBps: types.Int64Value(1000), Direction: types.StringValue("egress"), Region: types.StringValue("region-one")}
			},
			changed: func() any {
				return &qosMinimumBandwidthRuleModel{ID: types.StringValue("min-bw-1"), QoSPolicyID: types.StringValue(qosPolicyID),
					MinKBps: types.Int64Value(2000), Direction: types.StringValue("egress"), Region: types.StringValue("region-one")}
			},
			withoutID: func() any {
				return &qosMinimumBandwidthRuleModel{ID: types.StringNull(), QoSPolicyID: types.StringValue(qosPolicyID),
					MinKBps: types.Int64Value(1000), Direction: types.StringNull(), Region: types.StringNull()}
			},
			readInto: func(config *clients.Config, client *gophercloud.ServiceClient) (bool, diag.Diagnostics) {
				m := qosMinimumBandwidthRuleModel{Region: types.StringNull()}
				return (&qosMinimumBandwidthRuleResource{config: config}).readInto(context.Background(), client, qosPolicyID, "min-bw-1", &m)
			},
		},
	}
}

// rulesPath is the URL of qos-1's rules of this kind, and rulePath the rule's.
func (k qosRuleKind) rulesPath() string { return qosPolicyPath + "/" + k.name + "_rules" }
func (k qosRuleKind) rulePath() string  { return k.rulesPath() + "/" + k.id }

// body is the rule wrapped in its JSON key, as Neutron answers a create, get or
// update, and listBody the policy's rules of this kind, as Neutron answers a
// GET for their collection.
func (k qosRuleKind) body() string     { return `{"` + k.name + `_rule": ` + k.json + `}` }
func (k qosRuleKind) listBody() string { return `{"` + k.name + `_rules": [` + k.json + `]}` }

// routes answers every call the rule's resource makes the way Neutron does.
// Each test replaces the route whose failure it exercises.
func (k qosRuleKind) routes() neutronRoutes {
	return neutronRoutes{
		"POST " + k.rulesPath():  reply(http.StatusCreated, k.body()),
		"GET " + k.rulePath():    reply(http.StatusOK, k.body()),
		"PUT " + k.rulePath():    reply(http.StatusOK, k.body()),
		"DELETE " + k.rulePath(): reply(http.StatusNoContent, ""),
	}
}

// qosRuleRowID returns the id a rule's state row holds.
func qosRuleRowID(t *testing.T, state tfsdk.State) types.String {
	t.Helper()
	var id types.String
	if d := state.GetAttribute(context.Background(), path.Root("id"), &id); d.HasError() {
		t.Fatalf("reading the state: %v", d)
	}
	return id
}

// A create whose read-back succeeds must return the rule fully known.
func TestQoSRuleCreateFillsEveryAttribute(t *testing.T) {
	t.Parallel()
	for _, k := range qosRuleKinds() {
		t.Run(k.name, func(t *testing.T) {
			t.Parallel()
			neutron := newFakeNeutron(t, k.routes())
			r := k.newResource(neutron.config)
			s := schemaOf(t, r)

			resp := runCreate(r, newPlan(t, s, k.planned()))
			if resp.Diagnostics.HasError() {
				t.Fatalf("create: %v", resp.Diagnostics)
			}
			if want := newState(t, s, k.created()); !resp.State.Raw.Equal(want.Raw) {
				t.Fatalf("create state = %v, want %v", resp.State.Raw, want.Raw)
			}
			want := []string{"POST " + k.rulesPath(), "GET " + k.rulePath()}
			if sent := neutron.received(); !slices.Equal(sent, want) {
				t.Fatalf("create sent %v, want %v", sent, want)
			}
		})
	}
}

// Neutron keeps a rule whose read-back fails. Create used to write the plan's
// unknowns, which Terraform saved as null, ID included, so the rule was
// orphaned and the next refresh read the policy's rule collection and crashed.
// Create must return the error with the rule's ID in state (Terraform then
// taints it), and a refresh must fill in what the read-back would have.
func TestQoSRuleCreateKeepsStateWhenReadBackFails(t *testing.T) {
	t.Parallel()
	for _, k := range qosRuleKinds() {
		t.Run(k.name, func(t *testing.T) {
			t.Parallel()
			routes := k.routes()
			routes["GET "+k.rulePath()] = badGatewayFirst(routes["GET "+k.rulePath()])
			r := k.newResource(newFakeNeutron(t, routes).config)
			s := schemaOf(t, r)

			createResp := runCreate(r, newPlan(t, s, k.planned()))
			if !createResp.Diagnostics.HasError() {
				t.Fatal("create succeeded; want the 502 on the read-back reported")
			}
			if createResp.State.Raw.IsNull() {
				t.Fatalf("create returned no state: Terraform forgets %s, which Neutron keeps", k.id)
			}
			if !createResp.State.Raw.IsFullyKnown() {
				t.Fatalf("create state holds unknown values, which Terraform saves as null: %v", createResp.State.Raw)
			}
			if id := qosRuleRowID(t, createResp.State); id.ValueString() != k.id {
				t.Fatalf("create state id = %s, want %s: a row without one names nothing a destroy could delete", id, k.id)
			}

			// The replacing apply, or a destroy, refreshes the tainted rule first.
			readResp := runRead(r, createResp.State)
			if readResp.Diagnostics.HasError() {
				t.Fatalf("refresh of the recorded rule: %v", readResp.Diagnostics)
			}
			if want := newState(t, s, k.created()); !readResp.State.Raw.Equal(want.Raw) {
				t.Fatalf("refreshed state = %v, want %v", readResp.State.Raw, want.Raw)
			}
		})
	}
}

// A read-back that finds the rule gone right after the POST must be reported,
// with nothing left in state: Create records the rule before the read-back, so
// the 404 has to take that row out again.
func TestQoSRuleCreateDropsStateWhenReadBack404s(t *testing.T) {
	t.Parallel()
	for _, k := range qosRuleKinds() {
		t.Run(k.name, func(t *testing.T) {
			t.Parallel()
			routes := k.routes()
			routes["GET "+k.rulePath()] = reply(http.StatusNotFound, fmt.Sprintf(`{"NeutronError": {"type": "QosRuleNotFound",
				"message": "QoS rule %s for policy qos-1 could not be found.", "detail": ""}}`, k.id))
			r := k.newResource(newFakeNeutron(t, routes).config)

			resp := runCreate(r, newPlan(t, schemaOf(t, r), k.planned()))
			if !resp.Diagnostics.HasError() {
				t.Fatal("create succeeded; want the 404 read-back reported")
			}
			if !resp.State.Raw.IsNull() {
				t.Fatalf("create left a row for a rule the read-back found gone: %v", resp.State.Raw)
			}
		})
	}
}

// The row v0.1.14 left has no rule ID. Read used to send GET for an empty ID,
// which reaches the policy's collection of rules of that kind; Neutron answers
// that with the list, and readInto dereferenced the nil rule gophercloud
// decoded from it. Read must drop the row with a warning, without sending
// anything.
func TestQoSRuleReadDropsARowWithNoID(t *testing.T) {
	t.Parallel()
	for _, k := range qosRuleKinds() {
		t.Run(k.name, func(t *testing.T) {
			t.Parallel()
			routes := k.routes()
			routes["GET "+k.rulesPath()+"/"] = reply(http.StatusOK, k.listBody())
			neutron := newFakeNeutron(t, routes)
			r := k.newResource(neutron.config)

			resp := runRead(r, newState(t, schemaOf(t, r), k.withoutID()))
			if sent := neutron.received(); len(sent) != 0 {
				t.Fatalf("refresh of a row with no ID sent %v; it names nothing to read", sent)
			}
			if resp.Diagnostics.HasError() || resp.Diagnostics.WarningsCount() != 1 {
				t.Fatalf("diagnostics = %v, want exactly one warning", resp.Diagnostics)
			}
			if summary := resp.Diagnostics[0].Summary(); !strings.Contains(summary, k.what) {
				t.Fatalf("warning %q does not say it was a %s", summary, k.what)
			}
			if !resp.State.Raw.IsNull() {
				t.Fatalf("refresh kept a row with no ID: %v", resp.State.Raw)
			}
		})
	}
}

// A 200 for the rule's own URL whose body holds no rule object makes
// gophercloud return no rule and no error. readInto must report that as an
// error, and not as not-found: that would drop a real rule from state.
func TestQoSRuleReadIntoRefusesAnAnswerWithoutTheRule(t *testing.T) {
	t.Parallel()
	for _, k := range qosRuleKinds() {
		t.Run(k.name, func(t *testing.T) {
			t.Parallel()
			routes := k.routes()
			routes["GET "+k.rulePath()] = reply(http.StatusOK, k.listBody())
			config := newFakeNeutron(t, routes).config
			client, err := config.NetworkV2Client()
			if err != nil {
				t.Fatalf("building the fake Neutron client: %v", err)
			}

			notFound, diags := k.readInto(config, client)
			if notFound {
				t.Fatalf("readInto reported %s not found; the next refresh would drop a rule that exists", k.id)
			}
			if !diags.HasError() {
				t.Fatalf("readInto accepted an answer without the rule: diagnostics %v", diags)
			}
		})
	}
}

// An update whose read-back fails must return the error and keep the prior
// state, so the next plan compares against what was last read and retries the
// update. Update used to write the plan, which nothing had read back.
func TestQoSRuleUpdateKeepsStateWhenReadBackFails(t *testing.T) {
	t.Parallel()
	for _, k := range qosRuleKinds() {
		t.Run(k.name, func(t *testing.T) {
			t.Parallel()
			routes := k.routes()
			routes["GET "+k.rulePath()] = badGateway
			r := k.newResource(newFakeNeutron(t, routes).config)
			s := schemaOf(t, r)
			priorState := newState(t, s, k.created())

			resp := runUpdate(r, newPlan(t, s, k.changed()), priorState)
			if !resp.Diagnostics.HasError() {
				t.Fatal("update succeeded; want the 502 on the read-back reported")
			}
			if !resp.State.Raw.Equal(priorState.Raw) {
				t.Fatalf("update state = %v, want the prior state %v", resp.State.Raw, priorState.Raw)
			}
		})
	}
}

// terraform destroy -refresh=false reaches Delete without Read dropping the
// v0.1.14 row first. Delete used to send DELETE for an empty rule ID, which
// reaches the policy's collection of rules, and Neutron refuses that. Delete
// must warn and send nothing, so Terraform forgets the row.
func TestQoSRuleDeleteSkipsARowWithNoID(t *testing.T) {
	t.Parallel()
	for _, k := range qosRuleKinds() {
		t.Run(k.name, func(t *testing.T) {
			t.Parallel()
			routes := k.routes()
			routes["DELETE "+k.rulesPath()+"/"] = reply(http.StatusMethodNotAllowed, "")
			neutron := newFakeNeutron(t, routes)
			r := k.newResource(neutron.config)

			resp := runDelete(r, newState(t, schemaOf(t, r), k.withoutID()))
			if sent := neutron.received(); len(sent) != 0 {
				t.Fatalf("delete of a row with no ID sent %v; it names nothing to delete", sent)
			}
			if resp.Diagnostics.HasError() || resp.Diagnostics.WarningsCount() != 1 {
				t.Fatalf("diagnostics = %v, want exactly one warning", resp.Diagnostics)
			}
		})
	}
}
