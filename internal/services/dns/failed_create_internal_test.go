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
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
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
				// normal, non-ERROR in-progress status. Designate reports
				// this as status PENDING with action DELETE, not a combined
				// "PENDING_DELETE" status.
				status = "PENDING"
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
			"PENDING, well inside the 5s timeout -- a waiter polled through instead of "+
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

// The following four tests drive Create down the path neither test above
// reaches: the status wait succeeds (ACTIVE), and the read-back that follows
// it is what fails. readInto returns without touching its model argument on
// both a 404 and a non-404 error, so a Create that unconditionally sets state
// from the plan afterward would write back the plan's own unknown values on
// top of the clean, fully-known row RecordCreated already wrote -- a state
// Terraform refuses. The guard must instead: on a 404, report the error and
// remove the resource (nothing is left to keep); on any other error, report
// it and leave the recorded row in place (the object still exists).

// A recordset create whose status wait reaches ACTIVE, but whose read-back
// then finds the recordset already gone, must report the error and drop the
// recordset from state.
func TestRecordSetCreateDropsStateWhenReadBackAfterActive404s(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	var mu sync.Mutex
	gets := 0
	designate := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch r.Method + " " + r.URL.Path {
		case "POST /v2/zones/zone-1/recordsets":
			w.WriteHeader(http.StatusAccepted)
			fmt.Fprint(w, `{"id": "rr-1", "zone_id": "zone-1", "name": "www.tf-acc-gone.example.com.",
				"type": "A", "records": ["10.1.0.1"], "ttl": 300, "status": "PENDING",
				"action": "CREATE", "description": ""}`)
		case "GET /v2/zones/zone-1/recordsets/rr-1":
			gets++
			if gets == 1 {
				// Satisfies waitForRecordSetActive on the very first poll.
				fmt.Fprint(w, `{"id": "rr-1", "zone_id": "zone-1", "name": "www.tf-acc-gone.example.com.",
					"type": "A", "records": ["10.1.0.1"], "ttl": 300, "status": "ACTIVE",
					"action": "CREATE", "description": ""}`)
				return
			}
			// The read-back that immediately follows the wait finds it gone.
			w.WriteHeader(http.StatusNotFound)
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
		Name:        types.StringValue("www.tf-acc-gone.example.com."),
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
		t.Fatal("create succeeded; want the post-ACTIVE 404 read-back reported")
	}
	if !createResp.State.Raw.IsNull() {
		t.Fatal("create left a row in state for a recordset a 404 read-back confirmed gone: " +
			"a genuine 404 right after ACTIVE means there is nothing left to keep")
	}
	mu.Lock()
	defer mu.Unlock()
	if gets < 2 {
		t.Fatalf("GET called %d times; want the wait's ACTIVE poll and the read-back poll", gets)
	}
}

// A recordset create whose status wait reaches ACTIVE, but whose read-back
// then fails with a server error (not a 404), must report the error and
// leave the row RecordCreated wrote in place: the recordset still exists,
// and dropping it would recreate the orphan this change removes.
func TestRecordSetCreateKeepsStateWhenReadBackAfterActiveFails(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	var mu sync.Mutex
	gets := 0
	designate := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch r.Method + " " + r.URL.Path {
		case "POST /v2/zones/zone-1/recordsets":
			w.WriteHeader(http.StatusAccepted)
			fmt.Fprint(w, `{"id": "rr-1", "zone_id": "zone-1", "name": "www.tf-acc-flaky.example.com.",
				"type": "A", "records": ["10.1.0.1"], "ttl": 300, "status": "PENDING",
				"action": "CREATE", "description": ""}`)
		case "GET /v2/zones/zone-1/recordsets/rr-1":
			gets++
			if gets == 1 {
				// Satisfies waitForRecordSetActive on the very first poll.
				fmt.Fprint(w, `{"id": "rr-1", "zone_id": "zone-1", "name": "www.tf-acc-flaky.example.com.",
					"type": "A", "records": ["10.1.0.1"], "ttl": 300, "status": "ACTIVE",
					"action": "CREATE", "description": ""}`)
				return
			}
			// The read-back that immediately follows the wait hits a transient
			// server error; the recordset itself is still there.
			w.WriteHeader(http.StatusInternalServerError)
			fmt.Fprint(w, `{"error": "designate temporarily unavailable"}`)
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
		Name:        types.StringValue("www.tf-acc-flaky.example.com."),
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
		t.Fatal("create succeeded; want the post-ACTIVE 500 read-back reported")
	}
	if createResp.State.Raw.IsNull() {
		t.Fatal("create dropped rr-1 from state even though the recordset still exists behind the failed read-back")
	}
	if !createResp.State.Raw.IsFullyKnown() {
		t.Fatalf("create state holds unknown values, which Terraform refuses: %v", createResp.State.Raw)
	}
	var got recordSetModel
	if d := createResp.State.Get(ctx, &got); d.HasError() {
		t.Fatalf("reading the create state: %v", d)
	}
	if got.ID.ValueString() != "rr-1" {
		t.Fatalf("create state id = %s, want rr-1: a row without one names nothing a destroy could delete", got.ID)
	}
	mu.Lock()
	defer mu.Unlock()
	if gets < 2 {
		t.Fatalf("GET called %d times; want the wait's ACTIVE poll and the failed read-back poll", gets)
	}
}

