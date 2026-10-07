// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package networking

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

const (
	portID   = "port-1"
	portName = "workload-port"

	portsPath = "/v2.0/ports"
	portPath  = portsPath + "/" + portID
)

// portJSON is port-1 as Neutron returns it: a port on net-1 with no device, the
// address Neutron assigned it from the network's subnet, the network's default
// security group, and the tag the tests configure.
const portJSON = `{"id": "port-1", "network_id": "net-1", "name": "workload-port", "description": "",
	"admin_state_up": true, "status": "DOWN", "mac_address": "fa:16:3e:5d:1c:01",
	"fixed_ips": [{"subnet_id": "subnet-1", "ip_address": "10.0.0.10"}],
	"tenant_id": "proj-1", "project_id": "proj-1", "device_id": "", "device_owner": "",
	"security_groups": ["sg-default"], "allowed_address_pairs": [], "tags": ["workload"],
	"propagate_uplink_status": false, "revision_number": 1,
	"created_at": "2026-10-02T00:00:00Z", "updated_at": "2026-10-02T00:00:00Z"}`

// portRoutes answers every call portResource makes for port-1 the way Neutron
// does. Each test replaces the route whose failure it exercises.
func portRoutes() neutronRoutes {
	return neutronRoutes{
		"POST " + portsPath:         reply(http.StatusCreated, `{"port": `+portJSON+`}`),
		"GET " + portPath:           reply(http.StatusOK, `{"port": `+portJSON+`}`),
		"PUT " + portPath:           reply(http.StatusOK, `{"port": `+portJSON+`}`),
		"PUT " + portPath + "/tags": reply(http.StatusOK, `{"tags": ["workload"]}`),
		"DELETE " + portPath:        reply(http.StatusNoContent, ""),
	}
}

// portFixedIPType and portAddrPairType are the element types of fixed_ip and
// allowed_address_pairs. A model needs them to hold either attribute as null.
var (
	portFixedIPType  = types.ObjectType{AttrTypes: map[string]attr.Type{"subnet_id": types.StringType, "ip_address": types.StringType}}
	portAddrPairType = types.ObjectType{AttrTypes: map[string]attr.Type{"ip_address": types.StringType, "mac_address": types.StringType}}
)

