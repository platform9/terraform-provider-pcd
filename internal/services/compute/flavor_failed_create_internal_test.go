// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package compute

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
	flavorID   = "fl-1"
	flavorName = "tf-flavor"

	flavorsPath    = "/flavors"
	flavorPath     = flavorsPath + "/" + flavorID
	extraSpecsPath = flavorPath + "/os-extra_specs"
)

// flavorJSON is fl-1 as Nova returns it at the base microversion, which
// reports a swap of 0 as "".
const flavorJSON = `{"id": "fl-1", "name": "tf-flavor", "ram": 2048, "vcpus": 2, "disk": 20,
	"swap": "", "OS-FLV-EXT-DATA:ephemeral": 0, "OS-FLV-DISABLED:disabled": false,
	"os-flavor-access:is_public": true, "rxtx_factor": 1.0}`

// extraSpecsJSON is the extra spec the tests configure, as Nova returns it.
const extraSpecsJSON = `{"extra_specs": {"hw:cpu_policy": "dedicated"}}`

// flavorRoutes answers every call flavorResource makes for fl-1 the way Nova
// does. Each test replaces the route whose failure it exercises.
func flavorRoutes() novaRoutes {
	return novaRoutes{
		"POST " + flavorsPath:                         reply(http.StatusOK, `{"flavor": `+flavorJSON+`}`),
		"GET " + flavorPath:                           reply(http.StatusOK, `{"flavor": `+flavorJSON+`}`),
		"POST " + extraSpecsPath:                      reply(http.StatusOK, extraSpecsJSON),
		"GET " + extraSpecsPath:                       reply(http.StatusOK, extraSpecsJSON),
		"DELETE " + extraSpecsPath + "/hw:cpu_policy": reply(http.StatusOK, ""),
		"DELETE " + flavorPath:                        reply(http.StatusAccepted, ""),
	}
}

// stringMap returns m as a Terraform map of strings.
func stringMap(t *testing.T, m map[string]string) types.Map {
	t.Helper()
	v, d := types.MapValueFrom(context.Background(), types.StringType, m)
	if d.HasError() {
		t.Fatalf("building the map: %v", d)
	}
	return v
}

// flavorPlan is the plan the framework computes for a new flavor whose config
// sets name, ram, vcpus and disk, plus extra_specs when given. swap, is_public
// and ephemeral take their defaults, and the Computed attributes the config
// leaves unset are unknown.
func flavorPlan(t *testing.T, r *flavorResource, extraSpecs map[string]string) tfsdk.Plan {
	t.Helper()
	planned := flavorModel{
		ID:         types.StringUnknown(),
		Name:       types.StringValue(flavorName),
		RAM:        types.Int64Value(2048),
		VCPUs:      types.Int64Value(2),
		Disk:       types.Int64Value(20),
		FlavorID:   types.StringUnknown(),
		Swap:       types.Int64Value(0),
		RxTxFactor: types.Float64Unknown(),
		IsPublic:   types.BoolValue(true),
		Ephemeral:  types.Int64Value(0),
		ExtraSpecs: types.MapUnknown(types.StringType),
		Region:     types.StringUnknown(),
	}
	if extraSpecs != nil {
		planned.ExtraSpecs = stringMap(t, extraSpecs)
	}
	return newPlan(t, schemaOf(t, r), &planned)
}

// flavorInState is fl-1 as state holds it after a create that set extraSpecs.
func flavorInState(t *testing.T, extraSpecs map[string]string) flavorModel {
	t.Helper()
	return flavorModel{
		ID:         types.StringValue(flavorID),
		Name:       types.StringValue(flavorName),
		RAM:        types.Int64Value(2048),
		VCPUs:      types.Int64Value(2),
		Disk:       types.Int64Value(20),
		FlavorID:   types.StringValue(flavorID),
		Swap:       types.Int64Value(0),
		RxTxFactor: types.Float64Value(1),
		IsPublic:   types.BoolValue(true),
		Ephemeral:  types.Int64Value(0),
		ExtraSpecs: stringMap(t, extraSpecs),
		Region:     types.StringValue("region-one"),
	}
}

// flavorRowWithoutID is a flavor row whose id is null. Create sets the ID from
// the POST response before any step that can fail, so no provider path is
// known to write one; Read and Delete refuse it all the same, as every
// resource's do.
func flavorRowWithoutID(t *testing.T, r *flavorResource) tfsdk.State {
	t.Helper()
	m := flavorInState(t, map[string]string{})
	m.ID = types.StringNull()
	m.FlavorID = types.StringNull()
	m.RxTxFactor = types.Float64Null()
	m.ExtraSpecs = types.MapNull(types.StringType)
	m.Region = types.StringNull()
	return newState(t, schemaOf(t, r), &m)
}

