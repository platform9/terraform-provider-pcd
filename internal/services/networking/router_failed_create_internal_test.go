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
	routerID           = "router-1"
	routerName         = "workload-router"
	routerDesc         = "Gateway for the workload network"
	routerExtNetworkID = "ext-net-1"

	routersPath = "/v2.0/routers"
	routerPath  = routersPath + "/" + routerID
)

// routerJSON is router-1 as Neutron returns it, with a gateway on ext-net-1
// (which holds a port and an external IP) and the tag the tests configure.
const routerJSON = `{"id": "router-1", "name": "workload-router",
	"description": "Gateway for the workload network", "admin_state_up": true, "status": "ACTIVE",
	"external_gateway_info": {"network_id": "ext-net-1", "enable_snat": true,
		"external_fixed_ips": [{"subnet_id": "ext-subnet-1", "ip_address": "203.0.113.10"}]},
	"tenant_id": "proj-1", "project_id": "proj-1", "tags": ["workload"], "revision_number": 1,
	"routes": [], "availability_zone_hints": [], "distributed": false,
	"created_at": "2026-10-02T00:00:00Z", "updated_at": "2026-10-02T00:00:00Z"}`

// routerRoutes answers every call routerResource makes for router-1 the way
// Neutron does. Each test replaces the route whose failure it exercises.
func routerRoutes() neutronRoutes {
	return neutronRoutes{
		"POST " + routersPath:         reply(http.StatusCreated, `{"router": `+routerJSON+`}`),
		"GET " + routerPath:           reply(http.StatusOK, `{"router": `+routerJSON+`}`),
		"PUT " + routerPath:           reply(http.StatusOK, `{"router": `+routerJSON+`}`),
		"PUT " + routerPath + "/tags": reply(http.StatusOK, `{"tags": ["workload"]}`),
		"DELETE " + routerPath:        reply(http.StatusNoContent, ""),
	}
}

