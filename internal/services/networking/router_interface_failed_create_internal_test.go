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
	routerIfaceRouterID = "router-1"
	routerIfaceSubnetID = "subnet-1"
	routerIfacePortID   = "port-ri-1"

	routerIfaceRouterPath = "/v2.0/routers/" + routerIfaceRouterID
	routerIfacePortsPath  = "/v2.0/ports"
	routerIfacePortPath   = routerIfacePortsPath + "/" + routerIfacePortID
)

// routerIfacePortJSON is port-ri-1 as Neutron returns it: the port
// add_router_interface made for router-1 on subnet-1.
const routerIfacePortJSON = `{"id": "port-ri-1", "name": "", "network_id": "net-1", "admin_state_up": true,
	"status": "ACTIVE", "mac_address": "fa:16:3e:00:00:01",
	"fixed_ips": [{"subnet_id": "subnet-1", "ip_address": "10.0.0.1"}],
	"tenant_id": "proj-1", "project_id": "proj-1", "device_owner": "network:router_interface",
	"device_id": "router-1", "security_groups": [], "tags": [], "revision_number": 2,
	"created_at": "2026-10-02T00:00:00Z", "updated_at": "2026-10-02T00:00:00Z"}`

// routerIfaceInfoJSON is how Neutron answers add_router_interface and
// remove_router_interface for port-ri-1.
const routerIfaceInfoJSON = `{"id": "router-1", "tenant_id": "proj-1", "port_id": "port-ri-1",
	"network_id": "net-1", "subnet_id": "subnet-1", "subnet_ids": ["subnet-1"]}`

// routerIfaceRoutes answers every call routerInterfaceResource makes for
// port-ri-1 the way Neutron does. Each test replaces the route whose failure it
// exercises.
func routerIfaceRoutes() neutronRoutes {
	return neutronRoutes{
		"PUT " + routerIfaceRouterPath + "/add_router_interface":    reply(http.StatusOK, routerIfaceInfoJSON),
		"GET " + routerIfacePortPath:                                reply(http.StatusOK, `{"port": `+routerIfacePortJSON+`}`),
		"PUT " + routerIfaceRouterPath + "/remove_router_interface": reply(http.StatusOK, routerIfaceInfoJSON),
	}
}

// routerIfacePlan is the plan the framework computes for a new interface whose
// config sets router_id and subnet_id. id, port_id and region are unknown.
func routerIfacePlan(t *testing.T, r *routerInterfaceResource) tfsdk.Plan {
	t.Helper()
	return newPlan(t, schemaOf(t, r), &routerInterfaceModel{
		ID:       types.StringUnknown(),
		RouterID: types.StringValue(routerIfaceRouterID),
		SubnetID: types.StringValue(routerIfaceSubnetID),
		PortID:   types.StringUnknown(),
		Region:   types.StringUnknown(),
	})
}

// routerIfaceState is port-ri-1 as a successful create leaves it in state.
func routerIfaceState(t *testing.T, r *routerInterfaceResource) tfsdk.State {
	t.Helper()
	return newState(t, schemaOf(t, r), &routerInterfaceModel{
		ID:       types.StringValue(routerIfacePortID),
		RouterID: types.StringValue(routerIfaceRouterID),
		SubnetID: types.StringValue(routerIfaceSubnetID),
		PortID:   types.StringValue(routerIfacePortID),
		Region:   types.StringValue("region-one"),
	})
}

// routerIfaceRow decodes state into the resource's model.
func routerIfaceRow(t *testing.T, state tfsdk.State) routerInterfaceModel {
	t.Helper()
	var m routerInterfaceModel
	if d := state.Get(context.Background(), &m); d.HasError() {
		t.Fatalf("reading the state: %v", d)
	}
	return m
}