// flavorRow reads the flavor model out of state.
func flavorRow(t *testing.T, state tfsdk.State) flavorModel {
	t.Helper()
	var m flavorModel
	if d := state.Get(context.Background(), &m); d.HasError() {
		t.Fatalf("reading the state: %v", d)
	}
	return m
}

// A create whose every step succeeds must return the flavor fully known, after
// setting the extra specs.
func TestFlavorCreateFillsEveryAttribute(t *testing.T) {
	t.Parallel()
	nova := newFakeNova(t, flavorRoutes())
	r := &flavorResource{config: nova.config}

	resp := runCreate(r, flavorPlan(t, r, map[string]string{"hw:cpu_policy": "dedicated"}))
	if resp.Diagnostics.HasError() {
		t.Fatalf("create: %v", resp.Diagnostics)
	}
	if !resp.State.Raw.IsFullyKnown() {
		t.Fatalf("create state holds unknown values, which Terraform refuses: %v", resp.State.Raw)
	}
	got := flavorRow(t, resp.State)
	if got.ID.ValueString() != flavorID || got.FlavorID.ValueString() != flavorID ||
		got.RxTxFactor.ValueFloat64() != 1 || got.Region.ValueString() != "region-one" {
		t.Fatalf("create state id=%s flavor_id=%s rx_tx_factor=%s region=%s; want fl-1, fl-1, 1, region-one",
			got.ID, got.FlavorID, got.RxTxFactor, got.Region)
	}
	if want := stringMap(t, map[string]string{"hw:cpu_policy": "dedicated"}); !got.ExtraSpecs.Equal(want) {
		t.Fatalf("create state extra_specs = %s, want %s", got.ExtraSpecs, want)
	}
	want := []string{
		"POST " + flavorsPath,
		"POST " + extraSpecsPath,
		"GET " + extraSpecsPath,
	}
	if sent := nova.received(); !slices.Equal(sent, want) {
		t.Fatalf("create sent %v, want %v", sent, want)
	}
}

// Nova keeps a flavor whose extra-specs POST fails. Create used to return
// before it set any state, so Terraform forgot the flavor and left it in Nova.
// Create must return the error with the flavor's ID in state (Terraform then
// taints it).
func TestFlavorCreateKeepsStateWhenSettingExtraSpecsFails(t *testing.T) {
	t.Parallel()
	routes := flavorRoutes()
	routes["POST "+extraSpecsPath] = badGateway
	r := &flavorResource{config: newFakeNova(t, routes).config}

	resp := runCreate(r, flavorPlan(t, r, map[string]string{"hw:cpu_policy": "dedicated"}))
	if !resp.Diagnostics.HasError() {
		t.Fatal("create succeeded; want the 502 on the extra-specs POST reported")
	}
	if resp.State.Raw.IsNull() {
		t.Fatal("create returned no state: Terraform forgets fl-1 and leaves it in Nova")
	}
	if !resp.State.Raw.IsFullyKnown() {
		t.Fatalf("create state holds unknown values, which Terraform saves as null: %v", resp.State.Raw)
	}
	if got := flavorRow(t, resp.State); got.ID.ValueString() != flavorID {
		t.Fatalf("create state id = %s, want fl-1: a row without one names nothing a destroy could delete", got.ID)
	}
}

// Create's read-back is the extra-specs listing. When it failed, Create used to
// write the plan's unknown extra_specs, which Terraform refuses. Create must
// return the error with the flavor fully known in state (Terraform then taints
// it), and a refresh must fill in what the read-back would have.
func TestFlavorCreateKeepsStateWhenReadBackFails(t *testing.T) {
	t.Parallel()
	routes := flavorRoutes()
	// The config sets no extra specs, so Nova holds none.
	routes["GET "+extraSpecsPath] = badGatewayFirst(reply(http.StatusOK, `{"extra_specs": {}}`))
	r := &flavorResource{config: newFakeNova(t, routes).config}

	createResp := runCreate(r, flavorPlan(t, r, nil))
	if !createResp.Diagnostics.HasError() {
		t.Fatal("create succeeded; want the 502 on the read-back reported")
	}
	if createResp.State.Raw.IsNull() {
		t.Fatal("create returned no state: Terraform forgets fl-1 and leaves it in Nova")
	}
	if !createResp.State.Raw.IsFullyKnown() {
		t.Fatalf("create state holds unknown values, which Terraform saves as null: %v", createResp.State.Raw)
	}
	if got := flavorRow(t, createResp.State); got.ID.ValueString() != flavorID {
		t.Fatalf("create state id = %s, want fl-1", got.ID)
	}

	// The replacing apply, or a destroy, refreshes the tainted flavor first.
	readResp := runRead(r, createResp.State)
	if readResp.Diagnostics.HasError() {
		t.Fatalf("refresh of the recorded flavor: %v", readResp.Diagnostics)
	}
	if readResp.State.Raw.IsNull() {
		t.Fatal("refresh dropped fl-1 from state")
	}
	got := flavorRow(t, readResp.State)
	if got.ID.ValueString() != flavorID || got.ExtraSpecs.IsNull() || len(got.ExtraSpecs.Elements()) != 0 {
		t.Fatalf("refreshed id=%s extra_specs=%s; want fl-1 and an empty map", got.ID, got.ExtraSpecs)
	}
}

