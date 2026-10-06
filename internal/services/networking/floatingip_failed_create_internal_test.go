// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package networking

import (
	"context"
	"net/http"
	"slices"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

const (
	floatingIPID      = "fip-1"
	floatingIPPool    = "public"
	floatingIPAddress = "203.0.113.10"

	floatingIPCollectionPath = "/v2.0/floatingips"
	floatingIPItemPath       = floatingIPCollectionPath + "/" + floatingIPID
)

// floatingIPPoolJSON is the external network named public, as Neutron lists it.
const floatingIPPoolJSON = `{"id": "ext-net-1", "name": "public", "router:external": true,
	"status": "ACTIVE", "admin_state_up": true, "shared": false, "subnets": ["ext-subnet-1"],
	"tenant_id": "admin-proj", "project_id": "admin-proj"}`

// floatingIPJSON is fip-1 as Neutron returns it: allocated from public, not
// associated with a port, and carrying the tag the tests configure.
const floatingIPJSON = `{"id": "fip-1", "floating_ip_address": "203.0.113.10",
	"floating_network_id": "ext-net-1", "port_id": null, "fixed_ip_address": null, "router_id": null,
	"status": "DOWN", "description": "", "tenant_id": "proj-1", "project_id": "proj-1",
	"tags": ["workload"], "revision_number": 1, "port_forwardings": [],
	"created_at": "2026-10-02T00:00:00Z", "updated_at": "2026-10-02T00:00:00Z"}`

// floatingIPRoutes answers every call floatingIPResource makes for fip-1 the
// way Neutron does, including the external network lookup that resolves pool.
// Each test replaces the route whose failure it exercises.
func floatingIPRoutes() neutronRoutes {
	return neutronRoutes{
		"GET /v2.0/networks":                  reply(http.StatusOK, `{"networks": [`+floatingIPPoolJSON+`]}`),
		"POST " + floatingIPCollectionPath:    reply(http.StatusCreated, `{"floatingip": `+floatingIPJSON+`}`),
		"GET " + floatingIPItemPath:           reply(http.StatusOK, `{"floatingip": `+floatingIPJSON+`}`),
		"PUT " + floatingIPItemPath:           reply(http.StatusOK, `{"floatingip": `+floatingIPJSON+`}`),
		"PUT " + floatingIPItemPath + "/tags": reply(http.StatusOK, `{"tags": ["workload"]}`),
		"DELETE " + floatingIPItemPath:        reply(http.StatusNoContent, ""),
	}
}

// floatingIPPlan is the plan the framework computes for a new floating IP whose
// config sets pool, plus tags when given. The Computed attributes the config
// leaves unset are unknown, and ModifyPlan changes nothing while port_id is
// unset.
func floatingIPPlan(t *testing.T, r *floatingIPResource, tags []string) tfsdk.Plan {
	t.Helper()
	planned := floatingIPModel{
		ID:                types.StringUnknown(),
		Pool:              types.StringValue(floatingIPPool),
		FloatingNetworkID: types.StringUnknown(),
		Description:       types.StringUnknown(),
		Address:           types.StringUnknown(),
		PortID:            types.StringUnknown(),
		FixedIP:           types.StringUnknown(),
		TenantID:          types.StringUnknown(),
		Status:            types.StringUnknown(),
		RouterID:          types.StringUnknown(),
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

// floatingIPPortChange returns the state of fip-1, unassociated, and the plan
// the framework computes from it when the config sets port_id to port-1:
// ModifyPlan marks fixed_ip, router_id and status unknown, since Neutron
// derives them from the port.
func floatingIPPortChange(t *testing.T, r *floatingIPResource) (tfsdk.Plan, tfsdk.State) {
	t.Helper()
	s := schemaOf(t, r)
	tags, d := types.SetValueFrom(context.Background(), types.StringType, []string{"workload"})
	if d.HasError() {
		t.Fatalf("building the tags: %v", d)
	}
	prior := floatingIPModel{
		ID:                types.StringValue(floatingIPID),
		Pool:              types.StringValue(floatingIPPool),
		FloatingNetworkID: types.StringValue("ext-net-1"),
		Description:       types.StringValue(""),
		Address:           types.StringValue(floatingIPAddress),
		PortID:            types.StringValue(""),
		FixedIP:           types.StringValue(""),
		TenantID:          types.StringValue("proj-1"),
		Status:            types.StringValue("DOWN"),
		RouterID:          types.StringValue(""),
		Tags:              tags,
		Region:            types.StringValue("region-one"),
	}
	planned := prior
	planned.PortID = types.StringValue("port-1")
	planned.FixedIP = types.StringUnknown()
	planned.Status = types.StringUnknown()
	planned.RouterID = types.StringUnknown()
	return newPlan(t, s, &planned), newState(t, s, &prior)
}

// floatingIPRow returns the floating IP that state holds.
func floatingIPRow(t *testing.T, state tfsdk.State) floatingIPModel {
	t.Helper()
	var m floatingIPModel
	if d := state.Get(context.Background(), &m); d.HasError() {
		t.Fatalf("reading the state: %v", d)
	}
	return m
}

// A create whose every step succeeds must return the floating IP fully known,
// after resolving the pool and setting the tags.
func TestFloatingIPCreateFillsEveryAttribute(t *testing.T) {
	t.Parallel()
	neutron := newFakeNeutron(t, floatingIPRoutes())
	r := &floatingIPResource{config: neutron.config}

	resp := runCreate(r, floatingIPPlan(t, r, []string{"workload"}))
	if resp.Diagnostics.HasError() {
		t.Fatalf("create: %v", resp.Diagnostics)
	}
	if !resp.State.Raw.IsFullyKnown() {
		t.Fatalf("create state holds unknown values, which Terraform refuses: %v", resp.State.Raw)
	}
	got := floatingIPRow(t, resp.State)
	if got.ID.ValueString() != floatingIPID || got.Address.ValueString() != floatingIPAddress ||
		got.FloatingNetworkID.ValueString() != "ext-net-1" || got.Region.ValueString() != "region-one" {
		t.Fatalf("create state id=%s address=%s floating_network_id=%s region=%s; want fip-1, 203.0.113.10, ext-net-1, region-one",
			got.ID, got.Address, got.FloatingNetworkID, got.Region)
	}
	want := []string{
		"GET /v2.0/networks",
		"POST " + floatingIPCollectionPath,
		"PUT " + floatingIPItemPath + "/tags",
		"GET " + floatingIPItemPath,
	}
	if sent := neutron.received(); !slices.Equal(sent, want) {
		t.Fatalf("create sent %v, want %v", sent, want)
	}
}

// Neutron keeps a floating IP whose read-back fails. Create used to write the
// plan's unknowns, which Terraform saved as null, ID included, so the address
// stayed allocated with nothing pointing at it, and the next refresh read the
// collection URL and saved id "". Create must return the error with the
// floating IP's ID in state (Terraform then taints it), and a refresh must
// fill in what the read-back would have.
func TestFloatingIPCreateKeepsStateWhenReadBackFails(t *testing.T) {
	t.Parallel()
	routes := floatingIPRoutes()
	routes["GET "+floatingIPItemPath] = badGatewayFirst(routes["GET "+floatingIPItemPath])
	r := &floatingIPResource{config: newFakeNeutron(t, routes).config}

	createResp := runCreate(r, floatingIPPlan(t, r, nil))
	if !createResp.Diagnostics.HasError() {
		t.Fatal("create succeeded; want the 502 on the read-back reported")
	}
	if createResp.State.Raw.IsNull() {
		t.Fatal("create returned no state: Terraform forgets fip-1 and the next apply allocates a second address")
	}
	if !createResp.State.Raw.IsFullyKnown() {
		t.Fatalf("create state holds unknown values, which Terraform saves as null: %v", createResp.State.Raw)
	}
	if got := floatingIPRow(t, createResp.State); got.ID.ValueString() != floatingIPID {
		t.Fatalf("create state id = %s, want fip-1: a row without one names nothing a destroy could delete", got.ID)
	}

	// The replacing apply, or a destroy, refreshes the tainted floating IP first.
	readResp := runRead(r, createResp.State)
	if readResp.Diagnostics.HasError() {
		t.Fatalf("refresh of the recorded floating IP: %v", readResp.Diagnostics)
	}
	if readResp.State.Raw.IsNull() {
		t.Fatal("refresh dropped fip-1 from state")
	}
	got := floatingIPRow(t, readResp.State)
	if got.ID.ValueString() != floatingIPID || got.Address.ValueString() != floatingIPAddress ||
		got.Status.ValueString() != "DOWN" || got.Region.ValueString() != "region-one" {
		t.Fatalf("refreshed id=%s address=%s status=%s region=%s; want fip-1, 203.0.113.10, DOWN, region-one",
			got.ID, got.Address, got.Status, got.Region)
	}
}

// A failed tags PUT used to return before Create set any state, so Terraform
// forgot the floating IP and the next apply allocated another.
func TestFloatingIPCreateKeepsStateWhenTagsFail(t *testing.T) {
	t.Parallel()
	routes := floatingIPRoutes()
	routes["PUT "+floatingIPItemPath+"/tags"] = badGateway
	r := &floatingIPResource{config: newFakeNeutron(t, routes).config}

	resp := runCreate(r, floatingIPPlan(t, r, []string{"workload"}))
	if !resp.Diagnostics.HasError() {
		t.Fatal("create succeeded; want the 502 on the tags PUT reported")
	}
	if resp.State.Raw.IsNull() {
		t.Fatal("create returned no state: Terraform forgets fip-1 and the next apply allocates a second address")
	}
	if !resp.State.Raw.IsFullyKnown() {
		t.Fatalf("create state holds unknown values, which Terraform saves as null: %v", resp.State.Raw)
	}
	if got := floatingIPRow(t, resp.State); got.ID.ValueString() != floatingIPID {
		t.Fatalf("create state id = %s, want fip-1", got.ID)
	}
}

// A read-back that finds the floating IP gone right after the POST used to be
// swallowed: Create reported nothing and Terraform saved a row with no ID.
// Create must report it and leave nothing in state.
func TestFloatingIPCreateDropsStateWhenReadBack404s(t *testing.T) {
	t.Parallel()
	routes := floatingIPRoutes()
	routes["GET "+floatingIPItemPath] = reply(http.StatusNotFound,
		`{"NeutronError": {"type": "FloatingIPNotFound", "message": "Floating IP fip-1 could not be found", "detail": ""}}`)
	r := &floatingIPResource{config: newFakeNeutron(t, routes).config}

	resp := runCreate(r, floatingIPPlan(t, r, nil))
	if !resp.Diagnostics.HasError() {
		t.Fatal("create succeeded; want the 404 read-back reported")
	}
	if !resp.State.Raw.IsNull() {
		t.Fatalf("create left a row for a floating IP the read-back found gone: %v", resp.State.Raw)
	}
}

// An update whose read-back fails must return the error and keep the prior
// state, which the next plan compares against. Update used to write the plan,
// including the fixed_ip, router_id and status ModifyPlan marks unknown when
// the association changes, which Terraform refuses.
func TestFloatingIPUpdateKeepsStateWhenReadBackFails(t *testing.T) {
	t.Parallel()
	routes := floatingIPRoutes()
	routes["GET "+floatingIPItemPath] = badGateway
	neutron := newFakeNeutron(t, routes)
	r := &floatingIPResource{config: neutron.config}

	plan, priorState := floatingIPPortChange(t, r)
	resp := runUpdate(r, plan, priorState)
	if !resp.Diagnostics.HasError() {
		t.Fatal("update succeeded; want the 502 on the read-back reported")
	}
	if !resp.State.Raw.Equal(priorState.Raw) {
		t.Fatalf("update state = %v, want the prior state %v", resp.State.Raw, priorState.Raw)
	}
	want := []string{"PUT " + floatingIPItemPath, "GET " + floatingIPItemPath}
	if sent := neutron.received(); !slices.Equal(sent, want) {
		t.Fatalf("update sent %v, want %v", sent, want)
	}
}

// An update whose read-back finds the floating IP gone must report it and
// keep the prior state, so the next refresh drops the row. Update used to
// report nothing and write the plan's unknowns.
func TestFloatingIPUpdateKeepsStateWhenReadBack404s(t *testing.T) {
	t.Parallel()
	routes := floatingIPRoutes()
	routes["GET "+floatingIPItemPath] = reply(http.StatusNotFound,
		`{"NeutronError": {"type": "FloatingIPNotFound", "message": "Floating IP fip-1 could not be found", "detail": ""}}`)
	r := &floatingIPResource{config: newFakeNeutron(t, routes).config}

	plan, priorState := floatingIPPortChange(t, r)
	resp := runUpdate(r, plan, priorState)
	if !resp.Diagnostics.HasError() {
		t.Fatal("update succeeded; want the 404 read-back reported")
	}
	if !resp.State.Raw.Equal(priorState.Raw) {
		t.Fatalf("update state = %v, want the prior state %v", resp.State.Raw, priorState.Raw)
	}
}
