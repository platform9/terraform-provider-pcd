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
	fipAssocFloatingIPID = "fip-1"
	fipAssocPortID       = "port-1"
	fipAssocFixedIP      = "10.0.0.5"

	floatingIPsPath = "/v2.0/floatingips"
	floatingIPPath  = floatingIPsPath + "/" + fipAssocFloatingIPID
)

// fipAssocJSON is fip-1 as Neutron returns it while it is associated with
// portID at fixedIP.
func fipAssocJSON(portID, fixedIP string) string {
	return `{"id": "fip-1", "floating_ip_address": "203.0.113.10", "floating_network_id": "ext-net-1",
	"port_id": "` + portID + `", "fixed_ip_address": "` + fixedIP + `", "router_id": "router-1",
	"status": "ACTIVE", "description": "", "tenant_id": "proj-1", "project_id": "proj-1", "tags": [],
	"revision_number": 2, "created_at": "2026-10-02T00:00:00Z", "updated_at": "2026-10-02T00:00:00Z"}`
}

// fipAssocRoutes answers every call floatingIPAssociateResource makes for fip-1
// the way Neutron does, with fip-1 associated with port-1. Each test replaces
// the route whose failure it exercises.
func fipAssocRoutes() neutronRoutes {
	body := `{"floatingip": ` + fipAssocJSON(fipAssocPortID, fipAssocFixedIP) + `}`
	return neutronRoutes{
		"PUT " + floatingIPPath: reply(http.StatusOK, body),
		"GET " + floatingIPPath: reply(http.StatusOK, body),
	}
}

// fipAssocPlan is the plan the framework computes for a new association whose
// config sets floating_ip_id and port_id. id, fixed_ip and region are unknown.
func fipAssocPlan(t *testing.T, r *floatingIPAssociateResource) tfsdk.Plan {
	t.Helper()
	return newPlan(t, schemaOf(t, r), &floatingIPAssociateModel{
		ID:           types.StringUnknown(),
		FloatingIPID: types.StringValue(fipAssocFloatingIPID),
		PortID:       types.StringValue(fipAssocPortID),
		FixedIP:      types.StringUnknown(),
		Region:       types.StringUnknown(),
	})
}

// fipAssocRowWithoutID is the row provider v0.1.14 and earlier left in state
// when the read-back after a successful association failed: Terraform saved the
// unknowns Create returned as null, the ID among them. floating_ip_id and
// port_id came from the config, so they survived.
func fipAssocRowWithoutID(t *testing.T, r *floatingIPAssociateResource) tfsdk.State {
	t.Helper()
	return newState(t, schemaOf(t, r), &floatingIPAssociateModel{
		ID:           types.StringNull(),
		FloatingIPID: types.StringValue(fipAssocFloatingIPID),
		PortID:       types.StringValue(fipAssocPortID),
		FixedIP:      types.StringNull(),
		Region:       types.StringNull(),
	})
}

// fipAssocRowWithoutFloatingIPID is a row whose floating_ip_id is empty, so it
// names no floating IP. ImportState writes a row like this for an empty
// import ID.
func fipAssocRowWithoutFloatingIPID(t *testing.T, r *floatingIPAssociateResource) tfsdk.State {
	t.Helper()
	return newState(t, schemaOf(t, r), &floatingIPAssociateModel{
		ID:           types.StringValue(""),
		FloatingIPID: types.StringValue(""),
		PortID:       types.StringNull(),
		FixedIP:      types.StringNull(),
		Region:       types.StringNull(),
	})
}

// fipAssocRow decodes state into the resource's model.
func fipAssocRow(t *testing.T, state tfsdk.State) floatingIPAssociateModel {
	t.Helper()
	var m floatingIPAssociateModel
	if d := state.Get(context.Background(), &m); d.HasError() {
		t.Fatalf("reading the state: %v", d)
	}
	return m
}

