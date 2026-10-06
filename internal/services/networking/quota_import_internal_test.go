// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package networking

import (
	"context"
	"net/http"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

const (
	quotasPath = "/v2.0/quotas"
	quotaPath  = quotasPath + "/proj-1"
)

// quotaJSON is proj-1's quotas as Neutron returns them.
const quotaJSON = `{"floatingip": 50, "network": 100, "port": 500, "rbac_policy": 10, "router": 10,
	"security_group": 10, "security_group_rule": 100, "subnet": 100, "subnetpool": -1, "trunk": -1}`

// quotaListJSON is what Neutron answers an admin for the quotas collection
// URL: a list of projects' quotas, each tagged with its project.
const quotaListJSON = `{"quotas": [{"project_id": "proj-1", "tenant_id": "proj-1", "floatingip": 50,
	"network": 100, "port": 500, "rbac_policy": 10, "router": 10, "security_group": 10,
	"security_group_rule": 100, "subnet": 100, "subnetpool": -1, "trunk": -1}]}`

// runImport calls ImportState the way the framework does, with a response
// state that starts as a null object. Terraform then refreshes the imported
// row with Read before it saves it, unless the import reported an error.
func runImport(t *testing.T, r resource.ResourceWithImportState, id string) resource.ImportStateResponse {
	t.Helper()
	s := schemaOf(t, r)
	resp := resource.ImportStateResponse{State: tfsdk.State{Schema: s, Raw: tftypes.NewValue(s.Type().TerraformType(context.Background()), nil)}}
	r.ImportState(context.Background(), resource.ImportStateRequest{ID: id}, &resp)
	return resp
}

func quotaRow(t *testing.T, state tfsdk.State) quotaModel {
	t.Helper()
	var m quotaModel
	if d := state.Get(context.Background(), &m); d.HasError() {
		t.Fatalf("reading the state: %v", d)
	}
	return m
}

// A well-formed import ID imports the project's quotas, and the refresh after
// it fills every attribute.
func TestQuotaImportFillsEveryAttribute(t *testing.T) {
	t.Parallel()
	neutron := newFakeNeutron(t, neutronRoutes{
		"GET " + quotaPath: reply(http.StatusOK, `{"quota": `+quotaJSON+`}`),
	})
	r := &quotaResource{config: neutron.config}

	importResp := runImport(t, r, "proj-1/region-one")
	if importResp.Diagnostics.HasError() {
		t.Fatalf("import: %v", importResp.Diagnostics)
	}
	readResp := runRead(r, importResp.State)
	if readResp.Diagnostics.HasError() {
		t.Fatalf("refresh of the imported quotas: %v", readResp.Diagnostics)
	}
	if !readResp.State.Raw.IsFullyKnown() {
		t.Fatalf("refreshed state holds unknown values: %v", readResp.State.Raw)
	}
	got := quotaRow(t, readResp.State)
	if got.ID.ValueString() != "proj-1/region-one" || got.ProjectID.ValueString() != "proj-1" ||
		got.Network.ValueInt64() != 100 || got.SubnetPool.ValueInt64() != -1 {
		t.Fatalf("refreshed id=%s project_id=%s network=%s subnetpool=%s; want proj-1/region-one, proj-1, 100, -1",
			got.ID, got.ProjectID, got.Network, got.SubnetPool)
	}
}

// An import ID with no project part, such as "/region-one", used to import a
// row with project_id "". The refresh Terraform runs after an import then sent
// GET for an empty project ID, which reaches the collection URL; Neutron
// answers that with a list, gophercloud decodes it to no quota, and the
// provider crashed. Import must refuse the ID, so Terraform refreshes nothing.
func TestQuotaImportRejectsAnIDWithoutAProject(t *testing.T) {
	t.Parallel()
	neutron := newFakeNeutron(t, neutronRoutes{
		"GET " + quotasPath + "/": reply(http.StatusOK, quotaListJSON),
	})
	r := &quotaResource{config: neutron.config}

	resp := runImport(t, r, "/region-one")
	if !resp.Diagnostics.HasError() {
		// Terraform refreshes the imported row next.
		runRead(r, resp.State)
		t.Fatalf("import accepted an ID with no project; it sent %v", neutron.received())
	}
}

// A 200 for the project's own URL whose body holds no quota object makes
// gophercloud return no quota and no error. Read must report that as an error,
// and not as not-found: that would drop the row from state.
func TestQuotaReadRefusesAnAnswerWithoutTheQuota(t *testing.T) {
	t.Parallel()
	r := &quotaResource{config: newFakeNeutron(t, neutronRoutes{
		"GET " + quotaPath: reply(http.StatusOK, quotaListJSON),
	}).config}

	importResp := runImport(t, r, "proj-1/region-one")
	if importResp.Diagnostics.HasError() {
		t.Fatalf("import: %v", importResp.Diagnostics)
	}
	resp := runRead(r, importResp.State)
	if !resp.Diagnostics.HasError() {
		t.Fatalf("refresh accepted an answer without the quota: diagnostics %v", resp.Diagnostics)
	}
	if resp.State.Raw.IsNull() {
		t.Fatal("refresh dropped the quotas from state")
	}
}

// The same answer to the read-back after a create or an update must be an
// error too.
func TestQuotaReadIntoRefusesAnAnswerWithoutTheQuota(t *testing.T) {
	t.Parallel()
	r := &quotaResource{config: newFakeNeutron(t, neutronRoutes{
		"GET " + quotaPath: reply(http.StatusOK, quotaListJSON),
	}).config}
	client, err := r.config.NetworkV2Client()
	if err != nil {
		t.Fatalf("building the fake Neutron client: %v", err)
	}

	var m quotaModel
	if diags := r.readInto(context.Background(), client, "proj-1", "region-one", &m); !diags.HasError() {
		t.Fatalf("readInto accepted an answer without the quota: diagnostics %v", diags)
	}
}