// A zone create whose status wait reaches ACTIVE, but whose read-back then
// finds the zone already gone, must report the error and drop the zone from
// state.
func TestZoneCreateDropsStateWhenReadBackAfterActive404s(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	var mu sync.Mutex
	gets := 0
	designate := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch r.Method + " " + r.URL.Path {
		case "POST /v2/zones":
			w.WriteHeader(http.StatusAccepted)
			fmt.Fprint(w, `{"id": "zone-1", "name": "tf-acc-gone.example.com.",
				"status": "PENDING", "action": "CREATE"}`)
		case "GET /v2/zones/zone-1":
			gets++
			if gets == 1 {
				// Satisfies waitForZoneActive on the very first poll.
				fmt.Fprint(w, `{"id": "zone-1", "name": "tf-acc-gone.example.com.",
					"email": "dns@example.com", "type": "PRIMARY", "ttl": 3600,
					"description": "", "status": "ACTIVE", "action": "CREATE", "serial": 1,
					"pool_id": "pool-1", "project_id": "proj-1", "attributes": {}, "masters": []}`)
				return
			}
			// The read-back that immediately follows the wait finds it gone.
			w.WriteHeader(http.StatusNotFound)
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

	planned := zoneModel{
		ID:          types.StringUnknown(),
		Name:        types.StringValue("tf-acc-gone.example.com."),
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
		t.Fatal("create succeeded; want the post-ACTIVE 404 read-back reported")
	}
	if !createResp.State.Raw.IsNull() {
		t.Fatal("create left a row in state for a zone a 404 read-back confirmed gone: " +
			"a genuine 404 right after ACTIVE means there is nothing left to keep")
	}
	mu.Lock()
	defer mu.Unlock()
	if gets < 2 {
		t.Fatalf("GET called %d times; want the wait's ACTIVE poll and the read-back poll", gets)
	}
}

// A zone create whose status wait reaches ACTIVE, but whose read-back then
// fails with a server error (not a 404), must report the error and leave the
// row RecordCreated wrote in place: the zone still exists, and dropping it
// would recreate the orphan this change removes.
func TestZoneCreateKeepsStateWhenReadBackAfterActiveFails(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	var mu sync.Mutex
	gets := 0
	designate := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch r.Method + " " + r.URL.Path {
		case "POST /v2/zones":
			w.WriteHeader(http.StatusAccepted)
			fmt.Fprint(w, `{"id": "zone-1", "name": "tf-acc-flaky.example.com.",
				"status": "PENDING", "action": "CREATE"}`)
		case "GET /v2/zones/zone-1":
			gets++
			if gets == 1 {
				// Satisfies waitForZoneActive on the very first poll.
				fmt.Fprint(w, `{"id": "zone-1", "name": "tf-acc-flaky.example.com.",
					"email": "dns@example.com", "type": "PRIMARY", "ttl": 3600,
					"description": "", "status": "ACTIVE", "action": "CREATE", "serial": 1,
					"pool_id": "pool-1", "project_id": "proj-1", "attributes": {}, "masters": []}`)
				return
			}
			// The read-back that immediately follows the wait hits a transient
			// server error; the zone itself is still there.
			w.WriteHeader(http.StatusInternalServerError)
			fmt.Fprint(w, `{"error": "designate temporarily unavailable"}`)
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

	planned := zoneModel{
		ID:          types.StringUnknown(),
		Name:        types.StringValue("tf-acc-flaky.example.com."),
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
		t.Fatal("create succeeded; want the post-ACTIVE 500 read-back reported")
	}
	if createResp.State.Raw.IsNull() {
		t.Fatal("create dropped zone-1 from state even though the zone still exists behind the failed read-back")
	}
	if !createResp.State.Raw.IsFullyKnown() {
		t.Fatalf("create state holds unknown values, which Terraform refuses: %v", createResp.State.Raw)
	}
	var got zoneModel
	if d := createResp.State.Get(ctx, &got); d.HasError() {
		t.Fatalf("reading the create state: %v", d)
	}
	if got.ID.ValueString() != "zone-1" {
		t.Fatalf("create state id = %s, want zone-1: a row without one names nothing a destroy could delete", got.ID)
	}
	mu.Lock()
	defer mu.Unlock()
	if gets < 2 {
		t.Fatalf("GET called %d times; want the wait's ACTIVE poll and the failed read-back poll", gets)
	}
}

// The tests below cover the zone and recordset guards for rows provider
// v0.1.13 and earlier left in state, and for an update whose read-back fails.
// A row from then has no ID when the read-back after a successful create
// failed: Terraform saved the unknowns Create returned as null, the ID among
// them. The tests share an offline Designate fake and the framework plumbing
// that follows. Every request path carries a /v2/ prefix: openstack.NewDNSV2
// sets the client's ResourceBase to the endpoint plus "v2/".

// designateRoutes maps a request, written "METHOD /path" (for example
// "GET /v2/zones/zone-1"), to the handler that answers it.
type designateRoutes map[string]http.HandlerFunc

// fakeDesignate is a test server that answers from its routes and logs every
// request it receives, so a test can assert what a resource sent, including
// that it sent nothing. It serves one request at a time, so a route may keep a
// counter without a lock of its own. A request no route names fails the test.
type fakeDesignate struct {
	// config builds Designate clients that reach this server.
	config *clients.Config

	mu       sync.Mutex
	requests []string
}

// newFakeDesignate starts a fakeDesignate serving routes. The server closes
// when the test ends.
func newFakeDesignate(t *testing.T, routes designateRoutes) *fakeDesignate {
	t.Helper()
	f := &fakeDesignate{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		call := r.Method + " " + r.URL.Path
		f.requests = append(f.requests, call)
		if handle, ok := routes[call]; ok {
			handle(w, r)
			return
		}
		// 501 is no status Designate sends for any call; it only stops the
		// resource from waiting on a call the test did not expect.
		t.Errorf("unexpected request %s", call)
		w.WriteHeader(http.StatusNotImplemented)
	}))
	t.Cleanup(srv.Close)
	f.config = fakeConfig(srv.URL)
	return f
}