// routerPlan is the plan the framework computes for a new router whose config
// sets name, description and external_network_id, plus tags when given. The
// Computed attributes the config leaves unset are unknown, enable_snat among
// them, and admin_state_up takes its default of true.
func routerPlan(t *testing.T, r *routerResource, tags []string) tfsdk.Plan {
	t.Helper()
	planned := routerModel{
		ID:                types.StringUnknown(),
		Name:              types.StringValue(routerName),
		Description:       types.StringValue(routerDesc),
		AdminStateUp:      types.BoolValue(true),
		ExternalNetworkID: types.StringValue(routerExtNetworkID),
		EnableSNAT:        types.BoolUnknown(),
		TenantID:          types.StringUnknown(),
		Tags:              types.SetUnknown(types.StringType),
		Region:            types.StringUnknown(),
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

// routerRowWithoutID is the row provider v0.1.14 left in state when the
// read-back after a successful POST failed: Terraform saved the unknowns
// Create returned as null, the ID among them.
func routerRowWithoutID(t *testing.T, r *routerResource) tfsdk.State {
	t.Helper()
	return newState(t, schemaOf(t, r), &routerModel{
		ID:                types.StringNull(),
		Name:              types.StringValue(routerName),
		Description:       types.StringValue(routerDesc),
		AdminStateUp:      types.BoolValue(true),
		ExternalNetworkID: types.StringValue(routerExtNetworkID),
		EnableSNAT:        types.BoolNull(),
		TenantID:          types.StringNull(),
		Tags:              types.SetNull(types.StringType),
		Region:            types.StringNull(),
	})
}

// routerPriorState is router-1 as a successful create left it in state.
func routerPriorState(t *testing.T) routerModel {
	t.Helper()
	tags, d := types.SetValueFrom(context.Background(), types.StringType, []string{"workload"})
	if d.HasError() {
		t.Fatalf("building the tags: %v", d)
	}
	return routerModel{
		ID:                types.StringValue(routerID),
		Name:              types.StringValue(routerName),
		Description:       types.StringValue(routerDesc),
		AdminStateUp:      types.BoolValue(true),
		ExternalNetworkID: types.StringValue(routerExtNetworkID),
		EnableSNAT:        types.BoolValue(true),
		TenantID:          types.StringValue("proj-1"),
		Tags:              tags,
		Region:            types.StringValue("region-one"),
	}
}

// routerRenamePlan is the plan for renaming the router in prior. enable_snat
// has no plan modifier, so a config that leaves it unset plans it unknown.
func routerRenamePlan(prior routerModel) routerModel {
	planned := prior
	planned.Name = types.StringValue(routerName + "-renamed")
	planned.EnableSNAT = types.BoolUnknown()
	return planned
}

// routerRow reads the router model out of state.
func routerRow(t *testing.T, state tfsdk.State) routerModel {
	t.Helper()
	var m routerModel
	if d := state.Get(context.Background(), &m); d.HasError() {
		t.Fatalf("reading the state: %v", d)
	}
	return m
}

// A create whose every step succeeds must return the router fully known, after
// setting the tags.
func TestRouterCreateFillsEveryAttribute(t *testing.T) {
	t.Parallel()
	neutron := newFakeNeutron(t, routerRoutes())
	r := &routerResource{config: neutron.config}

	resp := runCreate(r, routerPlan(t, r, []string{"workload"}))
	if resp.Diagnostics.HasError() {
		t.Fatalf("create: %v", resp.Diagnostics)
	}
	if !resp.State.Raw.IsFullyKnown() {
		t.Fatalf("create state holds unknown values, which Terraform refuses: %v", resp.State.Raw)
	}
	got := routerRow(t, resp.State)
	if got.ID.ValueString() != routerID || got.TenantID.ValueString() != "proj-1" ||
		!got.EnableSNAT.ValueBool() || got.Region.ValueString() != "region-one" {
		t.Fatalf("create state id=%s tenant_id=%s enable_snat=%s region=%s; want router-1, proj-1, true, region-one",
			got.ID, got.TenantID, got.EnableSNAT, got.Region)
	}
	want := []string{
		"POST " + routersPath,
		"PUT " + routerPath + "/tags",
		"GET " + routerPath,
	}
	if sent := neutron.received(); !slices.Equal(sent, want) {
		t.Fatalf("create sent %v, want %v", sent, want)
	}
}

// Neutron keeps a router whose read-back fails, with its gateway port. Create
// used to write the plan's unknowns, which Terraform saved as null, ID
// included, so the router was orphaned and the next refresh read the
// collection URL and crashed. Create must return the error with the router's
// ID in state (Terraform then taints it), and a refresh must fill in what the
// read-back would have.
func TestRouterCreateKeepsStateWhenReadBackFails(t *testing.T) {
	t.Parallel()
	routes := routerRoutes()
	routes["GET "+routerPath] = badGatewayFirst(routes["GET "+routerPath])
	r := &routerResource{config: newFakeNeutron(t, routes).config}

	createResp := runCreate(r, routerPlan(t, r, nil))
	if !createResp.Diagnostics.HasError() {
		t.Fatal("create succeeded; want the 502 on the read-back reported")
	}
	if createResp.State.Raw.IsNull() {
		t.Fatal("create returned no state: Terraform forgets router-1 and the next apply creates a second router")
	}
	if !createResp.State.Raw.IsFullyKnown() {
		t.Fatalf("create state holds unknown values, which Terraform saves as null: %v", createResp.State.Raw)
	}
	if got := routerRow(t, createResp.State); got.ID.ValueString() != routerID {
		t.Fatalf("create state id = %s, want router-1: a row without one names nothing a destroy could delete", got.ID)
	}

	// The replacing apply, or a destroy, refreshes the tainted router first.
	readResp := runRead(r, createResp.State)
	if readResp.Diagnostics.HasError() {
		t.Fatalf("refresh of the recorded router: %v", readResp.Diagnostics)
	}
	if readResp.State.Raw.IsNull() {
		t.Fatal("refresh dropped router-1 from state")
	}
	got := routerRow(t, readResp.State)
	if got.ID.ValueString() != routerID || got.TenantID.ValueString() != "proj-1" ||
		!got.EnableSNAT.ValueBool() || got.Region.ValueString() != "region-one" {
		t.Fatalf("refreshed id=%s tenant_id=%s enable_snat=%s region=%s; want router-1, proj-1, true, region-one",
			got.ID, got.TenantID, got.EnableSNAT, got.Region)
	}
}

// A failed tags PUT used to return before Create set any state, so Terraform
// forgot the router, and its gateway port, and the next apply created another.
func TestRouterCreateKeepsStateWhenTagsFail(t *testing.T) {
	t.Parallel()
	routes := routerRoutes()
	routes["PUT "+routerPath+"/tags"] = badGateway
	r := &routerResource{config: newFakeNeutron(t, routes).config}

	resp := runCreate(r, routerPlan(t, r, []string{"workload"}))
	if !resp.Diagnostics.HasError() {
		t.Fatal("create succeeded; want the 502 on the tags PUT reported")
	}
	if resp.State.Raw.IsNull() {
		t.Fatal("create returned no state: Terraform forgets router-1 and the next apply creates a second router")
	}
	if !resp.State.Raw.IsFullyKnown() {
		t.Fatalf("create state holds unknown values, which Terraform saves as null: %v", resp.State.Raw)
	}
	if got := routerRow(t, resp.State); got.ID.ValueString() != routerID {
		t.Fatalf("create state id = %s, want router-1", got.ID)
	}
}

// A read-back that finds the router gone right after the POST used to be
// swallowed: Create reported nothing and Terraform saved a row with no ID.
// Create must report it and leave nothing in state.
func TestRouterCreateDropsStateWhenReadBack404s(t *testing.T) {
	t.Parallel()
	routes := routerRoutes()
	routes["GET "+routerPath] = reply(http.StatusNotFound,
		`{"NeutronError": {"type": "RouterNotFound", "message": "Router router-1 could not be found", "detail": ""}}`)
	r := &routerResource{config: newFakeNeutron(t, routes).config}

	resp := runCreate(r, routerPlan(t, r, nil))
	if !resp.Diagnostics.HasError() {
		t.Fatal("create succeeded; want the 404 read-back reported")
	}
	if !resp.State.Raw.IsNull() {
		t.Fatalf("create left a row for a router the read-back found gone: %v", resp.State.Raw)
	}
}

// The row v0.1.14 left has no ID. Read used to send GET for an empty ID, which
// reaches the collection URL; Neutron answers that with the list, and readInto
// dereferenced the nil router gophercloud decoded from it. Read must drop the
// row with a warning that names the router, without sending anything.
func TestRouterReadDropsARowWithNoID(t *testing.T) {
	t.Parallel()
	routes := routerRoutes()
	routes["GET "+routersPath+"/"] = reply(http.StatusOK, `{"routers": [`+routerJSON+`]}`)
	neutron := newFakeNeutron(t, routes)
	r := &routerResource{config: neutron.config}

	resp := runRead(r, routerRowWithoutID(t, r))
	if sent := neutron.received(); len(sent) != 0 {
		t.Fatalf("refresh of a row with no ID sent %v; it names nothing to read", sent)
	}
	if resp.Diagnostics.HasError() || resp.Diagnostics.WarningsCount() != 1 {
		t.Fatalf("diagnostics = %v, want exactly one warning", resp.Diagnostics)
	}
	if detail := resp.Diagnostics[0].Detail(); !strings.Contains(detail, `"workload-router"`) {
		t.Fatalf("warning detail %q does not name the router to look for", detail)
	}
	if !resp.State.Raw.IsNull() {
		t.Fatalf("refresh kept a row with no ID: %v", resp.State.Raw)
	}
}

// A 200 for the router's own URL whose body holds no router object makes
// gophercloud return no router and no error. readInto must report that as an
// error, and not as not-found: that would drop a real router from state.
func TestRouterReadIntoRefusesAnAnswerWithoutTheRouter(t *testing.T) {
	t.Parallel()
	routes := routerRoutes()
	routes["GET "+routerPath] = reply(http.StatusOK, `{"routers": [`+routerJSON+`]}`)
	r := &routerResource{config: newFakeNeutron(t, routes).config}
	client, err := r.config.NetworkV2Client()
	if err != nil {
		t.Fatalf("building the fake Neutron client: %v", err)
	}

	m := routerModel{Region: types.StringNull()}
	notFound, diags := r.readInto(context.Background(), client, routerID, &m)
	if notFound {
		t.Fatal("readInto reported router-1 not found; the next refresh would drop a router that exists")
	}
	if !diags.HasError() {
		t.Fatalf("readInto accepted an answer without the router: diagnostics %v", diags)
	}
}

// An update whose read-back fails must return the error and keep the prior
// state, which the next plan compares against. Update used to write the plan,
// including its unknown enable_snat, which Terraform refuses.
func TestRouterUpdateKeepsStateWhenReadBackFails(t *testing.T) {
	t.Parallel()
	routes := routerRoutes()
	routes["GET "+routerPath] = badGateway
	r := &routerResource{config: newFakeNeutron(t, routes).config}
	s := schemaOf(t, r)

	prior := routerPriorState(t)
	planned := routerRenamePlan(prior)
	priorState := newState(t, s, &prior)

	resp := runUpdate(r, newPlan(t, s, &planned), priorState)
	if !resp.Diagnostics.HasError() {
		t.Fatal("update succeeded; want the 502 on the read-back reported")
	}
	if !resp.State.Raw.IsFullyKnown() {
		t.Fatalf("update state holds unknown values (enable_snat), which Terraform refuses: %v", resp.State.Raw)
	}
	if !resp.State.Raw.Equal(priorState.Raw) {
		t.Fatalf("update state = %v, want the prior state %v", resp.State.Raw, priorState.Raw)
	}
}

// An update whose read-back finds the router gone must report it and keep the
// prior state; the next refresh drops the row. Update used to report nothing
// and write the plan, including its unknown enable_snat.
func TestRouterUpdateKeepsStateWhenReadBack404s(t *testing.T) {
	t.Parallel()
	routes := routerRoutes()
	routes["GET "+routerPath] = reply(http.StatusNotFound,
		`{"NeutronError": {"type": "RouterNotFound", "message": "Router router-1 could not be found", "detail": ""}}`)
	r := &routerResource{config: newFakeNeutron(t, routes).config}
	s := schemaOf(t, r)

	prior := routerPriorState(t)
	planned := routerRenamePlan(prior)
	priorState := newState(t, s, &prior)

	resp := runUpdate(r, newPlan(t, s, &planned), priorState)
	if !resp.Diagnostics.HasError() {
		t.Fatal("update succeeded; want the 404 read-back reported")
	}
	if !resp.State.Raw.Equal(priorState.Raw) {
		t.Fatalf("update state = %v, want the prior state %v", resp.State.Raw, priorState.Raw)
	}
}

// terraform destroy -refresh=false reaches Delete without Read dropping the
// v0.1.14 row first. Delete used to send DELETE for an empty ID, which reaches
// the collection URL, and Neutron refuses that. Delete must warn, naming the
// router, and send nothing, so Terraform forgets the row.
func TestRouterDeleteSkipsARowWithNoID(t *testing.T) {
	t.Parallel()
	routes := routerRoutes()
	routes["DELETE "+routersPath+"/"] = reply(http.StatusMethodNotAllowed, "")
	neutron := newFakeNeutron(t, routes)
	r := &routerResource{config: neutron.config}

	resp := runDelete(r, routerRowWithoutID(t, r))
	if sent := neutron.received(); len(sent) != 0 {
		t.Fatalf("delete of a row with no ID sent %v; it names nothing to delete", sent)
	}
	if resp.Diagnostics.HasError() || resp.Diagnostics.WarningsCount() != 1 {
		t.Fatalf("diagnostics = %v, want exactly one warning", resp.Diagnostics)
	}
	if detail := resp.Diagnostics[0].Detail(); !strings.Contains(detail, `"workload-router"`) {
		t.Fatalf("warning detail %q does not name the router to look for", detail)
	}
}