// A create whose add_router_interface and read-back succeed must return the
// interface fully known.
func TestRouterInterfaceCreateFillsEveryAttribute(t *testing.T) {
	t.Parallel()
	neutron := newFakeNeutron(t, routerIfaceRoutes())
	r := &routerInterfaceResource{config: neutron.config}

	resp := runCreate(r, routerIfacePlan(t, r))
	if resp.Diagnostics.HasError() {
		t.Fatalf("create: %v", resp.Diagnostics)
	}
	if !resp.State.Raw.IsFullyKnown() {
		t.Fatalf("create state holds unknown values, which Terraform refuses: %v", resp.State.Raw)
	}
	got := routerIfaceRow(t, resp.State)
	if got.ID.ValueString() != routerIfacePortID || got.PortID.ValueString() != routerIfacePortID ||
		got.SubnetID.ValueString() != routerIfaceSubnetID || got.Region.ValueString() != "region-one" {
		t.Fatalf("create state id=%s port_id=%s subnet_id=%s region=%s; want port-ri-1, port-ri-1, subnet-1, region-one",
			got.ID, got.PortID, got.SubnetID, got.Region)
	}
	want := []string{"PUT " + routerIfaceRouterPath + "/add_router_interface", "GET " + routerIfacePortPath}
	if sent := neutron.received(); !slices.Equal(sent, want) {
		t.Fatalf("create sent %v, want %v", sent, want)
	}
}

// Neutron keeps an interface whose read-back fails. Create used to swallow the
// read-back's error and write the plan's unknown port_id and region, so the
// failure itself was never shown. Create must return the error with the
// interface's ID in state (Terraform then taints it), and a refresh must fill
// in what the read-back would have.
func TestRouterInterfaceCreateKeepsStateWhenReadBackFails(t *testing.T) {
	t.Parallel()
	routes := routerIfaceRoutes()
	routes["GET "+routerIfacePortPath] = badGatewayFirst(routes["GET "+routerIfacePortPath])
	r := &routerInterfaceResource{config: newFakeNeutron(t, routes).config}

	createResp := runCreate(r, routerIfacePlan(t, r))
	if !createResp.Diagnostics.HasError() {
		t.Fatal("create succeeded; want the 502 on the read-back reported")
	}
	if createResp.State.Raw.IsNull() {
		t.Fatal("create returned no state: Terraform forgets port-ri-1, and the next apply's add_router_interface fails on it")
	}
	if !createResp.State.Raw.IsFullyKnown() {
		t.Fatalf("create state holds unknown values, which Terraform saves as null: %v", createResp.State.Raw)
	}
	if got := routerIfaceRow(t, createResp.State); got.ID.ValueString() != routerIfacePortID {
		t.Fatalf("create state id = %s, want port-ri-1", got.ID)
	}

	// The replacing apply, or a destroy, refreshes the tainted interface first.
	readResp := runRead(r, createResp.State)
	if readResp.Diagnostics.HasError() {
		t.Fatalf("refresh of the recorded interface: %v", readResp.Diagnostics)
	}
	if readResp.State.Raw.IsNull() {
		t.Fatal("refresh dropped port-ri-1 from state")
	}
	got := routerIfaceRow(t, readResp.State)
	if got.ID.ValueString() != routerIfacePortID || got.PortID.ValueString() != routerIfacePortID ||
		got.RouterID.ValueString() != routerIfaceRouterID || got.Region.ValueString() != "region-one" {
		t.Fatalf("refreshed id=%s port_id=%s router_id=%s region=%s; want port-ri-1, port-ri-1, router-1, region-one",
			got.ID, got.PortID, got.RouterID, got.Region)
	}
}