// received returns the requests served so far, in order, each written
// "METHOD /path".
func (f *fakeDesignate) received() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.requests...)
}

// reply answers with status and, unless body is empty, a JSON body. A route
// must answer with a status gophercloud accepts for that call when it models
// success: Designate answers a get with 200, and a zone PATCH or a recordset
// PUT with 202.
func reply(status int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		if body != "" {
			w.Header().Set("Content-Type", "application/json")
		}
		w.WriteHeader(status)
		fmt.Fprint(w, body)
	}
}

// nginxBadGatewayPage is the HTML page nginx serves with a 502.
const nginxBadGatewayPage = "<html>\r\n<head><title>502 Bad Gateway</title></head>\r\n<body>\r\n" +
	"<center><h1>502 Bad Gateway</h1></center>\r\n<hr><center>nginx</center>\r\n</body>\r\n</html>\r\n"

// badGateway answers 502 with nginx's HTML page, as an ingress in front of
// Designate can. The body is not JSON.
func badGateway(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html")
	w.WriteHeader(http.StatusBadGateway)
	fmt.Fprint(w, nginxBadGatewayPage)
}

// firstThen answers the first request with first and every later one with
// then: a status wait that sees ACTIVE on its first poll, followed by a
// read-back that does not.
func firstThen(first, then http.HandlerFunc) http.HandlerFunc {
	calls := 0
	return func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			first(w, r)
			return
		}
		then(w, r)
	}
}

