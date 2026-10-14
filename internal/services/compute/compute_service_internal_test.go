// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package compute

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/services"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

const (
	testHostID    = "6b1c3c7e-2f5d-4a8e-9b0c-1d2e3f4a5b6c"
	testServiceID = "4c7a2a7c-7a5e-4c0e-9d0f-2f6d6b1e3a10"
)

// fakeServicesNova serves os-services for one host. svc is the record GET
// lists (nil lists none); every PUT is recorded with its microversion header.
type fakeServicesNova struct {
	t     *testing.T
	mu    sync.Mutex
	svc   map[string]any
	lists int
	puts  []map[string]any
}

func newFakeServicesNova(t *testing.T, svc map[string]any) (*fakeServicesNova, *httptest.Server) {
	f := &fakeServicesNova{t: t, svc: svc}
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)
	return f, srv
}

// listCalls returns how many lists the fake has served. It reads under the
// lock: a client that gave up at its deadline may leave a handler running.
func (f *fakeServicesNova) listCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lists
}

func (f *fakeServicesNova) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	if got := r.Header.Get("OpenStack-API-Version"); got != "compute 2.53" {
		f.t.Errorf("%s %s sent OpenStack-API-Version %q, want \"compute 2.53\": the service UUID is only valid from 2.53", r.Method, r.URL.Path, got)
	}
	switch {
	case r.Method == "GET" && r.URL.Path == "/os-services":
		if q := r.URL.Query(); q.Get("binary") != "nova-compute" || q.Get("host") != testHostID {
			f.t.Errorf("list query = %s; want binary=nova-compute&host=%s", r.URL.RawQuery, testHostID)
		}
		f.lists++
		list := []any{}
		if f.svc != nil {
			list = append(list, f.svc)
		}
		out, _ := json.Marshal(map[string]any{"services": list})
		_, _ = w.Write(out)
	case r.Method == "PUT" && r.URL.Path == "/os-services/"+testServiceID:
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		if err := json.Unmarshal(raw, &body); err != nil {
			f.t.Errorf("PUT body: %v", err)
		}
		f.puts = append(f.puts, body)
		f.svc["status"] = body["status"]
		reason, _ := body["disabled_reason"].(string)
		if body["status"] == "enabled" {
			reason = ""
		}
		f.svc["disabled_reason"] = reason
		out, _ := json.Marshal(map[string]any{"service": f.svc})
		_, _ = w.Write(out)
	default:
		f.t.Errorf("unexpected request %s %s", r.Method, r.URL.String())
		w.WriteHeader(http.StatusNotImplemented)
	}
}

func serviceRecord(status, reason string) map[string]any {
	var r any
	if reason != "" {
		r = reason
	}
	return map[string]any{"id": testServiceID, "binary": "nova-compute", "host": testHostID, "zone": "cluster-1",
		"status": status, "state": "up", "disabled_reason": r, "forced_down": false, "updated_at": "2026-10-06T10:00:00.000000"}
}

func computeServiceTestResource(url string) *computeServiceResource {
	return &computeServiceResource{config: fakeConfig(url)}
}

func computeServiceSchema(ctx context.Context) (tfsdk.State, tfsdk.Plan) {
	var sch resource.SchemaResponse
	(&computeServiceResource{}).Schema(ctx, resource.SchemaRequest{}, &sch)
	null := tftypes.NewValue(sch.Schema.Type().TerraformType(ctx), nil)
	return tfsdk.State{Schema: sch.Schema, Raw: null}, tfsdk.Plan{Schema: sch.Schema, Raw: null}
}

func plannedService(status string) computeServiceModel {
	return computeServiceModel{ID: types.StringUnknown(), HostID: types.StringValue(testHostID), Status: types.StringValue(status),
		ServiceID: types.StringUnknown(), DisabledReason: types.StringUnknown(), ComputeState: types.StringUnknown()}
}