// portPlan is the plan the framework computes for a new port whose config sets
// network_id and name, plus tags when given. admin_state_up takes its default
// of true, fixed_ip and allowed_address_pairs stay null, and the Computed
// attributes the config leaves unset are unknown.
func portPlan(t *testing.T, r *portResource, tags []string) tfsdk.Plan {
	t.Helper()
	planned := portModel{
		ID:                  types.StringUnknown(),
		NetworkID:           types.StringValue("net-1"),
		Name:                types.StringValue(portName),
		Description:         types.StringUnknown(),
		AdminStateUp:        types.BoolValue(true),
		MACAddress:          types.StringUnknown(),
		DeviceID:            types.StringUnknown(),
		DeviceOwner:         types.StringUnknown(),
		FixedIP:             types.ListNull(portFixedIPType),
		SecurityGroupIDs:    types.SetUnknown(types.StringType),
		AllowedAddressPairs: types.SetNull(portAddrPairType),
		TenantID:            types.StringUnknown(),
		Tags:                types.SetUnknown(types.StringType),
		Status:              types.StringUnknown(),
		AllFixedIPs:         types.ListUnknown(types.StringType),
		Region:              types.StringUnknown(),
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

// portRowWithoutID is the row provider v0.1.14 left in state when the read-back
// after a successful POST failed: Terraform saved the unknowns Create returned
// as null, the ID among them.
func portRowWithoutID(t *testing.T, r *portResource) tfsdk.State {
	t.Helper()
	return newState(t, schemaOf(t, r), &portModel{
		ID:                  types.StringNull(),
		NetworkID:           types.StringValue("net-1"),
		Name:                types.StringValue(portName),
		Description:         types.StringNull(),
		AdminStateUp:        types.BoolValue(true),
		MACAddress:          types.StringNull(),
		DeviceID:            types.StringNull(),
		DeviceOwner:         types.StringNull(),
		FixedIP:             types.ListNull(portFixedIPType),
		SecurityGroupIDs:    types.SetNull(types.StringType),
		AllowedAddressPairs: types.SetNull(portAddrPairType),
		TenantID:            types.StringNull(),
		Tags:                types.SetNull(types.StringType),
		Status:              types.StringNull(),
		AllFixedIPs:         types.ListNull(types.StringType),
		Region:              types.StringNull(),
	})
}

// portRename returns the state a successful create of port-1 left, and the plan
// the framework computes when the config then renames the port. status and
// all_fixed_ips are Computed with no plan modifier, so any change plans them
// unknown; every other attribute keeps its prior value.
func portRename(t *testing.T, r *portResource) (tfsdk.Plan, tfsdk.State) {
	t.Helper()
	s := schemaOf(t, r)
	prior := portModel{
		ID:                  types.StringValue(portID),
		NetworkID:           types.StringValue("net-1"),
		Name:                types.StringValue(portName),
		Description:         types.StringValue(""),
		AdminStateUp:        types.BoolValue(true),
		MACAddress:          types.StringValue("fa:16:3e:5d:1c:01"),
		DeviceID:            types.StringValue(""),
		DeviceOwner:         types.StringValue(""),
		FixedIP:             types.ListNull(portFixedIPType),
		SecurityGroupIDs:    types.SetValueMust(types.StringType, []attr.Value{types.StringValue("sg-default")}),
		AllowedAddressPairs: types.SetNull(portAddrPairType),
		TenantID:            types.StringValue("proj-1"),
		Tags:                types.SetValueMust(types.StringType, []attr.Value{types.StringValue("workload")}),
		Status:              types.StringValue("DOWN"),
		AllFixedIPs:         types.ListValueMust(types.StringType, []attr.Value{types.StringValue("10.0.0.10")}),
		Region:              types.StringValue("region-one"),
	}
	planned := prior
	planned.Name = types.StringValue(portName + "-renamed")
	planned.Status = types.StringUnknown()
	planned.AllFixedIPs = types.ListUnknown(types.StringType)
	return newPlan(t, s, &planned), newState(t, s, &prior)
}

// portRow returns the port model state holds.
func portRow(t *testing.T, state tfsdk.State) portModel {
	t.Helper()
	var m portModel
	if d := state.Get(context.Background(), &m); d.HasError() {
		t.Fatalf("reading the state: %v", d)
	}
	return m
}

// portAllFixedIPs returns m's all_fixed_ips as strings.
func portAllFixedIPs(t *testing.T, m portModel) []string {
	t.Helper()
	var ips []string
	if d := m.AllFixedIPs.ElementsAs(context.Background(), &ips, false); d.HasError() {
		t.Fatalf("reading all_fixed_ips: %v", d)
	}
	return ips
}

// A create whose every step succeeds must return the port fully known, after
// setting the tags.
func TestPortCreateFillsEveryAttribute(t *testing.T) {
	t.Parallel()
	neutron := newFakeNeutron(t, portRoutes())
	r := &portResource{config: neutron.config}

	resp := runCreate(r, portPlan(t, r, []string{"workload"}))
	if resp.Diagnostics.HasError() {
		t.Fatalf("create: %v", resp.Diagnostics)
	}
	if !resp.State.Raw.IsFullyKnown() {
		t.Fatalf("create state holds unknown values, which Terraform refuses: %v", resp.State.Raw)
	}
	got := portRow(t, resp.State)
	if got.ID.ValueString() != portID || got.TenantID.ValueString() != "proj-1" ||
		got.Status.ValueString() != "DOWN" || got.Region.ValueString() != "region-one" ||
		!slices.Equal(portAllFixedIPs(t, got), []string{"10.0.0.10"}) {
		t.Fatalf("create state id=%s tenant_id=%s status=%s region=%s all_fixed_ips=%s; want port-1, proj-1, DOWN, region-one, [10.0.0.10]",
			got.ID, got.TenantID, got.Status, got.Region, got.AllFixedIPs)
	}
	want := []string{
		"POST " + portsPath,
		"PUT " + portPath + "/tags",
		"GET " + portPath,
	}
	if sent := neutron.received(); !slices.Equal(sent, want) {
		t.Fatalf("create sent %v, want %v", sent, want)
	}
}

// Neutron keeps a port whose read-back fails, and the port keeps the address it
// was given. Create used to write the plan's unknowns, which Terraform saved as
// null, ID included, so the port was orphaned, and the next refresh read the
// collection URL and saved the empty port it decoded from the list as id "".
// Create must return the error with the port's ID in state (Terraform then
// taints it), and a refresh must fill in what the read-back would have.
func TestPortCreateKeepsStateWhenReadBackFails(t *testing.T) {
	t.Parallel()
	routes := portRoutes()
	routes["GET "+portPath] = badGatewayFirst(routes["GET "+portPath])
	r := &portResource{config: newFakeNeutron(t, routes).config}

	createResp := runCreate(r, portPlan(t, r, nil))
	if !createResp.Diagnostics.HasError() {
		t.Fatal("create succeeded; want the 502 on the read-back reported")
	}
	if createResp.State.Raw.IsNull() {
		t.Fatal("create returned no state: Terraform forgets port-1 and the next apply creates a second port")
	}
	if !createResp.State.Raw.IsFullyKnown() {
		t.Fatalf("create state holds unknown values, which Terraform saves as null: %v", createResp.State.Raw)
	}
	if got := portRow(t, createResp.State); got.ID.ValueString() != portID {
		t.Fatalf("create state id = %s, want port-1: a row without one names nothing a destroy could delete", got.ID)
	}

	// The replacing apply, or a destroy, refreshes the tainted port first.
	readResp := runRead(r, createResp.State)
	if readResp.Diagnostics.HasError() {
		t.Fatalf("refresh of the recorded port: %v", readResp.Diagnostics)
	}
	if readResp.State.Raw.IsNull() {
		t.Fatal("refresh dropped port-1 from state")
	}
	got := portRow(t, readResp.State)
	if got.ID.ValueString() != portID || got.TenantID.ValueString() != "proj-1" ||
		got.Status.ValueString() != "DOWN" || got.Region.ValueString() != "region-one" ||
		!slices.Equal(portAllFixedIPs(t, got), []string{"10.0.0.10"}) {
		t.Fatalf("refreshed id=%s tenant_id=%s status=%s region=%s all_fixed_ips=%s; want port-1, proj-1, DOWN, region-one, [10.0.0.10]",
			got.ID, got.TenantID, got.Status, got.Region, got.AllFixedIPs)
	}
}

// A failed tags PUT used to return before Create set any state, so Terraform
// forgot the port and the next apply created another.
func TestPortCreateKeepsStateWhenTagsFail(t *testing.T) {
	t.Parallel()
	routes := portRoutes()
	routes["PUT "+portPath+"/tags"] = badGateway
	r := &portResource{config: newFakeNeutron(t, routes).config}

	resp := runCreate(r, portPlan(t, r, []string{"workload"}))
	if !resp.Diagnostics.HasError() {
		t.Fatal("create succeeded; want the 502 on the tags PUT reported")
	}
	if resp.State.Raw.IsNull() {
		t.Fatal("create returned no state: Terraform forgets port-1 and the next apply creates a second port")
	}
	if !resp.State.Raw.IsFullyKnown() {
		t.Fatalf("create state holds unknown values, which Terraform saves as null: %v", resp.State.Raw)
	}
	if got := portRow(t, resp.State); got.ID.ValueString() != portID {
		t.Fatalf("create state id = %s, want port-1", got.ID)
	}
}

// A read-back that finds the port gone right after the POST used to be
// swallowed: Create reported nothing and Terraform saved a row with no ID.
// Create must report it and leave nothing in state.
func TestPortCreateDropsStateWhenReadBack404s(t *testing.T) {
	t.Parallel()
	routes := portRoutes()
	routes["GET "+portPath] = reply(http.StatusNotFound,
		`{"NeutronError": {"type": "PortNotFound", "message": "Port port-1 could not be found.", "detail": ""}}`)
	r := &portResource{config: newFakeNeutron(t, routes).config}

	resp := runCreate(r, portPlan(t, r, nil))
	if !resp.Diagnostics.HasError() {
		t.Fatal("create succeeded; want the 404 read-back reported")
	}
	if !resp.State.Raw.IsNull() {
		t.Fatalf("create left a row for a port the read-back found gone: %v", resp.State.Raw)
	}
}

// The row v0.1.14 left has no ID. Read used to send GET for an empty ID, which
// reaches the collection URL; Neutron answers that with the list, gophercloud
// decodes an empty port from it, and readInto saved that port back with id "".
// Read must drop the row with a warning that names the port, without sending
// anything.
func TestPortReadDropsARowWithNoID(t *testing.T) {
	t.Parallel()
	routes := portRoutes()
	routes["GET "+portsPath+"/"] = reply(http.StatusOK, `{"ports": [`+portJSON+`]}`)
	neutron := newFakeNeutron(t, routes)
	r := &portResource{config: neutron.config}

	resp := runRead(r, portRowWithoutID(t, r))
	if sent := neutron.received(); len(sent) != 0 {
		t.Fatalf("refresh of a row with no ID sent %v; it names nothing to read", sent)
	}
	if resp.Diagnostics.HasError() || resp.Diagnostics.WarningsCount() != 1 {
		t.Fatalf("diagnostics = %v, want exactly one warning", resp.Diagnostics)
	}
	if detail := resp.Diagnostics[0].Detail(); !strings.Contains(detail, `"workload-port"`) {
		t.Fatalf("warning detail %q does not name the port to look for", detail)
	}
	if !resp.State.Raw.IsNull() {
		t.Fatalf("refresh kept a row with no ID: %v", resp.State.Raw)
	}
}

// A 200 for the port's own URL whose body holds no port object makes gophercloud
// return an empty port and no error, which readInto used to save, id "" and
// all. readInto must report that as an error, and not as not-found: that would
// drop a real port from state.
func TestPortReadIntoRefusesAnAnswerWithoutThePort(t *testing.T) {
	t.Parallel()
	routes := portRoutes()
	routes["GET "+portPath] = reply(http.StatusOK, `{"ports": [`+portJSON+`]}`)
	r := &portResource{config: newFakeNeutron(t, routes).config}
	client, err := r.config.NetworkV2Client()
	if err != nil {
		t.Fatalf("building the fake Neutron client: %v", err)
	}

	m := portModel{Region: types.StringNull()}
	notFound, diags := r.readInto(context.Background(), client, portID, &m)
	if notFound {
		t.Fatal("readInto reported port-1 not found; the next refresh would drop a port that exists")
	}
	if !diags.HasError() {
		t.Fatalf("readInto accepted an answer without the port: diagnostics %v", diags)
	}
}

// An update whose read-back fails must return the error and keep the prior
// state, which the next plan compares against. Update used to write the plan,
// including its unknown status and all_fixed_ips, which Terraform refuses.
func TestPortUpdateKeepsStateWhenReadBackFails(t *testing.T) {
	t.Parallel()
	routes := portRoutes()
	routes["GET "+portPath] = badGateway
	r := &portResource{config: newFakeNeutron(t, routes).config}

	plan, priorState := portRename(t, r)
	resp := runUpdate(r, plan, priorState)
	if !resp.Diagnostics.HasError() {
		t.Fatal("update succeeded; want the 502 on the read-back reported")
	}
	if !resp.State.Raw.Equal(priorState.Raw) {
		t.Fatalf("update state = %v, want the prior state %v", resp.State.Raw, priorState.Raw)
	}
}

// An update whose read-back finds the port gone used to be swallowed: Update
// reported nothing and wrote the plan, including its unknown status and
// all_fixed_ips. It must report the 404 and keep the prior state, so the next
// refresh finds the port gone and drops it.
func TestPortUpdateKeepsStateWhenReadBack404s(t *testing.T) {
	t.Parallel()
	routes := portRoutes()
	routes["GET "+portPath] = reply(http.StatusNotFound,
		`{"NeutronError": {"type": "PortNotFound", "message": "Port port-1 could not be found.", "detail": ""}}`)
	r := &portResource{config: newFakeNeutron(t, routes).config}

	plan, priorState := portRename(t, r)
	resp := runUpdate(r, plan, priorState)
	if !resp.Diagnostics.HasError() {
		t.Fatal("update succeeded; want the 404 read-back reported")
	}
	if !resp.State.Raw.Equal(priorState.Raw) {
		t.Fatalf("update state = %v, want the prior state %v", resp.State.Raw, priorState.Raw)
	}
}

// terraform destroy -refresh=false reaches Delete without Read dropping the
// v0.1.14 row first. Delete used to send DELETE for an empty ID, which reaches
// the collection URL, and Neutron refuses that. Delete must warn and send
// nothing, so Terraform forgets the row.
func TestPortDeleteSkipsARowWithNoID(t *testing.T) {
	t.Parallel()
	routes := portRoutes()
	routes["DELETE "+portsPath+"/"] = reply(http.StatusMethodNotAllowed, "")
	neutron := newFakeNeutron(t, routes)
	r := &portResource{config: neutron.config}

	resp := runDelete(r, portRowWithoutID(t, r))
	if sent := neutron.received(); len(sent) != 0 {
		t.Fatalf("delete of a row with no ID sent %v; it names nothing to delete", sent)
	}
	if resp.Diagnostics.HasError() || resp.Diagnostics.WarningsCount() != 1 {
		t.Fatalf("diagnostics = %v, want exactly one warning", resp.Diagnostics)
	}
}
