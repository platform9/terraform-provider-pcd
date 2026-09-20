// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package dns

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"

	"github.com/platform9/terraform-provider-pcd/internal/clients"
)

// fakeConfig points a clients.Config at a test server, so the Designate client
// the resources build resolves to it. The locator ignores the endpoint options,
// so it answers for every service type and availability. EndpointOverrides is
// deliberately left empty: applyOverride clears the client's ResourceBase, which
// would drop the "v2/" prefix NewDNSV2 sets and make every path below a miss.
func fakeConfig(url string) *clients.Config {
	return &clients.Config{
		Region: "region-one",
		Provider: &gophercloud.ProviderClient{
			EndpointLocator: func(gophercloud.EndpointOpts) (string, error) { return url + "/", nil },
		},
	}
}

// A recordset whose creation ends in ERROR stays in Designate. Create used to
// return without state, so Terraform forgot the recordset, the next apply
// created another, and the first had to be deleted by hand. Create must return
// the error with the recordset in state (Terraform then taints it), and the
// refresh and delete a destroy runs must remove the recordset even though
// Designate reports ERROR.
func TestRecordSetCreateKeepsARecordSetThatFailedToBuild(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	var mu sync.Mutex
	deleteCalled, getsAfterDelete := false, 0
	designate := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch r.Method + " " + r.URL.Path {
		case "POST /v2/zones/zone-1/recordsets":
			// Designate answers 202 for a recordset create, and gophercloud
			// decodes the body regardless of the status, so it must not be empty.
			w.WriteHeader(http.StatusAccepted)
			fmt.Fprint(w, `{"id": "rr-1", "zone_id": "zone-1", "name": "www.tf-acc-example.com.",
				"type": "A", "records": ["10.1.0.1"], "ttl": 300, "status": "PENDING",
				"action": "CREATE", "description": ""}`)
		case "GET /v2/zones/zone-1/recordsets/rr-1":
			if deleteCalled {
				getsAfterDelete++
				if getsAfterDelete > 1 {
					w.WriteHeader(http.StatusNotFound)
					return
				}
			}
			// No created_at/updated_at: RecordSet.UnmarshalJSON parses them with
			// gophercloud's no-Z layout, so a Z-suffixed value fails Extract.
			fmt.Fprint(w, `{"id": "rr-1", "zone_id": "zone-1", "name": "www.tf-acc-example.com.",
				"type": "A", "records": ["10.1.0.1"], "ttl": 300, "status": "ERROR",
				"action": "CREATE", "description": ""}`)
		case "DELETE /v2/zones/zone-1/recordsets/rr-1":
			deleteCalled = true
			w.WriteHeader(http.StatusAccepted)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotImplemented)
		}
	}))
	defer designate.Close()

	r := &recordSetResource{config: fakeConfig(designate.URL)}
	var sch resource.SchemaResponse
	r.Schema(ctx, resource.SchemaRequest{}, &sch)
	s := sch.Schema

	records, d := types.SetValueFrom(ctx, types.StringType, []string{"10.1.0.1"})
	if d.HasError() {
		t.Fatalf("building the records set: %v", d)
	}
	planned := recordSetModel{
		ID:          types.StringUnknown(),
		ZoneID:      types.StringValue("zone-1"),
		Name:        types.StringValue("www.tf-acc-example.com."),
		Type:        types.StringValue("A"),
		Records:     records,
		TTL:         types.Int64Unknown(),
		Description: types.StringUnknown(),
		Status:      types.StringUnknown(),
		Region:      types.StringUnknown(),
	}
	plan := tfsdk.Plan{Schema: s, Raw: tftypes.NewValue(s.Type().TerraformType(ctx), nil)}
	if d := plan.Set(ctx, &planned); d.HasError() {
		t.Fatalf("building the plan: %v", d)
	}

	createResp := resource.CreateResponse{State: tfsdk.State{Schema: s, Raw: tftypes.NewValue(s.Type().TerraformType(ctx), nil)}}
	r.Create(ctx, resource.CreateRequest{Plan: plan}, &createResp)
	if !createResp.Diagnostics.HasError() {
		t.Fatal("create succeeded; want the ERROR status reported")
	}
	if createResp.State.Raw.IsNull() {
		t.Fatal("create returned no state: Terraform forgets rr-1 and the next apply creates a second recordset")
	}
	if !createResp.State.Raw.IsFullyKnown() {
		t.Fatalf("create state holds unknown values, which Terraform refuses: %v", createResp.State.Raw)
	}
	var got recordSetModel
	if d := createResp.State.Get(ctx, &got); d.HasError() {
		t.Fatalf("reading the create state: %v", d)
	}
	if got.ID.ValueString() != "rr-1" || got.ZoneID.ValueString() != "zone-1" ||
		got.Name.ValueString() != "www.tf-acc-example.com." || got.Type.ValueString() != "A" {
		t.Fatalf("create state id=%s zone_id=%s name=%s type=%s; want rr-1, zone-1, www.tf-acc-example.com., A",
			got.ID, got.ZoneID, got.Name, got.Type)
	}
	// records is Required and echo-only, so the configured value must survive
	// into the recorded row: readInto refills it only when it is null.
	var recorded []string
	if d := got.Records.ElementsAs(ctx, &recorded, false); d.HasError() {
		t.Fatalf("reading records from the create state: %v", d)
	}
	if len(recorded) != 1 || recorded[0] != "10.1.0.1" {
		t.Fatalf("create state records = %v, want [10.1.0.1]", recorded)
	}

	// terraform destroy (or the replacing apply) refreshes the tainted recordset first.
	readResp := resource.ReadResponse{State: createResp.State}
	r.Read(ctx, resource.ReadRequest{State: createResp.State}, &readResp)
	if readResp.Diagnostics.HasError() {
		t.Fatalf("refresh of the failed recordset: %v", readResp.Diagnostics)
	}
	if readResp.State.Raw.IsNull() {
		t.Fatal("refresh dropped rr-1 from state")
	}
	if d := readResp.State.Get(ctx, &got); d.HasError() {
		t.Fatalf("reading the refreshed state: %v", d)
	}
	if got.Status.ValueString() != "ERROR" {
		t.Fatalf("refreshed status = %s, want ERROR", got.Status)
	}
	if got.TTL.ValueInt64() != 300 {
		t.Fatalf("refreshed ttl = %d, want 300", got.TTL.ValueInt64())
	}
	if got.Region.ValueString() != "region-one" {
		t.Fatalf("refreshed region = %s, want region-one", got.Region)
	}

	deleteResp := resource.DeleteResponse{State: readResp.State}
	r.Delete(ctx, resource.DeleteRequest{State: readResp.State}, &deleteResp)
	if deleteResp.Diagnostics.HasError() {
		t.Fatalf("delete of a recordset in ERROR: %v", deleteResp.Diagnostics)
	}
	mu.Lock()
	defer mu.Unlock()
	if !deleteCalled {
		t.Fatal("delete never called DELETE /v2/zones/zone-1/recordsets/rr-1")
	}
	if getsAfterDelete < 2 {
		t.Fatalf("delete returned after %d polls; want it to wait out the ERROR status until Designate answers 404", getsAfterDelete)
	}
}