func TestServiceStatusOf(t *testing.T) {
	for _, tc := range []struct{ status, reason, want string }{
		{"enabled", "", "enabled"},
		{"disabled", "", "disabled"},
		{"disabled", "0b5a6c3e-1f2d-4a5b-8c7d-9e0f1a2b3c4d:1759744800000", "maintenance"},
		{"disabled", "kernel upgrade", "maintenance"}, // any reason reads as maintenance, as in the UI
	} {
		if got := serviceStatusOf(&services.Service{Status: tc.status, DisabledReason: tc.reason}); got != tc.want {
			t.Errorf("serviceStatusOf(%s, %q) = %s, want %s", tc.status, tc.reason, got, tc.want)
		}
	}
}

func TestServiceUpdateFor(t *testing.T) {
	now := time.UnixMilli(1759744800123)
	ui := "0b5a6c3e-1f2d-4a5b-8c7d-9e0f1a2b3c4d:1759744800000"
	for _, tc := range []struct {
		name           string
		status, reason string
		want           string
		wantBody       string // "" means no update
	}{
		{"enable a disabled host", "disabled", "", "enabled", `{"status":"enabled"}`},
		{"leave maintenance to enabled sends no reason", "disabled", ui, "enabled", `{"status":"enabled"}`},
		{"disable an enabled host", "enabled", "", "disabled", `{"status":"disabled"}`},
		{"maintenance to disabled clears the reason", "disabled", ui, "disabled", `{"status":"disabled"}`},
		{"already enabled", "enabled", "", "enabled", ""},
		{"already disabled", "disabled", "", "disabled", ""},
		{"maintenance keeps the reason it has", "disabled", ui, "maintenance", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts := serviceUpdateFor(&services.Service{Status: tc.status, DisabledReason: tc.reason}, tc.want, now)
			if tc.wantBody == "" {
				if opts != nil {
					t.Fatalf("update %+v; want none", *opts)
				}
				return
			}
			if opts == nil {
				t.Fatalf("no update; want %s", tc.wantBody)
			}
			body, err := opts.ToServiceUpdateMap()
			if err != nil {
				t.Fatal(err)
			}
			raw, _ := json.Marshal(body)
			if string(raw) != tc.wantBody {
				t.Fatalf("body = %s, want %s", raw, tc.wantBody)
			}
		})
	}
	for _, from := range []string{"enabled", "disabled"} {
		opts := serviceUpdateFor(&services.Service{Status: from}, "maintenance", now)
		if opts == nil || opts.Status != services.ServiceDisabled {
			t.Fatalf("maintenance from %s: %+v", from, opts)
		}
		id, ok := maintenanceID(opts.DisabledReason)
		if !ok || !strings.HasSuffix(opts.DisabledReason, ":1759744800123") || len(id) != 36 {
			t.Fatalf("maintenance reason %q; want <uuid>:<epoch ms> as the PCD UI writes it", opts.DisabledReason)
		}
	}
}

func TestMaintenanceID(t *testing.T) {
	if id, ok := maintenanceID("0b5a6c3e-1f2d-4a5b-8c7d-9e0f1a2b3c4d:1759744800000"); !ok || id != "0b5a6c3e-1f2d-4a5b-8c7d-9e0f1a2b3c4d" {
		t.Fatalf("got %q, %v", id, ok)
	}
	for _, r := range []string{"", "kernel upgrade", "0b5a6c3e-1f2d-4a5b-8c7d-9e0f1a2b3c4d", "x:1"} {
		if _, ok := maintenanceID(r); ok {
			t.Errorf("maintenanceID(%q) matched", r)
		}
	}
}

// Create enters maintenance with a UI-style reason over the 2.53 UUID API and
// records the service as Nova reports it.
func TestComputeServiceCreateMaintenance(t *testing.T) {
	ctx := context.Background()
	f, srv := newFakeServicesNova(t, serviceRecord("enabled", ""))
	r := computeServiceTestResource(srv.URL)
	state, plan := computeServiceSchema(ctx)
	planned := plannedService("maintenance")
	if d := plan.Set(ctx, &planned); d.HasError() {
		t.Fatal(d)
	}
	resp := resource.CreateResponse{State: state}
	r.Create(ctx, resource.CreateRequest{Plan: plan}, &resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("create: %v", resp.Diagnostics)
	}
	if len(f.puts) != 1 || f.puts[0]["status"] != "disabled" {
		t.Fatalf("PUTs = %v; want one disable", f.puts)
	}
	if _, ok := maintenanceID(f.puts[0]["disabled_reason"].(string)); !ok {
		t.Fatalf("reason %v is not <uuid>:<epoch ms>; the PCD UI would not show maintenance progress", f.puts[0]["disabled_reason"])
	}
	var got computeServiceModel
	if d := resp.State.Get(ctx, &got); d.HasError() {
		t.Fatal(d)
	}
	if got.Status.ValueString() != "maintenance" || got.ServiceID.ValueString() != testServiceID ||
		got.ID.ValueString() != testHostID || got.ComputeState.ValueString() != "up" || got.DisabledReason.IsNull() {
		t.Fatalf("state = %+v", got)
	}
}

