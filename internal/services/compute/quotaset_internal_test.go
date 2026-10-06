// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package compute

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-go/tftypes"

	"github.com/platform9/terraform-provider-pcd/internal/clients"
)

// novaWithoutQuotaSet starts a test server that answers GET
// /os-quota-sets/proj-1 with a 200 whose body holds no quota_set object, and
// returns a config whose Nova client reaches it. Any other request fails the
// test.
func novaWithoutQuotaSet(t *testing.T) *clients.Config {
	t.Helper()
	nova := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method+" "+r.URL.Path != "GET /os-quota-sets/proj-1" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotImplemented)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{}`)
	}))
	t.Cleanup(nova.Close)
	return &clients.Config{
		Region: "region-one",
		Provider: &gophercloud.ProviderClient{EndpointLocator: func(gophercloud.EndpointOpts) (string, error) {
			return nova.URL + "/", nil
		}},
	}
}

// importQuotaset calls ImportState the way the framework does, with a response
// state that starts as a null object.
func importQuotaset(t *testing.T, r *quotasetResource, id string) resource.ImportStateResponse {
	t.Helper()
	ctx := context.Background()
	var sch resource.SchemaResponse
	r.Schema(ctx, resource.SchemaRequest{}, &sch)
	s := sch.Schema
	resp := resource.ImportStateResponse{State: tfsdk.State{Schema: s, Raw: tftypes.NewValue(s.Type().TerraformType(ctx), nil)}}
	r.ImportState(ctx, resource.ImportStateRequest{ID: id}, &resp)
	return resp
}

// An import ID with no project part, such as "/region-one", used to import a
// row with project_id "", whose refresh sends GET for an empty project ID.
// Import must refuse the ID.
func TestQuotasetImportRejectsAnIDWithoutAProject(t *testing.T) {
	t.Parallel()
	resp := importQuotaset(t, &quotasetResource{}, "/region-one")
	if !resp.Diagnostics.HasError() {
		t.Fatal("import accepted an ID with no project")
	}
}

// A 200 for the project's own URL whose body holds no quota_set object makes
// gophercloud return no quota set and no error. The refresh after an import
// must report that as an error, and not as not-found: that would drop the row.
func TestQuotasetReadRefusesAnAnswerWithoutTheQuotaSet(t *testing.T) {
	t.Parallel()
	r := &quotasetResource{config: novaWithoutQuotaSet(t)}
	importResp := importQuotaset(t, r, "proj-1/region-one")
	if importResp.Diagnostics.HasError() {
		t.Fatalf("import: %v", importResp.Diagnostics)
	}

	resp := resource.ReadResponse{State: importResp.State}
	r.Read(context.Background(), resource.ReadRequest{State: importResp.State}, &resp)
	if !resp.Diagnostics.HasError() {
		t.Fatalf("refresh accepted an answer without the quota set: diagnostics %v", resp.Diagnostics)
	}
	if resp.State.Raw.IsNull() {
		t.Fatal("refresh dropped the quotas from state")
	}
}

// The same answer to the read-back after a create or an update must be an
// error too.
func TestQuotasetReadIntoRefusesAnAnswerWithoutTheQuotaSet(t *testing.T) {
	t.Parallel()
	r := &quotasetResource{config: novaWithoutQuotaSet(t)}
	client, err := r.config.ComputeV2Client()
	if err != nil {
		t.Fatalf("building the fake Nova client: %v", err)
	}

	var m quotasetModel
	if diags := r.readInto(context.Background(), client, "proj-1", "region-one", &m); !diags.HasError() {
		t.Fatalf("readInto accepted an answer without the quota set: diagnostics %v", diags)
	}
}