// schemaOf returns r's schema.
func schemaOf(t *testing.T, r resource.Resource) schema.Schema {
	t.Helper()
	var resp resource.SchemaResponse
	r.Schema(context.Background(), resource.SchemaRequest{}, &resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("schema: %v", resp.Diagnostics)
	}
	return resp.Schema
}

// newPlan returns a plan of schema s that holds model, a pointer to the
// resource's model struct.
func newPlan(t *testing.T, s schema.Schema, model any) tfsdk.Plan {
	t.Helper()
	plan := tfsdk.Plan{Schema: s, Raw: tftypes.NewValue(s.Type().TerraformType(context.Background()), nil)}
	if d := plan.Set(context.Background(), model); d.HasError() {
		t.Fatalf("building the plan: %v", d)
	}
	return plan
}

// newState returns a state of schema s that holds model, a pointer to the
// resource's model struct.
func newState(t *testing.T, s schema.Schema, model any) tfsdk.State {
	t.Helper()
	state := tfsdk.State{Schema: s, Raw: tftypes.NewValue(s.Type().TerraformType(context.Background()), nil)}
	if d := state.Set(context.Background(), model); d.HasError() {
		t.Fatalf("building the state: %v", d)
	}
	return state
}

// The run helpers call a resource method the way the framework does, which
// decides what a method that returns without setting state leaves behind.

// runRead calls Read with a response state that starts as the current state.
func runRead(r resource.Resource, state tfsdk.State) resource.ReadResponse {
	resp := resource.ReadResponse{State: state}
	r.Read(context.Background(), resource.ReadRequest{State: state}, &resp)
	return resp
}

// runUpdate calls Update with a response state that starts as the prior state,
// so an Update that returns early keeps it.
func runUpdate(r resource.Resource, plan tfsdk.Plan, prior tfsdk.State) resource.UpdateResponse {
	resp := resource.UpdateResponse{State: prior}
	r.Update(context.Background(), resource.UpdateRequest{Plan: plan, State: prior}, &resp)
	return resp
}

// runDelete calls Delete with a response state that starts as the prior state.
func runDelete(r resource.Resource, prior tfsdk.State) resource.DeleteResponse {
	resp := resource.DeleteResponse{State: prior}
	r.Delete(context.Background(), resource.DeleteRequest{State: prior}, &resp)
	return resp
}

const (
	zonesPath = "/v2/zones"
	zonePath  = zonesPath + "/zone-1"

	recordSetsPath = zonePath + "/recordsets"
	recordSetPath  = recordSetsPath + "/rr-1"
)

// activeZoneJSON is zone-1 as Designate returns it once it is ACTIVE. It has
// no created_at or updated_at: Zone.UnmarshalJSON parses them with
// gophercloud's no-Z layout.
const activeZoneJSON = `{"id": "zone-1", "name": "tf-acc-example.com.", "email": "dns@example.com",
	"type": "PRIMARY", "ttl": 3600, "description": "", "status": "ACTIVE", "action": "NONE",
	"serial": 1, "pool_id": "pool-1", "project_id": "proj-1", "attributes": {}, "masters": []}`

// zoneListJSON is Designate's answer to a zone list, holding zone-1.
const zoneListJSON = `{"zones": [` + activeZoneJSON + `],
	"links": {"self": "https://pcd.example.com/dns/v2/zones"}, "metadata": {"total_count": 1}}`

// zoneRoutes answers the calls zoneResource makes to update zone-1 the way
// Designate does. Each test replaces the route whose failure it exercises.
func zoneRoutes() designateRoutes {
	return designateRoutes{
		"GET " + zonePath: reply(http.StatusOK, activeZoneJSON),
		"PATCH " + zonePath: reply(http.StatusAccepted,
			`{"id": "zone-1", "name": "tf-acc-example.com.", "status": "PENDING", "action": "UPDATE"}`),
	}
}