// A create whose PUT and read-back succeed must return the association fully
// known.
func TestFloatingIPAssociateCreateFillsEveryAttribute(t *testing.T) {
	t.Parallel()
	neutron := newFakeNeutron(t, fipAssocRoutes())
	r := &floatingIPAssociateResource{config: neutron.config}

	resp := runCreate(r, fipAssocPlan(t, r))
	if resp.Diagnostics.HasError() {
		t.Fatalf("create: %v", resp.Diagnostics)
	}
	if !resp.State.Raw.IsFullyKnown() {
		t.Fatalf("create state holds unknown values, which Terraform refuses: %v", resp.State.Raw)
	}
	got := fipAssocRow(t, resp.State)
	if got.ID.ValueString() != fipAssocFloatingIPID || got.FixedIP.ValueString() != fipAssocFixedIP ||
		got.Region.ValueString() != "region-one" {
		t.Fatalf("create state id=%s fixed_ip=%s region=%s; want fip-1, 10.0.0.5, region-one",
			got.ID, got.FixedIP, got.Region)
	}
	want := []string{"PUT " + floatingIPPath, "GET " + floatingIPPath}
	if sent := neutron.received(); !slices.Equal(sent, want) {
		t.Fatalf("create sent %v, want %v", sent, want)
	}
}

// Neutron keeps an association whose read-back fails. Create used to write the
// plan's unknown id, fixed_ip and region, which Terraform refuses and saves as
// null. Create must return the error with the association's ID in state
// (Terraform then taints it), and a refresh must fill in what the read-back
// would have.
func TestFloatingIPAssociateCreateKeepsStateWhenReadBackFails(t *testing.T) {
	t.Parallel()
	routes := fipAssocRoutes()
	routes["GET "+floatingIPPath] = badGatewayFirst(routes["GET "+floatingIPPath])
	r := &floatingIPAssociateResource{config: newFakeNeutron(t, routes).config}

	createResp := runCreate(r, fipAssocPlan(t, r))
	if !createResp.Diagnostics.HasError() {
		t.Fatal("create succeeded; want the 502 on the read-back reported")
	}
	if createResp.State.Raw.IsNull() {
		t.Fatal("create returned no state: Terraform forgets the association fip-1 has")
	}
	if !createResp.State.Raw.IsFullyKnown() {
		t.Fatalf("create state holds unknown values, which Terraform saves as null: %v", createResp.State.Raw)
	}
	got := fipAssocRow(t, createResp.State)
	if got.ID.ValueString() != fipAssocFloatingIPID || got.FloatingIPID.ValueString() != fipAssocFloatingIPID {
		t.Fatalf("create state id=%s floating_ip_id=%s; want fip-1 for both", got.ID, got.FloatingIPID)
	}

	// The replacing apply, or a destroy, refreshes the tainted association
	// first.
	readResp := runRead(r, createResp.State)
	if readResp.Diagnostics.HasError() {
		t.Fatalf("refresh of the recorded association: %v", readResp.Diagnostics)
	}
	if readResp.State.Raw.IsNull() {
		t.Fatal("refresh dropped the association from state")
	}
	got = fipAssocRow(t, readResp.State)
	if got.ID.ValueString() != fipAssocFloatingIPID || got.PortID.ValueString() != fipAssocPortID ||
		got.FixedIP.ValueString() != fipAssocFixedIP || got.Region.ValueString() != "region-one" {
		t.Fatalf("refreshed id=%s port_id=%s fixed_ip=%s region=%s; want fip-1, port-1, 10.0.0.5, region-one",
			got.ID, got.PortID, got.FixedIP, got.Region)
	}
}

// Create records the association before the read-back, so a read-back that
// finds the floating IP gone must take that row out again: Create must report
// it and leave nothing in state.
func TestFloatingIPAssociateCreateDropsStateWhenReadBack404s(t *testing.T) {
	t.Parallel()
	routes := fipAssocRoutes()
	routes["GET "+floatingIPPath] = reply(http.StatusNotFound,
		`{"NeutronError": {"type": "FloatingIPNotFound", "message": "Floating IP fip-1 could not be found", "detail": ""}}`)
	r := &floatingIPAssociateResource{config: newFakeNeutron(t, routes).config}

	resp := runCreate(r, fipAssocPlan(t, r))
	if !resp.Diagnostics.HasError() {
		t.Fatal("create succeeded; want the 404 read-back reported")
	}
	if !resp.State.Raw.IsNull() {
		t.Fatalf("create left a row for a floating IP the read-back found gone: %v", resp.State.Raw)
	}
}