// Create on a host whose service is already where the plan wants it sends no
// PUT: Nova answers 400 to an update that changes nothing.
func TestComputeServiceCreateNoChange(t *testing.T) {
	ctx := context.Background()
	f, srv := newFakeServicesNova(t, serviceRecord("enabled", ""))
	r := computeServiceTestResource(srv.URL)
	state, plan := computeServiceSchema(ctx)
	planned := plannedService("enabled")
	if d := plan.Set(ctx, &planned); d.HasError() {
		t.Fatal(d)
	}
	resp := resource.CreateResponse{State: state}
	r.Create(ctx, resource.CreateRequest{Plan: plan}, &resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("create: %v", resp.Diagnostics)
	}
	if len(f.puts) != 0 {
		t.Fatalf("PUTs = %v; want none", f.puts)
	}
	var got computeServiceModel
	_ = resp.State.Get(ctx, &got)
	if !got.DisabledReason.IsNull() {
		t.Fatalf("disabled_reason = %s; want null for an enabled service", got.DisabledReason)
	}
}

// Create waits for a hypervisor whose service has not registered yet, and
// gives up with a hint that names wait_until_converged.
func TestComputeServiceCreateWaitsForRegistration(t *testing.T) {
	oldI, oldT := computeServicePollInterval, computeServiceRegisterTimeout
	computeServicePollInterval, computeServiceRegisterTimeout = time.Millisecond, 50*time.Millisecond
	t.Cleanup(func() { computeServicePollInterval, computeServiceRegisterTimeout = oldI, oldT })
	ctx := context.Background()
	f, srv := newFakeServicesNova(t, nil)
	r := computeServiceTestResource(srv.URL)
	state, plan := computeServiceSchema(ctx)
	planned := plannedService("disabled")
	if d := plan.Set(ctx, &planned); d.HasError() {
		t.Fatal(d)
	}
	resp := resource.CreateResponse{State: state}
	r.Create(ctx, resource.CreateRequest{Plan: plan}, &resp)
	if !resp.Diagnostics.HasError() || !strings.Contains(resp.Diagnostics[0].Detail(), "wait_until_converged") {
		t.Fatalf("diags = %v; want a timeout naming wait_until_converged", resp.Diagnostics)
	}
	if n := f.listCalls(); n < 2 {
		t.Fatalf("listed %d times; want polling until the timeout", n)
	}
}

func TestComputeServiceRead(t *testing.T) {
	ctx := context.Background()
	read := func(t *testing.T, svc map[string]any) resource.ReadResponse {
		t.Helper()
		_, srv := newFakeServicesNova(t, svc)
		r := computeServiceTestResource(srv.URL)
		state, _ := computeServiceSchema(ctx)
		prior := computeServiceModel{ID: types.StringValue(testHostID), HostID: types.StringValue(testHostID),
			Status: types.StringValue("enabled"), ServiceID: types.StringValue(testServiceID),
			DisabledReason: types.StringNull(), ComputeState: types.StringValue("up")}
		if d := state.Set(ctx, &prior); d.HasError() {
			t.Fatal(d)
		}
		resp := resource.ReadResponse{State: state}
		r.Read(ctx, resource.ReadRequest{State: state}, &resp)
		return resp
	}
	t.Run("maintenance set in the UI shows as drift", func(t *testing.T) {
		resp := read(t, serviceRecord("disabled", "0b5a6c3e-1f2d-4a5b-8c7d-9e0f1a2b3c4d:1759744800000"))
		var got computeServiceModel
		if d := resp.State.Get(ctx, &got); d.HasError() {
			t.Fatal(d)
		}
		if got.Status.ValueString() != "maintenance" || got.DisabledReason.ValueString() != "0b5a6c3e-1f2d-4a5b-8c7d-9e0f1a2b3c4d:1759744800000" {
			t.Fatalf("state = %+v", got)
		}
	})
	t.Run("a host without a compute service leaves state", func(t *testing.T) {
		resp := read(t, nil)
		if resp.Diagnostics.HasError() || !resp.State.Raw.IsNull() {
			t.Fatalf("diags=%v state=%v", resp.Diagnostics, resp.State.Raw)
		}
	})
}