// Create records the interface before the read-back, so a read-back that finds
// the port gone must take that row out again: Create must report it and leave
// nothing in state.
func TestRouterInterfaceCreateDropsStateWhenReadBack404s(t *testing.T) {
	t.Parallel()
	routes := routerIfaceRoutes()
	routes["GET "+routerIfacePortPath] = reply(http.StatusNotFound,
		`{"NeutronError": {"type": "PortNotFound", "message": "Port port-ri-1 could not be found.", "detail": ""}}`)
	r := &routerInterfaceResource{config: newFakeNeutron(t, routes).config}

	resp := runCreate(r, routerIfacePlan(t, r))
	if !resp.Diagnostics.HasError() {
		t.Fatal("create succeeded; want the 404 read-back reported")
	}
	if !resp.State.Raw.IsNull() {
		t.Fatalf("create left a row for a port the read-back found gone: %v", resp.State.Raw)
	}
}

// A row with no ID names no port. Read used to send GET for the empty ID, which
// reaches the collection URL, and saved the zeroed port gophercloud decoded
// from Neutron's list as id "". Read must drop the row with a warning, without
// sending anything.
func TestRouterInterfaceReadDropsARowWithNoID(t *testing.T) {
	t.Parallel()
	routes := routerIfaceRoutes()
	routes["GET "+routerIfacePortsPath+"/"] = reply(http.StatusOK, `{"ports": [`+routerIfacePortJSON+`]}`)
	neutron := newFakeNeutron(t, routes)
	r := &routerInterfaceResource{config: neutron.config}

	row := newState(t, schemaOf(t, r), &routerInterfaceModel{
		ID:       types.StringNull(),
		RouterID: types.StringValue(routerIfaceRouterID),
		SubnetID: types.StringValue(routerIfaceSubnetID),
		PortID:   types.StringNull(),
		Region:   types.StringNull(),
	})
	resp := runRead(r, row)
	if sent := neutron.received(); len(sent) != 0 {
		t.Fatalf("refresh of a row with no ID sent %v; it names nothing to read", sent)
	}
	if resp.Diagnostics.HasError() || resp.Diagnostics.WarningsCount() != 1 {
		t.Fatalf("diagnostics = %v, want exactly one warning", resp.Diagnostics)
	}
	if !resp.State.Raw.IsNull() {
		t.Fatalf("refresh kept a row with no ID: %v", resp.State.Raw)
	}
}

// A refresh whose GET fails used to succeed: readInto swallowed every error but
// a 404, so the failure was never shown. Read must report it and keep the row,
// since the port may well still exist.
func TestRouterInterfaceReadReportsAFailedRead(t *testing.T) {
	t.Parallel()
	routes := routerIfaceRoutes()
	routes["GET "+routerIfacePortPath] = badGateway
	r := &routerInterfaceResource{config: newFakeNeutron(t, routes).config}
	prior := routerIfaceState(t, r)

	resp := runRead(r, prior)
	if !resp.Diagnostics.HasError() {
		t.Fatal("refresh succeeded; want the 502 reported")
	}
	if !resp.State.Raw.Equal(prior.Raw) {
		t.Fatalf("refresh state = %v, want the prior state %v", resp.State.Raw, prior.Raw)
	}
}

// A 200 for the port's own URL whose body holds no port object makes
// gophercloud return a zeroed port and no error. Read used to save it as
// id "". It must report an error instead, and not a not-found: that would drop
// an interface that exists from state.
func TestRouterInterfaceReadRefusesAnAnswerWithoutThePort(t *testing.T) {
	t.Parallel()
	routes := routerIfaceRoutes()
	routes["GET "+routerIfacePortPath] = reply(http.StatusOK, `{"ports": [`+routerIfacePortJSON+`]}`)
	r := &routerInterfaceResource{config: newFakeNeutron(t, routes).config}
	prior := routerIfaceState(t, r)

	resp := runRead(r, prior)
	if !resp.Diagnostics.HasError() {
		t.Fatalf("refresh accepted an answer without the port: state %v", resp.State.Raw)
	}
	if !resp.State.Raw.Equal(prior.Raw) {
		t.Fatalf("refresh state = %v, want the prior state %v", resp.State.Raw, prior.Raw)
	}
}
