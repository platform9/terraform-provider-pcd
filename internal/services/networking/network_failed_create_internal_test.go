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
	networkID   = "net-1"
	networkName = "workload-net"
	networkDesc = "Tenant network for the workload instance"

	networksPath = "/v2.0/networks"
	networkPath  = networksPath + "/" + networkID
)

// networkJSON is net-1 as Neutron returns it, with the tag the tests configure.
const networkJSON = `{"id": "net-1", "name": "workload-net",
	"description": "Tenant network for the workload instance", "admin_state_up": true,
	"status": "ACTIVE", "subnets": [], "shared": false, "router:external": false,
	"port_security_enabled": true, "mtu": 8942, "dns_domain": "",
	"tenant_id": "proj-1", "project_id": "proj-1", "tags": ["workload"], "revision_number": 1,
	"created_at": "2026-10-02T00:00:00Z", "updated_at": "2026-10-02T00:00:00Z"}`

// networkNotFound is Neutron's answer to a GET for a network that does not
// exist.
const networkNotFound = `{"NeutronError": {"type": "NetworkNotFound", "message": "Network net-1 could not be found.", "detail": ""}}`

// networkRoutes answers every call networkResource makes for net-1 the way
// Neutron does. Each test replaces the route whose failure it exercises.
func networkRoutes() neutronRoutes {
	return neutronRoutes{
		"POST " + networksPath:         reply(http.StatusCreated, `{"network": `+networkJSON+`}`),
		"GET " + networkPath:           reply(http.StatusOK, `{"network": `+networkJSON+`}`),
		"PUT " + networkPath:           reply(http.StatusOK, `{"network": `+networkJSON+`}`),
		"PUT " + networkPath + "/tags": reply(http.StatusOK, `{"tags": ["workload"]}`),
		"DELETE " + networkPath:        reply(http.StatusNoContent, ""),
	}
}

// networkNoSegments is the segments value of a network that sets none: a null
// list of the schema's segment object type.
func networkNoSegments() types.List {
	return types.ListNull(types.ObjectType{AttrTypes: map[string]attr.Type{
		"physical_network": types.StringType,
		"network_type":     types.StringType,
		"segmentation_id":  types.Int64Type,
	}})
}