// zoneRowWithoutID is the row provider v0.1.13 and earlier left in state when
// the read-back after a successful create failed. The config set name, type
// and email.
func zoneRowWithoutID(t *testing.T, r *zoneResource) tfsdk.State {
	t.Helper()
	return newState(t, schemaOf(t, r), &zoneModel{
		ID:          types.StringNull(),
		Name:        types.StringValue("tf-acc-example.com."),
		Type:        types.StringValue("PRIMARY"),
		Email:       types.StringValue("dns@example.com"),
		TTL:         types.Int64Null(),
		Description: types.StringNull(),
		Masters:     types.ListNull(types.StringType),
		Attributes:  types.MapNull(types.StringType),
		Serial:      types.Int64Null(),
		Status:      types.StringNull(),
		PoolID:      types.StringNull(),
		ProjectID:   types.StringNull(),
		Region:      types.StringNull(),
	})
}

// activeZone is zone-1 as the state holds it after a successful apply.
func activeZone(t *testing.T) zoneModel {
	t.Helper()
	masters, d := types.ListValueFrom(context.Background(), types.StringType, []string{})
	if d.HasError() {
		t.Fatalf("building the masters: %v", d)
	}
	attrs, d := types.MapValueFrom(context.Background(), types.StringType, map[string]string{})
	if d.HasError() {
		t.Fatalf("building the attributes: %v", d)
	}
	return zoneModel{
		ID:          types.StringValue("zone-1"),
		Name:        types.StringValue("tf-acc-example.com."),
		Type:        types.StringValue("PRIMARY"),
		Email:       types.StringValue("dns@example.com"),
		TTL:         types.Int64Value(3600),
		Description: types.StringValue(""),
		Masters:     masters,
		Attributes:  attrs,
		Serial:      types.Int64Value(1),
		Status:      types.StringValue("ACTIVE"),
		PoolID:      types.StringValue("pool-1"),
		ProjectID:   types.StringValue("proj-1"),
		Region:      types.StringValue("region-one"),
	}
}

// The row v0.1.13 and earlier left has no ID. Read used to send GET for an
// empty ID, which reaches the collection URL. A zone list there decodes to a
// zone with every field empty, which readInto saved back as id "", so the row
// stayed and still named nothing. Read must drop the row with a warning that
// names the zone, without sending anything.
func TestZoneReadDropsARowWithNoID(t *testing.T) {
	t.Parallel()
	routes := zoneRoutes()
	routes["GET "+zonesPath+"/"] = reply(http.StatusOK, zoneListJSON)
	designate := newFakeDesignate(t, routes)
	r := &zoneResource{config: designate.config}

	resp := runRead(r, zoneRowWithoutID(t, r))
	if sent := designate.received(); len(sent) != 0 {
		t.Fatalf("refresh of a row with no ID sent %v; it names nothing to read", sent)
	}
	if resp.Diagnostics.HasError() || resp.Diagnostics.WarningsCount() != 1 {
		t.Fatalf("diagnostics = %v, want exactly one warning", resp.Diagnostics)
	}
	if detail := resp.Diagnostics[0].Detail(); !strings.Contains(detail, `"tf-acc-example.com."`) {
		t.Fatalf("warning detail %q does not name the zone to look for", detail)
	}
	if !resp.State.Raw.IsNull() {
		t.Fatalf("refresh kept a row with no ID: %v", resp.State.Raw)
	}
}

// A 200 for the zone's own URL whose body is not a zone makes gophercloud
// return a zone with every field empty, or for a JSON null no zone at all, and
// no error. readInto must report either as an error, and not as not-found:
// that would drop a real zone from state.
func TestZoneReadIntoRefusesAnAnswerWithoutTheZone(t *testing.T) {
	t.Parallel()
	for _, answer := range []struct{ name, body string }{
		{"a zone list", zoneListJSON},
		{"a JSON null", "null"},
	} {
		routes := zoneRoutes()
		routes["GET "+zonePath] = reply(http.StatusOK, answer.body)
		r := &zoneResource{config: newFakeDesignate(t, routes).config}
		client, err := r.config.DNSV2Client()
		if err != nil {
			t.Fatalf("building the fake DNS client: %v", err)
		}

		m := zoneModel{Region: types.StringNull()}
		notFound, diags := r.readInto(context.Background(), client, "zone-1", &m)
		if notFound {
			t.Fatalf("%s: readInto reported zone-1 not found; the next refresh would drop a zone that exists", answer.name)
		}
		if !diags.HasError() {
			t.Fatalf("%s: readInto accepted an answer without the zone: diagnostics %v", answer.name, diags)
		}
	}
}

