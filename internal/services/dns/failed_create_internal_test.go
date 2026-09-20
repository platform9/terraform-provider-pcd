// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package dns

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

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

// A zone whose build ends in ERROR stays in Designate. Create used to return
// without state, so Terraform forgot the zone, the next apply created another,
// and the first had to be deleted by hand. Create must return the error with
// the zone in state (Terraform then taints it), and the refresh and delete a
// destroy runs must remove the zone even though Designate still reports ERROR
// on the first poll after the DELETE is accepted.
func TestZoneCreateKeepsAZoneThatFailedToBuild(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	const zoneJSON = `{"id": "zone-1", "name": "tf-acc-failed.example.com.",
		"email": "dns@example.com", "type": "PRIMARY", "ttl": 3600, "description": "",
		"status": "ERROR", "action": "CREATE", "serial": 1, "pool_id": "pool-1",
		"project_id": "proj-1", "attributes": {}, "masters": []}`

	var mu sync.Mutex
	deleteCalled, getsAfterDelete := false, 0
	designate := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch r.Method + " " + r.URL.Path {
		case "POST /v2/zones":
			// Designate answers 201 or 202; the zone is bare, with no wrapper key.
			w.WriteHeader(http.StatusAccepted)
			fmt.Fprint(w, `{"id": "zone-1", "name": "tf-acc-failed.example.com.",
				"status": "PENDING", "action": "CREATE"}`)
		case "GET /v2/zones/zone-1":
			if deleteCalled {
				getsAfterDelete++
				// The first poll after the DELETE still reports ERROR: the delete
				// waiter must poll through it rather than bail.
				if getsAfterDelete > 1 {
					w.WriteHeader(http.StatusNotFound)
					return
				}
			}
			fmt.Fprint(w, zoneJSON)
		case "DELETE /v2/zones/zone-1":
			// zones.Delete accepts 202 only, and decodes the body, so an empty
			// body fails with io.EOF.
			deleteCalled = true
			w.WriteHeader(http.StatusAccepted)
			fmt.Fprint(w, `{"id": "zone-1", "status": "PENDING", "action": "DELETE"}`)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotImplemented)
		}
	}))
	defer designate.Close()

	r := &zoneResource{config: fakeConfig(designate.URL)}
	var sch resource.SchemaResponse
	r.Schema(ctx, resource.SchemaRequest{}, &sch)
	s := sch.Schema

	// No attribute in the zone schema carries a static default, so every
	// Optional+Computed attribute the config does not set is unknown here.
	planned := zoneModel{
		ID:          types.StringUnknown(),
		Name:        types.StringValue("tf-acc-failed.example.com."),
		Type:        types.StringValue("PRIMARY"),
		Email:       types.StringValue("dns@example.com"),
		TTL:         types.Int64Unknown(),
		Description: types.StringUnknown(),
		Masters:     types.ListUnknown(types.StringType),
		Attributes:  types.MapUnknown(types.StringType),
		Serial:      types.Int64Unknown(),
		Status:      types.StringUnknown(),
		PoolID:      types.StringUnknown(),
		ProjectID:   types.StringUnknown(),
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
		t.Fatal("create returned no state: Terraform forgets zone-1 and the next apply creates a second zone")
	}
	if !createResp.State.Raw.IsFullyKnown() {
		t.Fatalf("create state holds unknown values, which Terraform refuses: %v", createResp.State.Raw)
	}
	var got zoneModel
	if d := createResp.State.Get(ctx, &got); d.HasError() {
		t.Fatalf("reading the create state: %v", d)
	}
	if got.ID.ValueString() != "zone-1" || got.Name.ValueString() != "tf-acc-failed.example.com." {
		t.Fatalf("create state id=%s name=%s; want zone-1, tf-acc-failed.example.com.", got.ID, got.Name)
	}
	if !got.Status.IsNull() {
		t.Fatalf("create state status = %s; want null, since Designate has not reported it yet", got.Status)
	}

	// terraform destroy (or the replacing apply) refreshes the tainted zone first.
	readResp := resource.ReadResponse{State: createResp.State}
	r.Read(ctx, resource.ReadRequest{State: createResp.State}, &readResp)
	if readResp.Diagnostics.HasError() {
		t.Fatalf("refresh of the failed zone: %v", readResp.Diagnostics)
	}
	if d := readResp.State.Get(ctx, &got); d.HasError() {
		t.Fatalf("reading the refreshed state: %v", d)
	}
	if got.Status.ValueString() != "ERROR" {
		t.Fatalf("refreshed status = %s, want ERROR", got.Status)
	}
	if got.Region.ValueString() != "region-one" {
		t.Fatalf("refreshed region = %s, want region-one backfilled from the provider config", got.Region)
	}

	deleteResp := resource.DeleteResponse{State: readResp.State}
	r.Delete(ctx, resource.DeleteRequest{State: readResp.State}, &deleteResp)
	if deleteResp.Diagnostics.HasError() {
		t.Fatalf("delete of a zone in ERROR: %v", deleteResp.Diagnostics)
	}
	mu.Lock()
	defer mu.Unlock()
	if !deleteCalled {
		t.Fatal("delete never called DELETE /v2/zones/zone-1")
	}
	if getsAfterDelete < 2 {
		t.Fatalf("delete returned after %d polls; want it to poll through the ERROR status until Designate answers 404", getsAfterDelete)
	}
}

