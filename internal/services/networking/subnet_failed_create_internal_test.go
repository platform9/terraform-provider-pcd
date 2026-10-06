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
	subnetID        = "subnet-1"
	subnetName      = "workload-subnet"
	subnetNetworkID = "net-1"
	subnetCIDR      = "10.20.0.0/24"

	subnetsPath = "/v2.0/subnets"
	subnetPath  = subnetsPath + "/" + subnetID
)

// subnetJSON is subnet-1 as Neutron returns it, with the gateway and allocation
// pool Neutron derives from the CIDR and the tag the tests configure.
const subnetJSON = `{"id": "subnet-1", "name": "workload-subnet", "description": "",
	"network_id": "net-1", "tenant_id": "proj-1", "project_id": "proj-1",
	"ip_version": 4, "cidr": "10.20.0.0/24", "gateway_ip": "10.20.0.1", "enable_dhcp": true,
	"allocation_pools": [{"start": "10.20.0.2", "end": "10.20.0.254"}], "host_routes": [],
	"dns_nameservers": [], "dns_publish_fixed_ip": false, "service_types": [],
	"subnetpool_id": null, "ipv6_address_mode": null, "ipv6_ra_mode": null, "segment_id": null,
	"tags": ["workload"], "revision_number": 1,
	"created_at": "2026-10-02T00:00:00Z", "updated_at": "2026-10-02T00:00:00Z"}`

// subnetRoutes answers every call subnetResource makes for subnet-1 the way
// Neutron does. Each test replaces the route whose failure it exercises.
func subnetRoutes() neutronRoutes {
	return neutronRoutes{
		"POST " + subnetsPath:         reply(http.StatusCreated, `{"subnet": `+subnetJSON+`}`),
		"GET " + subnetPath:           reply(http.StatusOK, `{"subnet": `+subnetJSON+`}`),
		"PUT " + subnetPath:           reply(http.StatusOK, `{"subnet": `+subnetJSON+`}`),
		"PUT " + subnetPath + "/tags": reply(http.StatusOK, `{"tags": ["workload"]}`),
		"DELETE " + subnetPath:        reply(http.StatusNoContent, ""),
	}
}