// An update whose read-back fails must return the error and keep the prior
// state, which the next plan compares against. Update used to save the plan
// over it, as though the read-back had succeeded.
func TestZoneUpdateKeepsStateWhenReadBackFails(t *testing.T) {
	t.Parallel()
	routes := zoneRoutes()
	routes["GET "+zonePath] = firstThen(routes["GET "+zonePath], badGateway)
	r := &zoneResource{config: newFakeDesignate(t, routes).config}
	s := schemaOf(t, r)

	prior := activeZone(t)
	planned := prior
	planned.Description = types.StringValue("updated")
	priorState := newState(t, s, &prior)

	resp := runUpdate(r, newPlan(t, s, &planned), priorState)
	if !resp.Diagnostics.HasError() {
		t.Fatal("update succeeded; want the 502 on the read-back reported")
	}
	if !resp.State.Raw.Equal(priorState.Raw) {
		t.Fatalf("update state = %v, want the prior state %v", resp.State.Raw, priorState.Raw)
	}
}

// An update whose read-back finds the zone gone must report it and keep the
// prior state. Update used to report nothing and save the plan.
func TestZoneUpdateKeepsStateWhenReadBack404s(t *testing.T) {
	t.Parallel()
	routes := zoneRoutes()
	routes["GET "+zonePath] = firstThen(routes["GET "+zonePath], reply(http.StatusNotFound, ""))
	r := &zoneResource{config: newFakeDesignate(t, routes).config}
	s := schemaOf(t, r)

	prior := activeZone(t)
	planned := prior
	planned.Description = types.StringValue("updated")
	priorState := newState(t, s, &prior)

	resp := runUpdate(r, newPlan(t, s, &planned), priorState)
	if !resp.Diagnostics.HasError() {
		t.Fatal("update succeeded; want the 404 on the read-back reported")
	}
	if !resp.State.Raw.Equal(priorState.Raw) {
		t.Fatalf("update state = %v, want the prior state %v", resp.State.Raw, priorState.Raw)
	}
}

// terraform destroy -refresh=false reaches Delete without Read dropping the
// v0.1.13 row first. Delete used to send DELETE for an empty ID, which reaches
// the collection URL. Delete must warn and send nothing, so Terraform forgets
// the row.
func TestZoneDeleteSkipsARowWithNoID(t *testing.T) {
	t.Parallel()
	routes := zoneRoutes()
	routes["DELETE "+zonesPath+"/"] = reply(http.StatusMethodNotAllowed, "")
	designate := newFakeDesignate(t, routes)
	r := &zoneResource{config: designate.config}

	resp := runDelete(r, zoneRowWithoutID(t, r))
	if sent := designate.received(); len(sent) != 0 {
		t.Fatalf("delete of a row with no ID sent %v; it names nothing to delete", sent)
	}
	if resp.Diagnostics.HasError() || resp.Diagnostics.WarningsCount() != 1 {
		t.Fatalf("diagnostics = %v, want exactly one warning", resp.Diagnostics)
	}
}

// activeRecordSetJSON is rr-1 as Designate returns it once it is ACTIVE. It has
// no created_at or updated_at, for the same reason as activeZoneJSON.
const activeRecordSetJSON = `{"id": "rr-1", "zone_id": "zone-1", "name": "www.tf-acc-example.com.",
	"type": "A", "records": ["10.1.0.1"], "ttl": 300, "status": "ACTIVE", "action": "NONE",
	"description": ""}`

// recordSetListJSON is Designate's answer to a recordset list, holding rr-1.
const recordSetListJSON = `{"recordsets": [` + activeRecordSetJSON + `],
	"links": {"self": "https://pcd.example.com/dns/v2/zones/zone-1/recordsets"}, "metadata": {"total_count": 1}}`