// Destroy re-enables a disabled host; a host already enabled, or one whose
// service is gone, needs no call.
func TestComputeServiceDelete(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name     string
		svc      map[string]any
		wantPuts int
	}{
		{"maintenance is lifted", serviceRecord("disabled", "0b5a6c3e-1f2d-4a5b-8c7d-9e0f1a2b3c4d:1759744800000"), 1},
		{"enabled stays", serviceRecord("enabled", ""), 0},
		{"gone is fine", nil, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, srv := newFakeServicesNova(t, tc.svc)
			r := computeServiceTestResource(srv.URL)
			state, _ := computeServiceSchema(ctx)
			prior := computeServiceModel{ID: types.StringValue(testHostID), HostID: types.StringValue(testHostID),
				Status: types.StringValue("maintenance"), ServiceID: types.StringValue(testServiceID),
				DisabledReason: types.StringNull(), ComputeState: types.StringValue("up")}
			if d := state.Set(ctx, &prior); d.HasError() {
				t.Fatal(d)
			}
			resp := resource.DeleteResponse{State: state}
			r.Delete(ctx, resource.DeleteRequest{State: state}, &resp)
			if resp.Diagnostics.HasError() {
				t.Fatalf("delete: %v", resp.Diagnostics)
			}
			if len(f.puts) != tc.wantPuts {
				t.Fatalf("PUTs = %v, want %d", f.puts, tc.wantPuts)
			}
			if tc.wantPuts == 1 {
				if raw, _ := json.Marshal(f.puts[0]); string(raw) != `{"status":"enabled"}` {
					t.Fatalf("PUT body = %s; Nova rejects a reason with enabled", raw)
				}
			}
		})
	}
}

func TestLookupComputeServiceRejectsDuplicates(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		rec, _ := json.Marshal(serviceRecord("enabled", ""))
		fmt.Fprintf(w, `{"services": [%s, %s]}`, rec, rec)
	}))
	t.Cleanup(srv.Close)
	client := &gophercloud.ServiceClient{
		ProviderClient: &gophercloud.ProviderClient{HTTPClient: http.Client{Transport: &http.Transport{}}},
		Endpoint:       srv.URL + "/",
		Type:           "compute",
	}
	if _, err := lookupComputeService(context.Background(), client, testHostID); err == nil || !strings.Contains(err.Error(), "expected one") {
		t.Fatalf("err = %v", err)
	}
}

// An empty host ID is refused before any request: a list without a host
// filter would return every hypervisor's nova-compute service, and Create
// would then change whichever one it got.
func TestLookupComputeServiceRefusesEmptyHostID(t *testing.T) {
	f, srv := newFakeServicesNova(t, serviceRecord("enabled", ""))
	client, err := fakeConfig(srv.URL).ComputeV2Client()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lookupComputeService(context.Background(), client, ""); err == nil || !strings.Contains(err.Error(), "needs a host ID") {
		t.Fatalf("err = %v; want the empty host ID refused", err)
	}
	if n := f.listCalls(); n != 0 {
		t.Fatalf("listed %d times; want no request for an empty host ID", n)
	}
}