// The row v0.1.14 and earlier left has no id, but its floating_ip_id still
// names the floating IP, and Read keys on floating_ip_id. Read must refresh that
// row and fill its id in, not drop it as a row that names nothing.
func TestFloatingIPAssociateReadFillsTheIDOfARowWithoutOne(t *testing.T) {
	t.Parallel()
	neutron := newFakeNeutron(t, fipAssocRoutes())
	r := &floatingIPAssociateResource{config: neutron.config}

	resp := runRead(r, fipAssocRowWithoutID(t, r))
	if len(resp.Diagnostics) != 0 {
		t.Fatalf("diagnostics = %v, want none", resp.Diagnostics)
	}
	if resp.State.Raw.IsNull() {
		t.Fatal("refresh dropped a row whose floating_ip_id names fip-1")
	}
	got := fipAssocRow(t, resp.State)
	if got.ID.ValueString() != fipAssocFloatingIPID || got.FixedIP.ValueString() != fipAssocFixedIP ||
		got.Region.ValueString() != "region-one" {
		t.Fatalf("refreshed id=%s fixed_ip=%s region=%s; want fip-1, 10.0.0.5, region-one",
			got.ID, got.FixedIP, got.Region)
	}
	if sent := neutron.received(); !slices.Equal(sent, []string{"GET " + floatingIPPath}) {
		t.Fatalf("refresh sent %v, want only GET %s", sent, floatingIPPath)
	}
}

// A row with an empty floating_ip_id names no floating IP. Read used to send
// GET for the empty ID, which reaches the collection URL. Read must drop the
// row with a warning, without sending anything.
func TestFloatingIPAssociateReadDropsARowWithNoFloatingIPID(t *testing.T) {
	t.Parallel()
	routes := fipAssocRoutes()
	routes["GET "+floatingIPsPath+"/"] = reply(http.StatusOK,
		`{"floatingips": [`+fipAssocJSON(fipAssocPortID, fipAssocFixedIP)+`]}`)
	neutron := newFakeNeutron(t, routes)
	r := &floatingIPAssociateResource{config: neutron.config}

	resp := runRead(r, fipAssocRowWithoutFloatingIPID(t, r))
	if sent := neutron.received(); len(sent) != 0 {
		t.Fatalf("refresh of a row with no floating_ip_id sent %v; it names nothing to read", sent)
	}
	if resp.Diagnostics.HasError() || resp.Diagnostics.WarningsCount() != 1 {
		t.Fatalf("diagnostics = %v, want exactly one warning", resp.Diagnostics)
	}
	if !resp.State.Raw.IsNull() {
		t.Fatalf("refresh kept a row with no floating_ip_id: %v", resp.State.Raw)
	}
}

// A 200 for the floating IP's own URL whose body holds no floatingip object
// makes gophercloud return a zeroed floating IP and no error, and its empty
// port_id reads as a lost association. readInto must report it as an error,
// and not as not-found: that would drop an association that exists from state.
func TestFloatingIPAssociateReadIntoRefusesAnAnswerWithoutTheFloatingIP(t *testing.T) {
	t.Parallel()
	routes := fipAssocRoutes()
	routes["GET "+floatingIPPath] = reply(http.StatusOK,
		`{"floatingips": [`+fipAssocJSON(fipAssocPortID, fipAssocFixedIP)+`]}`)
	r := &floatingIPAssociateResource{config: newFakeNeutron(t, routes).config}
	client, err := r.config.NetworkV2Client()
	if err != nil {
		t.Fatalf("building the fake Neutron client: %v", err)
	}

	m := floatingIPAssociateModel{Region: types.StringNull()}
	notFound, diags := r.readInto(context.Background(), client, fipAssocFloatingIPID, &m)
	if notFound {
		t.Fatal("readInto reported fip-1 not found; the next refresh would drop an association that exists")
	}
	if !diags.HasError() {
		t.Fatalf("readInto accepted an answer without the floating IP: diagnostics %v", diags)
	}
}