// networkPlan is the plan the framework computes for a new network whose config
// sets name and description, plus tags when given. The Computed attributes the
// config leaves unset are unknown, and admin_state_up and dns_domain take their
// defaults of true and "".
func networkPlan(t *testing.T, r *networkResource, tags []string) tfsdk.Plan {
	t.Helper()
	planned := networkModel{
		ID:           types.StringUnknown(),
		Name:         types.StringValue(networkName),
		Description:  types.StringValue(networkDesc),
		AdminStateUp: types.BoolValue(true),
		Shared:       types.BoolUnknown(),
		External:     types.BoolUnknown(),
		TenantID:     types.StringUnknown(),
		Tags:         types.SetUnknown(types.StringType),
		Region:       types.StringUnknown(),
		Segments:     networkNoSegments(),
		PortSecurity: types.BoolUnknown(),
		DNSDomain:    types.StringValue(""),
		MTU:          types.Int64Unknown(),
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

// networkRowWithoutID is the row provider v0.1.14 left in state when the
// read-back after a successful POST failed: Terraform saved the unknowns
// Create returned as null, the ID among them.
func networkRowWithoutID(t *testing.T, r *networkResource) tfsdk.State {
	t.Helper()
	return newState(t, schemaOf(t, r), &networkModel{
		ID:           types.StringNull(),
		Name:         types.StringValue(networkName),
		Description:  types.StringValue(networkDesc),
		AdminStateUp: types.BoolValue(true),
		Shared:       types.BoolNull(),
		External:     types.BoolNull(),
		TenantID:     types.StringNull(),
		Tags:         types.SetNull(types.StringType),
		Region:       types.StringNull(),
		Segments:     networkNoSegments(),
		PortSecurity: types.BoolNull(),
		DNSDomain:    types.StringValue(""),
		MTU:          types.Int64Null(),
	})
}

// networkPrior is net-1 as a successful create or refresh leaves it in state.
func networkPrior(t *testing.T) networkModel {
	t.Helper()
	tags, d := types.SetValueFrom(context.Background(), types.StringType, []string{"workload"})
	if d.HasError() {
		t.Fatalf("building the tags: %v", d)
	}
	return networkModel{
		ID:           types.StringValue(networkID),
		Name:         types.StringValue(networkName),
		Description:  types.StringValue(networkDesc),
		AdminStateUp: types.BoolValue(true),
		Shared:       types.BoolValue(false),
		External:     types.BoolValue(false),
		TenantID:     types.StringValue("proj-1"),
		Tags:         tags,
		Region:       types.StringValue("region-one"),
		Segments:     networkNoSegments(),
		PortSecurity: types.BoolValue(true),
		DNSDomain:    types.StringValue(""),
		MTU:          types.Int64Value(8942),
	}
}

// networkRenamed is the plan for renaming prior. shared, external,
// port_security_enabled and mtu have no plan modifier, so a config that leaves
// them unset plans them unknown.
func networkRenamed(prior networkModel) networkModel {
	planned := prior
	planned.Name = types.StringValue(networkName + "-renamed")
	planned.Shared = types.BoolUnknown()
	planned.External = types.BoolUnknown()
	planned.PortSecurity = types.BoolUnknown()
	planned.MTU = types.Int64Unknown()
	return planned
}

// networkRow returns the network model state holds.
func networkRow(t *testing.T, state tfsdk.State) networkModel {
	t.Helper()
	var m networkModel
	if d := state.Get(context.Background(), &m); d.HasError() {
		t.Fatalf("reading the state: %v", d)
	}
	return m
}

// A create whose every step succeeds must return the network fully known,
// after setting the tags.
func TestNetworkCreateFillsEveryAttribute(t *testing.T) {
	t.Parallel()
	neutron := newFakeNeutron(t, networkRoutes())
	r := &networkResource{config: neutron.config}

	resp := runCreate(r, networkPlan(t, r, []string{"workload"}))
	if resp.Diagnostics.HasError() {
		t.Fatalf("create: %v", resp.Diagnostics)
	}
	if !resp.State.Raw.IsFullyKnown() {
		t.Fatalf("create state holds unknown values, which Terraform refuses: %v", resp.State.Raw)
	}
	got := networkRow(t, resp.State)
	if got.ID.ValueString() != networkID || got.TenantID.ValueString() != "proj-1" ||
		!got.PortSecurity.ValueBool() || got.MTU.ValueInt64() != 8942 || got.Region.ValueString() != "region-one" {
		t.Fatalf("create state id=%s tenant_id=%s port_security_enabled=%s mtu=%s region=%s; want net-1, proj-1, true, 8942, region-one",
			got.ID, got.TenantID, got.PortSecurity, got.MTU, got.Region)
	}
	want := []string{
		"POST " + networksPath,
		"PUT " + networkPath + "/tags",
		"GET " + networkPath,
	}
	if sent := neutron.received(); !slices.Equal(sent, want) {
		t.Fatalf("create sent %v, want %v", sent, want)
	}
}

// Neutron keeps a network whose read-back fails. Create used to write the
// plan's unknowns, which Terraform saved as null, ID included, so the network
// was orphaned, and the next refresh read the collection URL and saved the
// row back with id "". Create must return the error with the network's ID in
// state (Terraform then taints it), and a refresh must fill in what the
// read-back would have.
func TestNetworkCreateKeepsStateWhenReadBackFails(t *testing.T) {
	t.Parallel()
	routes := networkRoutes()
	routes["GET "+networkPath] = badGatewayFirst(routes["GET "+networkPath])
	r := &networkResource{config: newFakeNeutron(t, routes).config}

	createResp := runCreate(r, networkPlan(t, r, nil))
	if !createResp.Diagnostics.HasError() {
		t.Fatal("create succeeded; want the 502 on the read-back reported")
	}
	if createResp.State.Raw.IsNull() {
		t.Fatal("create returned no state: Terraform forgets net-1 and the next apply creates a second network")
	}
	if !createResp.State.Raw.IsFullyKnown() {
		t.Fatalf("create state holds unknown values, which Terraform saves as null: %v", createResp.State.Raw)
	}
	if got := networkRow(t, createResp.State); got.ID.ValueString() != networkID {
		t.Fatalf("create state id = %s, want net-1: a row without one names nothing a destroy could delete", got.ID)
	}

	// The replacing apply, or a destroy, refreshes the tainted network first.
	readResp := runRead(r, createResp.State)
	if readResp.Diagnostics.HasError() {
		t.Fatalf("refresh of the recorded network: %v", readResp.Diagnostics)
	}
	if readResp.State.Raw.IsNull() {
		t.Fatal("refresh dropped net-1 from state")
	}
	got := networkRow(t, readResp.State)
	if got.ID.ValueString() != networkID || got.TenantID.ValueString() != "proj-1" ||
		!got.PortSecurity.ValueBool() || got.MTU.ValueInt64() != 8942 || got.Region.ValueString() != "region-one" {
		t.Fatalf("refreshed id=%s tenant_id=%s port_security_enabled=%s mtu=%s region=%s; want net-1, proj-1, true, 8942, region-one",
			got.ID, got.TenantID, got.PortSecurity, got.MTU, got.Region)
	}
}

// A failed tags PUT used to return before Create set any state, so Terraform
// forgot the network and the next apply created another.
func TestNetworkCreateKeepsStateWhenTagsFail(t *testing.T) {
	t.Parallel()
	routes := networkRoutes()
	routes["PUT "+networkPath+"/tags"] = badGateway
	r := &networkResource{config: newFakeNeutron(t, routes).config}

	resp := runCreate(r, networkPlan(t, r, []string{"workload"}))
	if !resp.Diagnostics.HasError() {
		t.Fatal("create succeeded; want the 502 on the tags PUT reported")
	}
	if resp.State.Raw.IsNull() {
		t.Fatal("create returned no state: Terraform forgets net-1 and the next apply creates a second network")
	}
	if !resp.State.Raw.IsFullyKnown() {
		t.Fatalf("create state holds unknown values, which Terraform saves as null: %v", resp.State.Raw)
	}
	if got := networkRow(t, resp.State); got.ID.ValueString() != networkID {
		t.Fatalf("create state id = %s, want net-1", got.ID)
	}
}

// A read-back that finds the network gone right after the POST used to be
// swallowed: Create reported nothing and Terraform saved a row with no ID.
// Create must report it and leave nothing in state.
func TestNetworkCreateDropsStateWhenReadBack404s(t *testing.T) {
	t.Parallel()
	routes := networkRoutes()
	routes["GET "+networkPath] = reply(http.StatusNotFound, networkNotFound)
	r := &networkResource{config: newFakeNeutron(t, routes).config}

	resp := runCreate(r, networkPlan(t, r, nil))
	if !resp.Diagnostics.HasError() {
		t.Fatal("create succeeded; want the 404 read-back reported")
	}
	if !resp.State.Raw.IsNull() {
		t.Fatalf("create left a row for a network the read-back found gone: %v", resp.State.Raw)
	}
}

// The row v0.1.14 left has no ID. Read used to send GET for an empty ID, which
// reaches the collection URL; Neutron answers that with the list, gophercloud
// decodes it to a zeroed network, and Read saved the row back with id "" and
// no diagnostic. Read must drop the row with a warning that names the network,
// without sending anything.
func TestNetworkReadDropsARowWithNoID(t *testing.T) {
	t.Parallel()
	routes := networkRoutes()
	routes["GET "+networksPath+"/"] = reply(http.StatusOK, `{"networks": [`+networkJSON+`]}`)
	neutron := newFakeNeutron(t, routes)
	r := &networkResource{config: neutron.config}

	resp := runRead(r, networkRowWithoutID(t, r))
	if sent := neutron.received(); len(sent) != 0 {
		t.Fatalf("refresh of a row with no ID sent %v; it names nothing to read", sent)
	}
	if resp.Diagnostics.HasError() || resp.Diagnostics.WarningsCount() != 1 {
		t.Fatalf("diagnostics = %v, want exactly one warning", resp.Diagnostics)
	}
	if detail := resp.Diagnostics[0].Detail(); !strings.Contains(detail, `"workload-net"`) {
		t.Fatalf("warning detail %q does not name the network to look for", detail)
	}
	if !resp.State.Raw.IsNull() {
		t.Fatalf("refresh kept a row with no ID: %v", resp.State.Raw)
	}
}

// A 200 for the network's own URL whose body holds no network object makes
// gophercloud return a zeroed network and no error. readInto must report that
// as an error, and not as not-found: that would drop a real network from state.
func TestNetworkReadIntoRefusesAnAnswerWithoutTheNetwork(t *testing.T) {
	t.Parallel()
	routes := networkRoutes()
	routes["GET "+networkPath] = reply(http.StatusOK, `{"networks": [`+networkJSON+`]}`)
	r := &networkResource{config: newFakeNeutron(t, routes).config}
	client, err := r.config.NetworkV2Client()
	if err != nil {
		t.Fatalf("building the fake Neutron client: %v", err)
	}

	m := networkModel{Region: types.StringNull()}
	notFound, diags := r.readInto(context.Background(), client, networkID, &m)
	if notFound {
		t.Fatal("readInto reported net-1 not found; the next refresh would drop a network that exists")
	}
	if !diags.HasError() {
		t.Fatalf("readInto accepted an answer without the network: diagnostics %v", diags)
	}
}

// An update whose read-back fails must return the error and keep the prior
// state, which the next plan compares against. Update used to write the plan,
// including its unknown shared, external, port_security_enabled and mtu,
// which Terraform refuses.
func TestNetworkUpdateKeepsStateWhenReadBackFails(t *testing.T) {
	t.Parallel()
	routes := networkRoutes()
	routes["GET "+networkPath] = badGateway
	r := &networkResource{config: newFakeNeutron(t, routes).config}
	s := schemaOf(t, r)

	prior := networkPrior(t)
	planned := networkRenamed(prior)
	priorState := newState(t, s, &prior)

	resp := runUpdate(r, newPlan(t, s, &planned), priorState)
	if !resp.Diagnostics.HasError() {
		t.Fatal("update succeeded; want the 502 on the read-back reported")
	}
	if !resp.State.Raw.Equal(priorState.Raw) {
		t.Fatalf("update state = %v, want the prior state %v", resp.State.Raw, priorState.Raw)
	}
}

// An update whose read-back finds the network gone must report it and keep
// the prior state. Update used to swallow the 404 and write the plan, with its
// unknowns.
func TestNetworkUpdateKeepsStateWhenReadBack404s(t *testing.T) {
	t.Parallel()
	routes := networkRoutes()
	routes["GET "+networkPath] = reply(http.StatusNotFound, networkNotFound)
	r := &networkResource{config: newFakeNeutron(t, routes).config}
	s := schemaOf(t, r)

	prior := networkPrior(t)
	planned := networkRenamed(prior)
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
// the collection URL, and Neutron refuses that; on v0.1.14 the replace of a
// row a refresh had saved with id "" reached it too. Delete must warn and send
// nothing, so Terraform forgets the row.
func TestNetworkDeleteSkipsARowWithNoID(t *testing.T) {
	t.Parallel()
	routes := networkRoutes()
	routes["DELETE "+networksPath+"/"] = reply(http.StatusMethodNotAllowed, "")
	neutron := newFakeNeutron(t, routes)
	r := &networkResource{config: neutron.config}

	resp := runDelete(r, networkRowWithoutID(t, r))
	if sent := neutron.received(); len(sent) != 0 {
		t.Fatalf("delete of a row with no ID sent %v; it names nothing to delete", sent)
	}
	if resp.Diagnostics.HasError() || resp.Diagnostics.WarningsCount() != 1 {
		t.Fatalf("diagnostics = %v, want exactly one warning", resp.Diagnostics)
	}
}
