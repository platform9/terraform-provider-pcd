// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package networking

import (
	"context"
	"net/http"
	"slices"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

const (
	routeDestination = "10.30.0.0/24"
	routeNextHop     = "10.20.0.254"

	routeRouterPath = "/v2.0/routers/router-1"
	routeSubnetPath = "/v2.0/subnets/subnet-1"
)

const (
	// otherRouteJSON is a route that another configuration manages.
	otherRouteJSON = `{"destination": "10.40.0.0/24", "nexthop": "10.20.0.253"}`
	// addedRouteJSON is the route the tests add.
	addedRouteJSON = `{"destination": "10.30.0.0/24", "nexthop": "10.20.0.254"}`
)

// routeRouterJSON is router-1 as Neutron returns it, holding routes, a
// comma-separated list of route objects.
func routeRouterJSON(routes string) string {
	return `{"id": "router-1", "name": "workload-router", "description": "",
	"status": "ACTIVE", "admin_state_up": true, "distributed": false, "ha": false,
	"tenant_id": "proj-1", "project_id": "proj-1", "revision_number": 3,
	"routes": [` + routes + `]}`
}

// routeSubnetJSON is subnet-1 as Neutron returns it, holding routes as its
// host routes.
func routeSubnetJSON(routes string) string {
	return `{"id": "subnet-1", "network_id": "net-1", "name": "workload-subnet",
	"description": "", "ip_version": 4, "cidr": "10.20.0.0/24", "gateway_ip": "10.20.0.1",
	"enable_dhcp": true, "dns_nameservers": [], "allocation_pools": [{"start": "10.20.0.2", "end": "10.20.0.250"}],
	"tenant_id": "proj-1", "project_id": "proj-1", "revision_number": 2,
	"created_at": "2026-10-02T00:00:00Z", "updated_at": "2026-10-02T00:00:00Z",
	"host_routes": [` + routes + `]}`
}

// routeRouterListJSON and routeSubnetListJSON are what Neutron answers for
// the routers and subnets collection URLs: a list, here of the one parent,
// holding both routes.
var (
	routeRouterListJSON = `{"routers": [` + routeRouterJSON(otherRouteJSON+", "+addedRouteJSON) + `]}`
	routeSubnetListJSON = `{"subnets": [` + routeSubnetJSON(otherRouteJSON+", "+addedRouteJSON) + `]}`
)

// routerRoutePlan is the plan for a route on routerID whose config leaves
// region unset.
func routerRoutePlan(t *testing.T, r *routerRouteResource, routerID string) tfsdk.Plan {
	t.Helper()
	return newPlan(t, schemaOf(t, r), &routerRouteModel{
		ID:              types.StringUnknown(),
		RouterID:        types.StringValue(routerID),
		DestinationCIDR: types.StringValue(routeDestination),
		NextHop:         types.StringValue(routeNextHop),
		Region:          types.StringUnknown(),
	})
}

// routerRouteRow is the row a create of the route on router-1 saved.
func routerRouteRow(t *testing.T, r *routerRouteResource) tfsdk.State {
	t.Helper()
	return newState(t, schemaOf(t, r), &routerRouteModel{
		ID:              types.StringValue("router-1/" + routeDestination + "/" + routeNextHop),
		RouterID:        types.StringValue("router-1"),
		DestinationCIDR: types.StringValue(routeDestination),
		NextHop:         types.StringValue(routeNextHop),
		Region:          types.StringValue("region-one"),
	})
}

// subnetRoutePlan is the plan for a host route on subnetID whose config leaves
// region unset.
func subnetRoutePlan(t *testing.T, r *subnetRouteResource, subnetID string) tfsdk.Plan {
	t.Helper()
	return newPlan(t, schemaOf(t, r), &subnetRouteModel{
		ID:              types.StringUnknown(),
		SubnetID:        types.StringValue(subnetID),
		DestinationCIDR: types.StringValue(routeDestination),
		NextHop:         types.StringValue(routeNextHop),
		Region:          types.StringUnknown(),
	})
}

// subnetRouteRow is the row a create of the host route on subnet-1 saved.
func subnetRouteRow(t *testing.T, r *subnetRouteResource) tfsdk.State {
	t.Helper()
	return newState(t, schemaOf(t, r), &subnetRouteModel{
		ID:              types.StringValue("subnet-1/" + routeDestination + "/" + routeNextHop),
		SubnetID:        types.StringValue("subnet-1"),
		DestinationCIDR: types.StringValue(routeDestination),
		NextHop:         types.StringValue(routeNextHop),
		Region:          types.StringValue("region-one"),
	})
}

// refusesEmptyString reports whether the validators of the string attribute
// name in r's schema refuse "".
func refusesEmptyString(t *testing.T, r resource.Resource, name string) bool {
	t.Helper()
	attr, ok := schemaOf(t, r).Attributes[name].(schema.StringAttribute)
	if !ok {
		t.Fatalf("%s is not a string attribute", name)
	}
	req := validator.StringRequest{Path: path.Root(name), ConfigValue: types.StringValue("")}
	var resp validator.StringResponse
	for _, v := range attr.StringValidators() {
		v.ValidateString(context.Background(), req, &resp)
	}
	return resp.Diagnostics.HasError()
}

// A route on a router that exists is added next to the routes already there,
// and the create returns it fully known.
func TestRouterRouteCreateAddsTheRoute(t *testing.T) {
	t.Parallel()
	neutron := newFakeNeutron(t, neutronRoutes{
		"GET " + routeRouterPath: reply(http.StatusOK, `{"router": `+routeRouterJSON(otherRouteJSON)+`}`),
		"PUT " + routeRouterPath: reply(http.StatusOK, `{"router": `+routeRouterJSON(otherRouteJSON+", "+addedRouteJSON)+`}`),
	})
	r := &routerRouteResource{config: neutron.config}

	resp := runCreate(r, routerRoutePlan(t, r, "router-1"))
	if resp.Diagnostics.HasError() {
		t.Fatalf("create: %v", resp.Diagnostics)
	}
	if !resp.State.Raw.IsFullyKnown() {
		t.Fatalf("create state holds unknown values, which Terraform refuses: %v", resp.State.Raw)
	}
	var got routerRouteModel
	if d := resp.State.Get(context.Background(), &got); d.HasError() {
		t.Fatalf("reading the create state: %v", d)
	}
	if want := "router-1/" + routeDestination + "/" + routeNextHop; got.ID.ValueString() != want {
		t.Fatalf("create state id = %s, want %s", got.ID, want)
	}
	if sent, want := neutron.received(), []string{"GET " + routeRouterPath, "PUT " + routeRouterPath}; !slices.Equal(sent, want) {
		t.Fatalf("create sent %v, want %v", sent, want)
	}
}

// An empty router_id names no router. The schema must refuse it at plan time,
// where the user can fix it.
func TestRouterRouteSchemaRefusesAnEmptyRouterID(t *testing.T) {
	t.Parallel()
	if !refusesEmptyString(t, &routerRouteResource{}, "router_id") {
		t.Fatal(`router_id accepts ""`)
	}
}

// Terraform runs the schema's validator before it applies, but Create must not
// depend on it. Create used to send GET for an empty router_id, which reaches
// the collection URL; Neutron answers that with a list, gophercloud decodes it
// to no router, and the provider crashed. Create must refuse the empty ID
// without sending anything.
func TestRouterRouteCreateRefusesAnEmptyRouterID(t *testing.T) {
	t.Parallel()
	neutron := newFakeNeutron(t, neutronRoutes{
		"GET /v2.0/routers/": reply(http.StatusOK, routeRouterListJSON),
	})
	r := &routerRouteResource{config: neutron.config}

	resp := runCreate(r, routerRoutePlan(t, r, ""))
	if sent := neutron.received(); len(sent) != 0 {
		t.Fatalf("create with an empty router_id sent %v; it names no router", sent)
	}
	if !resp.Diagnostics.HasError() {
		t.Fatal("create succeeded; want the empty router_id reported")
	}
	if !resp.State.Raw.IsNull() {
		t.Fatalf("create left a row for a route it never added: %v", resp.State.Raw)
	}
}

// A 200 for the router's own URL whose body holds no router object makes
// gophercloud return no router and no error. Create must report it, and add
// nothing.
func TestRouterRouteCreateRefusesAnAnswerWithoutTheRouter(t *testing.T) {
	t.Parallel()
	neutron := newFakeNeutron(t, neutronRoutes{
		"GET " + routeRouterPath: reply(http.StatusOK, routeRouterListJSON),
	})
	r := &routerRouteResource{config: neutron.config}

	resp := runCreate(r, routerRoutePlan(t, r, "router-1"))
	if !resp.Diagnostics.HasError() {
		t.Fatalf("create accepted an answer without the router: diagnostics %v", resp.Diagnostics)
	}
	if sent, want := neutron.received(), []string{"GET " + routeRouterPath}; !slices.Equal(sent, want) {
		t.Fatalf("create sent %v, want only %v", sent, want)
	}
}

// The same answer to a refresh must be an error, and not a not-found: that
// would drop a route that exists from state.
func TestRouterRouteReadRefusesAnAnswerWithoutTheRouter(t *testing.T) {
	t.Parallel()
	r := &routerRouteResource{config: newFakeNeutron(t, neutronRoutes{
		"GET " + routeRouterPath: reply(http.StatusOK, routeRouterListJSON),
	}).config}
	row := routerRouteRow(t, r)

	resp := runRead(r, row)
	if !resp.Diagnostics.HasError() {
		t.Fatalf("refresh accepted an answer without the router: diagnostics %v", resp.Diagnostics)
	}
	if !resp.State.Raw.Equal(row.Raw) {
		t.Fatalf("refresh state = %v, want the row unchanged", resp.State.Raw)
	}
}

// The same answer to the read that starts a delete must be an error, and the
// delete must not write the router's routes.
func TestRouterRouteDeleteRefusesAnAnswerWithoutTheRouter(t *testing.T) {
	t.Parallel()
	neutron := newFakeNeutron(t, neutronRoutes{
		"GET " + routeRouterPath: reply(http.StatusOK, routeRouterListJSON),
	})
	r := &routerRouteResource{config: neutron.config}

	resp := runDelete(r, routerRouteRow(t, r))
	if !resp.Diagnostics.HasError() {
		t.Fatalf("delete accepted an answer without the router: diagnostics %v", resp.Diagnostics)
	}
	if sent, want := neutron.received(), []string{"GET " + routeRouterPath}; !slices.Equal(sent, want) {
		t.Fatalf("delete sent %v, want only %v", sent, want)
	}
}

// A host route on a subnet that exists is added next to the host routes
// already there, and the create returns it fully known.
func TestSubnetRouteCreateAddsTheRoute(t *testing.T) {
	t.Parallel()
	neutron := newFakeNeutron(t, neutronRoutes{
		"GET " + routeSubnetPath: reply(http.StatusOK, `{"subnet": `+routeSubnetJSON(otherRouteJSON)+`}`),
		"PUT " + routeSubnetPath: reply(http.StatusOK, `{"subnet": `+routeSubnetJSON(otherRouteJSON+", "+addedRouteJSON)+`}`),
	})
	r := &subnetRouteResource{config: neutron.config}

	resp := runCreate(r, subnetRoutePlan(t, r, "subnet-1"))
	if resp.Diagnostics.HasError() {
		t.Fatalf("create: %v", resp.Diagnostics)
	}
	if !resp.State.Raw.IsFullyKnown() {
		t.Fatalf("create state holds unknown values, which Terraform refuses: %v", resp.State.Raw)
	}
	var got subnetRouteModel
	if d := resp.State.Get(context.Background(), &got); d.HasError() {
		t.Fatalf("reading the create state: %v", d)
	}
	if want := "subnet-1/" + routeDestination + "/" + routeNextHop; got.ID.ValueString() != want {
		t.Fatalf("create state id = %s, want %s", got.ID, want)
	}
	if sent, want := neutron.received(), []string{"GET " + routeSubnetPath, "PUT " + routeSubnetPath}; !slices.Equal(sent, want) {
		t.Fatalf("create sent %v, want %v", sent, want)
	}
}

// An empty subnet_id names no subnet. The schema must refuse it at plan time,
// where the user can fix it.
func TestSubnetRouteSchemaRefusesAnEmptySubnetID(t *testing.T) {
	t.Parallel()
	if !refusesEmptyString(t, &subnetRouteResource{}, "subnet_id") {
		t.Fatal(`subnet_id accepts ""`)
	}
}

// Terraform runs the schema's validator before it applies, but Create must not
// depend on it. Create used to send GET for an empty subnet_id, which reaches
// the collection URL; Neutron answers that with a list, gophercloud decodes it
// to no subnet, and the provider crashed. Create must refuse the empty ID
// without sending anything.
func TestSubnetRouteCreateRefusesAnEmptySubnetID(t *testing.T) {
	t.Parallel()
	neutron := newFakeNeutron(t, neutronRoutes{
		"GET /v2.0/subnets/": reply(http.StatusOK, routeSubnetListJSON),
	})
	r := &subnetRouteResource{config: neutron.config}

	resp := runCreate(r, subnetRoutePlan(t, r, ""))
	if sent := neutron.received(); len(sent) != 0 {
		t.Fatalf("create with an empty subnet_id sent %v; it names no subnet", sent)
	}
	if !resp.Diagnostics.HasError() {
		t.Fatal("create succeeded; want the empty subnet_id reported")
	}
	if !resp.State.Raw.IsNull() {
		t.Fatalf("create left a row for a host route it never added: %v", resp.State.Raw)
	}
}

// A 200 for the subnet's own URL whose body holds no subnet object makes
// gophercloud return no subnet and no error. Create must report it, and add
// nothing.
func TestSubnetRouteCreateRefusesAnAnswerWithoutTheSubnet(t *testing.T) {
	t.Parallel()
	neutron := newFakeNeutron(t, neutronRoutes{
		"GET " + routeSubnetPath: reply(http.StatusOK, routeSubnetListJSON),
	})
	r := &subnetRouteResource{config: neutron.config}

	resp := runCreate(r, subnetRoutePlan(t, r, "subnet-1"))
	if !resp.Diagnostics.HasError() {
		t.Fatalf("create accepted an answer without the subnet: diagnostics %v", resp.Diagnostics)
	}
	if sent, want := neutron.received(), []string{"GET " + routeSubnetPath}; !slices.Equal(sent, want) {
		t.Fatalf("create sent %v, want only %v", sent, want)
	}
}

// The same answer to a refresh must be an error, and not a not-found: that
// would drop a host route that exists from state.
func TestSubnetRouteReadRefusesAnAnswerWithoutTheSubnet(t *testing.T) {
	t.Parallel()
	r := &subnetRouteResource{config: newFakeNeutron(t, neutronRoutes{
		"GET " + routeSubnetPath: reply(http.StatusOK, routeSubnetListJSON),
	}).config}
	row := subnetRouteRow(t, r)

	resp := runRead(r, row)
	if !resp.Diagnostics.HasError() {
		t.Fatalf("refresh accepted an answer without the subnet: diagnostics %v", resp.Diagnostics)
	}
	if !resp.State.Raw.Equal(row.Raw) {
		t.Fatalf("refresh state = %v, want the row unchanged", resp.State.Raw)
	}
}

// The same answer to the read that starts a delete must be an error, and the
// delete must not write the subnet's host routes.
func TestSubnetRouteDeleteRefusesAnAnswerWithoutTheSubnet(t *testing.T) {
	t.Parallel()
	neutron := newFakeNeutron(t, neutronRoutes{
		"GET " + routeSubnetPath: reply(http.StatusOK, routeSubnetListJSON),
	})
	r := &subnetRouteResource{config: neutron.config}

	resp := runDelete(r, subnetRouteRow(t, r))
	if !resp.Diagnostics.HasError() {
		t.Fatalf("delete accepted an answer without the subnet: diagnostics %v", resp.Diagnostics)
	}
	if sent, want := neutron.received(), []string{"GET " + routeSubnetPath}; !slices.Equal(sent, want) {
		t.Fatalf("delete sent %v, want only %v", sent, want)
	}
}