// An update whose read-back fails must return the error and keep the prior
// state, which the next plan compares against. Update used to write the plan
// over it.
func TestFloatingIPAssociateUpdateKeepsStateWhenReadBackFails(t *testing.T) {
	t.Parallel()
	routes := fipAssocRoutes()
	routes["PUT "+floatingIPPath] = reply(http.StatusOK, `{"floatingip": `+fipAssocJSON("port-2", "10.0.1.5")+`}`)
	routes["GET "+floatingIPPath] = badGateway
	r := &floatingIPAssociateResource{config: newFakeNeutron(t, routes).config}
	s := schemaOf(t, r)

	prior := floatingIPAssociateModel{
		ID:           types.StringValue(fipAssocFloatingIPID),
		FloatingIPID: types.StringValue(fipAssocFloatingIPID),
		PortID:       types.StringValue(fipAssocPortID),
		FixedIP:      types.StringValue(fipAssocFixedIP),
		Region:       types.StringValue("region-one"),
	}
	// A move to port-2, with fixed_ip configured for it.
	planned := prior
	planned.PortID = types.StringValue("port-2")
	planned.FixedIP = types.StringValue("10.0.1.5")
	priorState := newState(t, s, &prior)

	resp := runUpdate(r, newPlan(t, s, &planned), priorState)
	if !resp.Diagnostics.HasError() {
		t.Fatal("update succeeded; want the 502 on the read-back reported")
	}
	if !resp.State.Raw.Equal(priorState.Raw) {
		t.Fatalf("update state = %v, want the prior state %v", resp.State.Raw, priorState.Raw)
	}
}

// terraform destroy -refresh=false reaches Delete without Read dropping such a
// row first. Delete used to send the disassociating PUT for the empty ID, which
// reaches the collection URL, and Neutron refuses that. Delete must warn and
// send nothing, so Terraform forgets the row.
func TestFloatingIPAssociateDeleteSkipsARowWithNoFloatingIPID(t *testing.T) {
	t.Parallel()
	routes := fipAssocRoutes()
	routes["PUT "+floatingIPsPath+"/"] = reply(http.StatusMethodNotAllowed, "")
	neutron := newFakeNeutron(t, routes)
	r := &floatingIPAssociateResource{config: neutron.config}

	resp := runDelete(r, fipAssocRowWithoutFloatingIPID(t, r))
	if sent := neutron.received(); len(sent) != 0 {
		t.Fatalf("delete of a row with no floating_ip_id sent %v; it names nothing to disassociate", sent)
	}
	if resp.Diagnostics.HasError() || resp.Diagnostics.WarningsCount() != 1 {
		t.Fatalf("diagnostics = %v, want exactly one warning", resp.Diagnostics)
	}
}

// terraform destroy -refresh=false also reaches Delete with the row v0.1.14 and
// earlier left as it is: no id, but a floating_ip_id that names fip-1, and
// Delete keys on floating_ip_id. Delete must disassociate fip-1, not skip the
// row as one that names nothing: Terraform would then forget an association
// that exists.
func TestFloatingIPAssociateDeleteDisassociatesARowWithoutID(t *testing.T) {
	t.Parallel()
	routes := fipAssocRoutes()
	routes["PUT "+floatingIPPath] = reply(http.StatusOK, `{"floatingip": {"id": "fip-1",
		"floating_ip_address": "203.0.113.10", "floating_network_id": "ext-net-1", "port_id": null,
		"fixed_ip_address": null, "router_id": null, "status": "ACTIVE", "description": "",
		"tenant_id": "proj-1", "project_id": "proj-1", "tags": [], "revision_number": 3,
		"created_at": "2026-10-02T00:00:00Z", "updated_at": "2026-10-02T00:00:00Z"}}`)
	neutron := newFakeNeutron(t, routes)
	r := &floatingIPAssociateResource{config: neutron.config}

	resp := runDelete(r, fipAssocRowWithoutID(t, r))
	if len(resp.Diagnostics) != 0 {
		t.Fatalf("diagnostics = %v, want none", resp.Diagnostics)
	}
	if sent := neutron.received(); !slices.Equal(sent, []string{"PUT " + floatingIPPath}) {
		t.Fatalf("delete sent %v, want only PUT %s", sent, floatingIPPath)
	}
}