// recordSetRoutes answers the calls recordSetResource makes to update rr-1 the
// way Designate does. Each test replaces the route whose failure it exercises.
func recordSetRoutes() designateRoutes {
	return designateRoutes{
		"GET " + recordSetPath: reply(http.StatusOK, activeRecordSetJSON),
		"PUT " + recordSetPath: reply(http.StatusAccepted, `{"id": "rr-1", "zone_id": "zone-1",
			"name": "www.tf-acc-example.com.", "type": "A", "records": ["10.1.0.2"], "ttl": 300,
			"status": "PENDING", "action": "UPDATE", "description": ""}`),
	}
}

// recordSetRecords returns values as a records set.
func recordSetRecords(t *testing.T, values ...string) types.Set {
	t.Helper()
	records, d := types.SetValueFrom(context.Background(), types.StringType, values)
	if d.HasError() {
		t.Fatalf("building the records set: %v", d)
	}
	return records
}

// recordSetRowWithoutID is the row provider v0.1.13 and earlier left in state
// when the read-back after a successful create failed. The config set zone_id,
// name, type and records.
func recordSetRowWithoutID(t *testing.T, r *recordSetResource) tfsdk.State {
	t.Helper()
	return newState(t, schemaOf(t, r), &recordSetModel{
		ID:          types.StringNull(),
		ZoneID:      types.StringValue("zone-1"),
		Name:        types.StringValue("www.tf-acc-example.com."),
		Type:        types.StringValue("A"),
		Records:     recordSetRecords(t, "10.1.0.1"),
		TTL:         types.Int64Null(),
		Description: types.StringNull(),
		Status:      types.StringNull(),
		Region:      types.StringNull(),
	})
}

// activeRecordSet is rr-1 as the state holds it after a successful apply.
func activeRecordSet(t *testing.T) recordSetModel {
	t.Helper()
	return recordSetModel{
		ID:          types.StringValue("rr-1"),
		ZoneID:      types.StringValue("zone-1"),
		Name:        types.StringValue("www.tf-acc-example.com."),
		Type:        types.StringValue("A"),
		Records:     recordSetRecords(t, "10.1.0.1"),
		TTL:         types.Int64Value(300),
		Description: types.StringValue(""),
		Status:      types.StringValue("ACTIVE"),
		Region:      types.StringValue("region-one"),
	}
}

// The row v0.1.13 and earlier left has no ID. Read used to send GET for an
// empty ID, which reaches the zone's recordset collection URL. A recordset list
// there decodes to a recordset with every field empty, which readInto saved
// back as id "", so the row stayed and still named nothing. Read must drop the
// row with a warning that names the recordset, without sending anything.
func TestRecordSetReadDropsARowWithNoID(t *testing.T) {
	t.Parallel()
	routes := recordSetRoutes()
	routes["GET "+recordSetsPath+"/"] = reply(http.StatusOK, recordSetListJSON)
	designate := newFakeDesignate(t, routes)
	r := &recordSetResource{config: designate.config}

	resp := runRead(r, recordSetRowWithoutID(t, r))
	if sent := designate.received(); len(sent) != 0 {
		t.Fatalf("refresh of a row with no ID sent %v; it names nothing to read", sent)
	}
	if resp.Diagnostics.HasError() || resp.Diagnostics.WarningsCount() != 1 {
		t.Fatalf("diagnostics = %v, want exactly one warning", resp.Diagnostics)
	}
	if detail := resp.Diagnostics[0].Detail(); !strings.Contains(detail, `"www.tf-acc-example.com."`) {
		t.Fatalf("warning detail %q does not name the recordset to look for", detail)
	}
	if !resp.State.Raw.IsNull() {
		t.Fatalf("refresh kept a row with no ID: %v", resp.State.Raw)
	}
}