// The next two tests drive waitForZoneDeleted directly, bypassing
// Create/Read/Delete, so each pins exactly one of the latch's three required
// cases (the third, ERROR-then-non-error-then-404, is already exercised end
// to end by TestZoneCreateKeepsAZoneThatFailedToBuild above).

// Case 1: a healthy zone whose delete genuinely fails. The first poll sees a
// normal in-progress status, latching seenNonError, and a later ERROR must
// still fail fast with the existing message. TestZoneCreateKeepsAZoneThat-
// FailedToBuild never puts a non-ERROR status before an ERROR one, so it
// cannot catch a regression here: simplifying the waiter to an unconditional
// poll to 404/timeout (the design's rejected Option A) would pass that test
// unchanged while turning this case into a ten-minute timeout.
func TestWaitForZoneDeletedFailsFastWhenAHealthyZoneEntersError(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	var mu sync.Mutex
	gets := 0
	designate := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch r.Method + " " + r.URL.Path {
		case "GET /v2/zones/zone-1":
			gets++
			status := "ERROR"
			if gets == 1 {
				// The first poll finds the zone still on its way out: a
				// normal, non-ERROR in-progress status.
				status = "PENDING_DELETE"
			}
			fmt.Fprintf(w, `{"id": "zone-1", "name": "healthy.example.com.", "status": %q, "action": "DELETE"}`, status)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotImplemented)
		}
	}))
	defer designate.Close()

	client, err := fakeConfig(designate.URL).DNSV2Client()
	if err != nil {
		t.Fatalf("building the fake DNS client: %v", err)
	}

	start := time.Now()
	err = waitForZoneDeleted(ctx, client, "zone-1", 5*time.Second)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("waitForZoneDeleted succeeded; want the ERROR entered during delete reported")
	}
	if !strings.Contains(err.Error(), "entered ERROR status during delete") {
		t.Fatalf("error = %v; want it to name the ERROR-during-delete failure", err)
	}
	if elapsed >= 4*time.Second {
		t.Fatalf("waitForZoneDeleted took %s to fail; want it to fail on the poll right after "+
			"PENDING_DELETE, well inside the 5s timeout -- a waiter polled through instead of "+
			"latched would take the full timeout", elapsed)
	}
}

// Case 3: a zone the failed create wait already abandoned in ERROR, whose
// backend delete then also fails, so it never leaves ERROR. Unlike case 1,
// there is no "during delete" transition to report -- the zone was already
// broken when the delete began -- so this must not fail fast, and it must
// eventually time out rather than being wedged forever the way the
// unconditional pre-fix bail wedged it (see the reverted-bail negative
// control in the task report; that is a different regression from case 1's
// and does not exercise the same code path this test does).
func TestWaitForZoneDeletedTimesOutRatherThanWedgingOnPersistentError(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	designate := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "GET /v2/zones/zone-1":
			// The zone never leaves ERROR: the backend refuses the delete.
			fmt.Fprint(w, `{"id": "zone-1", "name": "wedged.example.com.", "status": "ERROR", "action": "DELETE"}`)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotImplemented)
		}
	}))
	defer designate.Close()

	client, err := fakeConfig(designate.URL).DNSV2Client()
	if err != nil {
		t.Fatalf("building the fake DNS client: %v", err)
	}

	start := time.Now()
	err = waitForZoneDeleted(ctx, client, "zone-1", 2*time.Second)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("waitForZoneDeleted succeeded; want the persistent ERROR to time out the delete")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v; want it to wrap context.DeadlineExceeded", err)
	}
	if strings.Contains(err.Error(), "entered ERROR status during delete") {
		t.Fatalf("error = %v; a zone already in ERROR before the delete began must not report "+
			`"entered ERROR status during delete" -- it was never seen leaving ERROR`, err)
	}
	if !strings.Contains(err.Error(), `last status "ERROR"`) {
		t.Fatalf("error = %v; want the timeout to name the last observed status", err)
	}
	if elapsed < 2*time.Second {
		t.Fatalf("waitForZoneDeleted returned after %s, before its own 2s timeout elapsed", elapsed)
	}
}