// Read must drop a row with no ID with a warning that names the flavor,
// without sending anything: a GET for an empty ID reaches the flavors
// collection URL, not a flavor.
func TestFlavorReadDropsARowWithNoID(t *testing.T) {
	t.Parallel()
	nova := newFakeNova(t, flavorRoutes())
	r := &flavorResource{config: nova.config}

	resp := runRead(r, flavorRowWithoutID(t, r))
	if sent := nova.received(); len(sent) != 0 {
		t.Fatalf("refresh of a row with no ID sent %v; it names nothing to read", sent)
	}
	if resp.Diagnostics.HasError() || resp.Diagnostics.WarningsCount() != 1 {
		t.Fatalf("diagnostics = %v, want exactly one warning", resp.Diagnostics)
	}
	if detail := resp.Diagnostics[0].Detail(); !strings.Contains(detail, `"tf-flavor"`) {
		t.Fatalf("warning detail %q does not name the flavor to look for", detail)
	}
	if !resp.State.Raw.IsNull() {
		t.Fatalf("refresh kept a row with no ID: %v", resp.State.Raw)
	}
}

// A 200 for the flavor's own URL whose body holds no flavor object makes
// gophercloud return no flavor and no error, and Read dereferenced it. Read
// must report that as an error and keep the row, and not treat it as
// not-found: that would drop a real flavor from state.
func TestFlavorReadRefusesAnAnswerWithoutTheFlavor(t *testing.T) {
	t.Parallel()
	routes := flavorRoutes()
	routes["GET "+flavorPath] = reply(http.StatusOK, `{"flavors": [`+flavorJSON+`]}`)
	r := &flavorResource{config: newFakeNova(t, routes).config}
	prior := flavorInState(t, map[string]string{"hw:cpu_policy": "dedicated"})
	priorState := newState(t, schemaOf(t, r), &prior)

	resp := runRead(r, priorState)
	if !resp.Diagnostics.HasError() {
		t.Fatalf("refresh accepted an answer without the flavor: diagnostics %v", resp.Diagnostics)
	}
	if !resp.State.Raw.Equal(priorState.Raw) {
		t.Fatalf("refresh state = %v, want the prior state %v", resp.State.Raw, priorState.Raw)
	}
}

// An update whose read-back fails must return the error and keep the prior
// state, which the next plan compares against. Update used to write the plan
// as if the read-back had confirmed it, so a plan whose extra_specs was unknown
// put an unknown in state, which Terraform refuses.
func TestFlavorUpdateKeepsStateWhenReadBackFails(t *testing.T) {
	t.Parallel()
	routes := flavorRoutes()
	routes["GET "+extraSpecsPath] = badGateway
	nova := newFakeNova(t, routes)
	r := &flavorResource{config: nova.config}
	s := schemaOf(t, r)

	prior := flavorInState(t, map[string]string{"hw:cpu_policy": "shared"})
	planned := prior
	planned.ExtraSpecs = stringMap(t, map[string]string{"hw:cpu_policy": "dedicated"})
	priorState := newState(t, s, &prior)

	resp := runUpdate(r, newPlan(t, s, &planned), priorState)
	if !resp.Diagnostics.HasError() {
		t.Fatal("update succeeded; want the 502 on the read-back reported")
	}
	if !resp.State.Raw.Equal(priorState.Raw) {
		t.Fatalf("update state = %v, want the prior state %v", resp.State.Raw, priorState.Raw)
	}
	want := []string{"POST " + extraSpecsPath, "GET " + extraSpecsPath}
	if sent := nova.received(); !slices.Equal(sent, want) {
		t.Fatalf("update sent %v, want %v", sent, want)
	}
}

// terraform destroy -refresh=false reaches Delete without Read dropping a row
// with no ID first. A DELETE for an empty ID reaches the flavors collection
// URL. Delete must warn and send nothing, so Terraform forgets the row.
func TestFlavorDeleteSkipsARowWithNoID(t *testing.T) {
	t.Parallel()
	nova := newFakeNova(t, flavorRoutes())
	r := &flavorResource{config: nova.config}

	resp := runDelete(r, flavorRowWithoutID(t, r))
	if sent := nova.received(); len(sent) != 0 {
		t.Fatalf("delete of a row with no ID sent %v; it names nothing to delete", sent)
	}
	if resp.Diagnostics.HasError() || resp.Diagnostics.WarningsCount() != 1 {
		t.Fatalf("diagnostics = %v, want exactly one warning", resp.Diagnostics)
	}
}