// host_id = "" fails validation, so a plan never reaches Create with it. A
// host_id known only at apply, such as pcd_host_cluster_role's, passes.
func TestComputeServiceSchemaRefusesEmptyHostID(t *testing.T) {
	attr, ok := schemaOf(t, &computeServiceResource{}).Attributes["host_id"].(schema.StringAttribute)
	if !ok {
		t.Fatal("host_id is not a string attribute")
	}
	for _, tc := range []struct {
		value   types.String
		wantErr bool
	}{
		{types.StringValue(""), true},
		{types.StringValue(testHostID), false},
		{types.StringUnknown(), false},
	} {
		req := validator.StringRequest{Path: path.Root("host_id"), ConfigValue: tc.value}
		var resp validator.StringResponse
		for _, v := range attr.StringValidators() {
			v.ValidateString(context.Background(), req, &resp)
		}
		if resp.Diagnostics.HasError() != tc.wantErr {
			t.Fatalf("host_id %s: diags = %v; want an error: %v", tc.value, resp.Diagnostics, tc.wantErr)
		}
		if tc.wantErr && resp.Diagnostics[0].Summary() != "Invalid Attribute Value Length" {
			t.Fatalf("host_id %s: summary %q; want the length validator's", tc.value, resp.Diagnostics[0].Summary())
		}
	}
}

// A row with no host ID names no service: listing with an empty host would
// return every nova-compute service. Read drops the row and Delete skips it,
// without a request (PR #69's state guards).
func TestComputeServiceRowWithoutID(t *testing.T) {
	ctx := context.Background()
	f := newFakeNova(t, novaRoutes{})
	r := &computeServiceResource{config: f.config}
	state, _ := computeServiceSchema(ctx)
	prior := computeServiceModel{ID: types.StringNull(), HostID: types.StringNull(), Status: types.StringValue("enabled"),
		ServiceID: types.StringNull(), DisabledReason: types.StringNull(), ComputeState: types.StringNull()}
	if d := state.Set(ctx, &prior); d.HasError() {
		t.Fatal(d)
	}
	read := runRead(r, state)
	if read.Diagnostics.HasError() || len(read.Diagnostics) != 1 || !read.State.Raw.IsNull() {
		t.Fatalf("read: diags=%v state=%v; want one warning and no state", read.Diagnostics, read.State.Raw)
	}
	if del := runDelete(r, state); del.Diagnostics.HasError() {
		t.Fatalf("delete: %v", del.Diagnostics)
	}
	if got := f.received(); len(got) != 0 {
		t.Fatalf("requests = %v; want none for a row without an ID", got)
	}
}

// Update from maintenance to disabled clears the reason: the host stays out
// of scheduling but no longer shows as in maintenance in the PCD UI.
func TestComputeServiceUpdateMaintenanceToDisabled(t *testing.T) {
	ctx := context.Background()
	f, srv := newFakeServicesNova(t, serviceRecord("disabled", "0b5a6c3e-1f2d-4a5b-8c7d-9e0f1a2b3c4d:1759744800000"))
	r := computeServiceTestResource(srv.URL)
	state, plan := computeServiceSchema(ctx)
	prior := computeServiceModel{ID: types.StringValue(testHostID), HostID: types.StringValue(testHostID),
		Status: types.StringValue("maintenance"), ServiceID: types.StringValue(testServiceID),
		DisabledReason: types.StringValue("0b5a6c3e-1f2d-4a5b-8c7d-9e0f1a2b3c4d:1759744800000"), ComputeState: types.StringValue("up")}
	planned := prior
	planned.Status = types.StringValue("disabled")
	planned.DisabledReason = types.StringUnknown()
	planned.ComputeState = types.StringUnknown()
	if d := state.Set(ctx, &prior); d.HasError() {
		t.Fatal(d)
	}
	if d := plan.Set(ctx, &planned); d.HasError() {
		t.Fatal(d)
	}
	resp := resource.UpdateResponse{State: state}
	r.Update(ctx, resource.UpdateRequest{Plan: plan, State: state}, &resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("update: %v", resp.Diagnostics)
	}
	if raw, _ := json.Marshal(f.puts); string(raw) != `[{"status":"disabled"}]` {
		t.Fatalf("PUTs = %s, want one {\"status\":\"disabled\"}", raw)
	}
	var got computeServiceModel
	if d := resp.State.Get(ctx, &got); d.HasError() {
		t.Fatal(d)
	}
	if got.Status.ValueString() != "disabled" || !got.DisabledReason.IsNull() {
		t.Fatalf("state = %+v; want disabled with no reason", got)
	}
}