// subnetPlan is the plan the framework computes for a new subnet whose config
// sets network_id, name and cidr, plus tags when given. The Computed attributes
// the config leaves unset are unknown, and ip_version, enable_dhcp and
// dns_publish_fixed_ip take their defaults.
func subnetPlan(t *testing.T, r *subnetResource, tags []string) tfsdk.Plan {
	t.Helper()
	planned := subnetModel{
		ID:                types.StringUnknown(),
		NetworkID:         types.StringValue(subnetNetworkID),
		Name:              types.StringValue(subnetName),
		Description:       types.StringUnknown(),
		CIDR:              types.StringValue(subnetCIDR),
		IPVersion:         types.Int64Value(4),
		GatewayIP:         types.StringUnknown(),
		EnableDHCP:        types.BoolValue(true),
		DNSNameservers:    types.ListUnknown(types.StringType),
		AllocationPools:   types.ListUnknown(poolObjType),
		DNSPublishFixedIP: types.BoolValue(false),
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

// subnetRowWithoutID is the row provider v0.1.14 left in state when the
// read-back after a successful POST failed: Terraform saved the unknowns
// Create returned as null, the ID among them.
func subnetRowWithoutID(t *testing.T, r *subnetResource) tfsdk.State {
	t.Helper()
	return newState(t, schemaOf(t, r), &subnetModel{
		ID:                types.StringNull(),
		NetworkID:         types.StringValue(subnetNetworkID),
		Name:              types.StringValue(subnetName),
		Description:       types.StringNull(),
		CIDR:              types.StringValue(subnetCIDR),
		IPVersion:         types.Int64Value(4),
		GatewayIP:         types.StringNull(),
		EnableDHCP:        types.BoolValue(true),
		DNSNameservers:    types.ListNull(types.StringType),
		AllocationPools:   types.ListNull(poolObjType),
		DNSPublishFixedIP: types.BoolValue(false),
		TenantID:          types.StringNull(),
		Tags:              types.SetNull(types.StringType),
		Region:            types.StringNull(),
	})
}

// subnetRecorded is subnet-1 as state holds it after a successful create with
// the workload tag.
func subnetRecorded(t *testing.T) subnetModel {
	t.Helper()
	ctx := context.Background()
	tags, d := types.SetValueFrom(ctx, types.StringType, []string{"workload"})
	if d.HasError() {
		t.Fatalf("building the tags: %v", d)
	}
	nameservers, d := types.ListValueFrom(ctx, types.StringType, []string{})
	if d.HasError() {
		t.Fatalf("building the nameservers: %v", d)
	}
	pools, d := types.ListValueFrom(ctx, poolObjType, []allocationPoolModel{
		{Start: types.StringValue("10.20.0.2"), End: types.StringValue("10.20.0.254")},
	})
	if d.HasError() {
		t.Fatalf("building the allocation pools: %v", d)
	}
	return subnetModel{
		ID:                types.StringValue(subnetID),
		NetworkID:         types.StringValue(subnetNetworkID),
		Name:              types.StringValue(subnetName),
		Description:       types.StringValue(""),
		CIDR:              types.StringValue(subnetCIDR),
		IPVersion:         types.Int64Value(4),
		GatewayIP:         types.StringValue("10.20.0.1"),
		EnableDHCP:        types.BoolValue(true),
		DNSNameservers:    nameservers,
		AllocationPools:   pools,
		DNSPublishFixedIP: types.BoolValue(false),
		TenantID:          types.StringValue("proj-1"),
		Tags:              tags,
		Region:            types.StringValue("region-one"),
	}
}

// subnetRow reads the subnet model out of state.
func subnetRow(t *testing.T, state tfsdk.State) subnetModel {
	t.Helper()
	var m subnetModel
	if d := state.Get(context.Background(), &m); d.HasError() {
		t.Fatalf("reading the state: %v", d)
	}
	return m
}

// A create whose every step succeeds must return the subnet fully known, after
// setting the tags.
func TestSubnetCreateFillsEveryAttribute(t *testing.T) {
	t.Parallel()
	neutron := newFakeNeutron(t, subnetRoutes())
	r := &subnetResource{config: neutron.config}

	resp := runCreate(r, subnetPlan(t, r, []string{"workload"}))
	if resp.Diagnostics.HasError() {
		t.Fatalf("create: %v", resp.Diagnostics)
	}
	if !resp.State.Raw.IsFullyKnown() {
		t.Fatalf("create state holds unknown values, which Terraform refuses: %v", resp.State.Raw)
	}
	got := subnetRow(t, resp.State)
	if got.ID.ValueString() != subnetID || got.TenantID.ValueString() != "proj-1" ||
		got.GatewayIP.ValueString() != "10.20.0.1" || got.Region.ValueString() != "region-one" {
		t.Fatalf("create state id=%s tenant_id=%s gateway_ip=%s region=%s; want subnet-1, proj-1, 10.20.0.1, region-one",
			got.ID, got.TenantID, got.GatewayIP, got.Region)
	}
	want := []string{
		"POST " + subnetsPath,
		"PUT " + subnetPath + "/tags",
		"GET " + subnetPath,
	}
	if sent := neutron.received(); !slices.Equal(sent, want) {
		t.Fatalf("create sent %v, want %v", sent, want)
	}
}

// Neutron keeps a subnet whose read-back fails. Create used to write the plan's
// unknowns, which Terraform saved as null, ID included, so the subnet was
// orphaned and the next refresh read the collection URL and crashed. Create
// must return the error with the subnet's ID in state (Terraform then taints
// it), and a refresh must fill in what the read-back would have.
func TestSubnetCreateKeepsStateWhenReadBackFails(t *testing.T) {
	t.Parallel()
	routes := subnetRoutes()
	routes["GET "+subnetPath] = badGatewayFirst(routes["GET "+subnetPath])
	r := &subnetResource{config: newFakeNeutron(t, routes).config}

	createResp := runCreate(r, subnetPlan(t, r, nil))
	if !createResp.Diagnostics.HasError() {
		t.Fatal("create succeeded; want the 502 on the read-back reported")
	}
	if createResp.State.Raw.IsNull() {
		t.Fatal("create returned no state: Terraform forgets subnet-1 and the next apply creates a second subnet")
	}
	if !createResp.State.Raw.IsFullyKnown() {
		t.Fatalf("create state holds unknown values, which Terraform saves as null: %v", createResp.State.Raw)
	}
	if got := subnetRow(t, createResp.State); got.ID.ValueString() != subnetID {
		t.Fatalf("create state id = %s, want subnet-1: a row without one names nothing a destroy could delete", got.ID)
	}

	// The replacing apply, or a destroy, refreshes the tainted subnet first.
	readResp := runRead(r, createResp.State)
	if readResp.Diagnostics.HasError() {
		t.Fatalf("refresh of the recorded subnet: %v", readResp.Diagnostics)
	}
	if readResp.State.Raw.IsNull() {
		t.Fatal("refresh dropped subnet-1 from state")
	}
	got := subnetRow(t, readResp.State)
	if got.ID.ValueString() != subnetID || got.TenantID.ValueString() != "proj-1" ||
		got.GatewayIP.ValueString() != "10.20.0.1" || got.Region.ValueString() != "region-one" {
		t.Fatalf("refreshed id=%s tenant_id=%s gateway_ip=%s region=%s; want subnet-1, proj-1, 10.20.0.1, region-one",
			got.ID, got.TenantID, got.GatewayIP, got.Region)
	}
}

// A failed tags PUT used to return before Create set any state, so Terraform
// forgot the subnet and the next apply sent a second create for it.
func TestSubnetCreateKeepsStateWhenTagsFail(t *testing.T) {
	t.Parallel()
	routes := subnetRoutes()
	routes["PUT "+subnetPath+"/tags"] = badGateway
	r := &subnetResource{config: newFakeNeutron(t, routes).config}

	resp := runCreate(r, subnetPlan(t, r, []string{"workload"}))
	if !resp.Diagnostics.HasError() {
		t.Fatal("create succeeded; want the 502 on the tags PUT reported")
	}
	if resp.State.Raw.IsNull() {
		t.Fatal("create returned no state: Terraform forgets subnet-1 and the next apply creates a second subnet")
	}
	if !resp.State.Raw.IsFullyKnown() {
		t.Fatalf("create state holds unknown values, which Terraform saves as null: %v", resp.State.Raw)
	}
	if got := subnetRow(t, resp.State); got.ID.ValueString() != subnetID {
		t.Fatalf("create state id = %s, want subnet-1", got.ID)
	}
}

// A read-back that finds the subnet gone right after the POST used to be
// swallowed: Create reported nothing and Terraform saved a row with no ID.
// Create must report it and leave nothing in state.
func TestSubnetCreateDropsStateWhenReadBack404s(t *testing.T) {
	t.Parallel()
	routes := subnetRoutes()
	routes["GET "+subnetPath] = reply(http.StatusNotFound,
		`{"NeutronError": {"type": "SubnetNotFound", "message": "Subnet subnet-1 could not be found.", "detail": ""}}`)
	r := &subnetResource{config: newFakeNeutron(t, routes).config}

	resp := runCreate(r, subnetPlan(t, r, nil))
	if !resp.Diagnostics.HasError() {
		t.Fatal("create succeeded; want the 404 read-back reported")
	}
	if !resp.State.Raw.IsNull() {
		t.Fatalf("create left a row for a subnet the read-back found gone: %v", resp.State.Raw)
	}
}

// The row v0.1.14 left has no ID. Read used to send GET for an empty ID, which
// reaches the collection URL; Neutron answers that with the list, and readInto
// dereferenced the nil subnet gophercloud decoded from it. Read must drop the
// row with a warning that names the subnet, without sending anything.
func TestSubnetReadDropsARowWithNoID(t *testing.T) {
	t.Parallel()
	routes := subnetRoutes()
	routes["GET "+subnetsPath+"/"] = reply(http.StatusOK, `{"subnets": [`+subnetJSON+`]}`)
	neutron := newFakeNeutron(t, routes)
	r := &subnetResource{config: neutron.config}

	resp := runRead(r, subnetRowWithoutID(t, r))
	if sent := neutron.received(); len(sent) != 0 {
		t.Fatalf("refresh of a row with no ID sent %v; it names nothing to read", sent)
	}
	if resp.Diagnostics.HasError() || resp.Diagnostics.WarningsCount() != 1 {
		t.Fatalf("diagnostics = %v, want exactly one warning", resp.Diagnostics)
	}
	if detail := resp.Diagnostics[0].Detail(); !strings.Contains(detail, `"workload-subnet"`) {
		t.Fatalf("warning detail %q does not name the subnet to look for", detail)
	}
	if !resp.State.Raw.IsNull() {
		t.Fatalf("refresh kept a row with no ID: %v", resp.State.Raw)
	}
}

// A 200 for the subnet's own URL whose body holds no subnet object makes
// gophercloud return no subnet and no error. readInto must report that as an
// error, and not as not-found: that would drop a real subnet from state.
func TestSubnetReadIntoRefusesAnAnswerWithoutTheSubnet(t *testing.T) {
	t.Parallel()
	routes := subnetRoutes()
	routes["GET "+subnetPath] = reply(http.StatusOK, `{"subnets": [`+subnetJSON+`]}`)
	r := &subnetResource{config: newFakeNeutron(t, routes).config}
	client, err := r.config.NetworkV2Client()
	if err != nil {
		t.Fatalf("building the fake Neutron client: %v", err)
	}

	m := subnetModel{Region: types.StringNull()}
	notFound, diags := r.readInto(context.Background(), client, subnetID, &m)
	if notFound {
		t.Fatal("readInto reported subnet-1 not found; the next refresh would drop a subnet that exists")
	}
	if !diags.HasError() {
		t.Fatalf("readInto accepted an answer without the subnet: diagnostics %v", diags)
	}
}

// An update whose read-back fails must return the error and keep the prior
// state, which the next plan compares against. Update used to write the plan
// over it, as though the read-back had confirmed every value.
func TestSubnetUpdateKeepsStateWhenReadBackFails(t *testing.T) {
	t.Parallel()
	routes := subnetRoutes()
	routes["GET "+subnetPath] = badGateway
	r := &subnetResource{config: newFakeNeutron(t, routes).config}
	s := schemaOf(t, r)

	// A rename. Every Computed attribute has a default or keeps its state
	// value, so the plan is the prior state with the new name.
	prior := subnetRecorded(t)
	planned := prior
	planned.Name = types.StringValue(subnetName + "-renamed")
	priorState := newState(t, s, &prior)

	resp := runUpdate(r, newPlan(t, s, &planned), priorState)
	if !resp.Diagnostics.HasError() {
		t.Fatal("update succeeded; want the 502 on the read-back reported")
	}
	if !resp.State.Raw.Equal(priorState.Raw) {
		t.Fatalf("update state = %v, want the prior state %v", resp.State.Raw, priorState.Raw)
	}
}

// A read-back that finds the subnet gone right after the update used to be
// swallowed: Update reported success and saved the plan. Update must report it
// and keep the prior state; the next refresh then drops the row.
func TestSubnetUpdateReportsAReadBack404(t *testing.T) {
	t.Parallel()
	routes := subnetRoutes()
	routes["GET "+subnetPath] = reply(http.StatusNotFound,
		`{"NeutronError": {"type": "SubnetNotFound", "message": "Subnet subnet-1 could not be found.", "detail": ""}}`)
	r := &subnetResource{config: newFakeNeutron(t, routes).config}
	s := schemaOf(t, r)

	prior := subnetRecorded(t)
	planned := prior
	planned.Name = types.StringValue(subnetName + "-renamed")
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
// the collection URL, and Neutron refuses that. Delete must warn and send
// nothing, so Terraform forgets the row.
func TestSubnetDeleteSkipsARowWithNoID(t *testing.T) {
	t.Parallel()
	routes := subnetRoutes()
	routes["DELETE "+subnetsPath+"/"] = reply(http.StatusMethodNotAllowed, "")
	neutron := newFakeNeutron(t, routes)
	r := &subnetResource{config: neutron.config}

	resp := runDelete(r, subnetRowWithoutID(t, r))
	if sent := neutron.received(); len(sent) != 0 {
		t.Fatalf("delete of a row with no ID sent %v; it names nothing to delete", sent)
	}
	if resp.Diagnostics.HasError() || resp.Diagnostics.WarningsCount() != 1 {
		t.Fatalf("diagnostics = %v, want exactly one warning", resp.Diagnostics)
	}
	if detail := resp.Diagnostics[0].Detail(); !strings.Contains(detail, `"workload-subnet"`) {
		t.Fatalf("warning detail %q does not name the subnet to look for", detail)
	}
}