// A 200 for the recordset's own URL whose body is not a recordset makes
// gophercloud return a recordset with every field empty, or for a JSON null no
// recordset at all, and no error. readInto must report either as an error, and
// not as not-found: that would drop a real recordset from state.
func TestRecordSetReadIntoRefusesAnAnswerWithoutTheRecordSet(t *testing.T) {
	t.Parallel()
	for _, answer := range []struct{ name, body string }{
		{"a recordset list", recordSetListJSON},
		{"a JSON null", "null"},
	} {
		routes := recordSetRoutes()
		routes["GET "+recordSetPath] = reply(http.StatusOK, answer.body)
		r := &recordSetResource{config: newFakeDesignate(t, routes).config}
		client, err := r.config.DNSV2Client()
		if err != nil {
			t.Fatalf("building the fake DNS client: %v", err)
		}

		m := recordSetModel{Records: types.SetNull(types.StringType), Region: types.StringNull()}
		notFound, diags := r.readInto(context.Background(), client, "zone-1", "rr-1", &m)
		if notFound {
			t.Fatalf("%s: readInto reported rr-1 not found; the next refresh would drop a recordset that exists", answer.name)
		}
		if !diags.HasError() {
			t.Fatalf("%s: readInto accepted an answer without the recordset: diagnostics %v", answer.name, diags)
		}
	}
}

// An update whose read-back fails must return the error and keep the prior
// state, which the next plan compares against. Update used to save the plan
// over it, as though the read-back had succeeded.
func TestRecordSetUpdateKeepsStateWhenReadBackFails(t *testing.T) {
	t.Parallel()
	routes := recordSetRoutes()
	routes["GET "+recordSetPath] = firstThen(routes["GET "+recordSetPath], badGateway)
	r := &recordSetResource{config: newFakeDesignate(t, routes).config}
	s := schemaOf(t, r)

	prior := activeRecordSet(t)
	planned := prior
	planned.Records = recordSetRecords(t, "10.1.0.2")
	priorState := newState(t, s, &prior)

	resp := runUpdate(r, newPlan(t, s, &planned), priorState)
	if !resp.Diagnostics.HasError() {
		t.Fatal("update succeeded; want the 502 on the read-back reported")
	}
	if !resp.State.Raw.Equal(priorState.Raw) {
		t.Fatalf("update state = %v, want the prior state %v", resp.State.Raw, priorState.Raw)
	}
}

// An update whose read-back finds the recordset gone must report it and keep
// the prior state. Update used to report nothing and save the plan.
func TestRecordSetUpdateKeepsStateWhenReadBack404s(t *testing.T) {
	t.Parallel()
	routes := recordSetRoutes()
	routes["GET "+recordSetPath] = firstThen(routes["GET "+recordSetPath], reply(http.StatusNotFound, ""))
	r := &recordSetResource{config: newFakeDesignate(t, routes).config}
	s := schemaOf(t, r)

	prior := activeRecordSet(t)
	planned := prior
	planned.Records = recordSetRecords(t, "10.1.0.2")
	priorState := newState(t, s, &prior)

	resp := runUpdate(r, newPlan(t, s, &planned), priorState)
	if !resp.Diagnostics.HasError() {
		t.Fatal("update succeeded; want the 404 on the read-back reported")
	}
	if !resp.State.Raw.Equal(priorState.Raw) {
		t.Fatalf("update state = %v, want the prior state %v", resp.State.Raw, priorState.Raw)
	}
}

// terraform destroy -refresh=false reaches Delete without Read dropping the
// v0.1.13 row first. Delete used to send DELETE for an empty ID, which reaches
// the zone's recordset collection URL. Delete must warn and send nothing, so
// Terraform forgets the row.
func TestRecordSetDeleteSkipsARowWithNoID(t *testing.T) {
	t.Parallel()
	routes := recordSetRoutes()
	routes["DELETE "+recordSetsPath+"/"] = reply(http.StatusMethodNotAllowed, "")
	designate := newFakeDesignate(t, routes)
	r := &recordSetResource{config: designate.config}

	resp := runDelete(r, recordSetRowWithoutID(t, r))
	if sent := designate.received(); len(sent) != 0 {
		t.Fatalf("delete of a row with no ID sent %v; it names nothing to delete", sent)
	}
	if resp.Diagnostics.HasError() || resp.Diagnostics.WarningsCount() != 1 {
		t.Fatalf("diagnostics = %v, want exactly one warning", resp.Diagnostics)
	}
}
