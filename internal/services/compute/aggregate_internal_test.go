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
	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/aggregates"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

// fakeAggregateNova is an httptest Nova for os-aggregates. It records every
// call as "METHOD path [action]" so tests can assert order, and keeps the
// aggregate's hosts and metadata so GET reflects what the actions did.
type fakeAggregateNova struct {
	t       *testing.T
	mu      sync.Mutex
	calls   []string
	bodies  map[string][]map[string]any // action name -> request bodies
	agg     map[string]any              // the aggregate GET returns; nil means 404
	addHost func(host string, n int) (int, string)
	adds    int
}

func newFakeAggregateNova(t *testing.T, agg map[string]any) (*fakeAggregateNova, *httptest.Server) {
	f := &fakeAggregateNova{t: t, bodies: map[string][]map[string]any{}, agg: agg}
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)
	return f, srv
}

func (f *fakeAggregateNova) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	var body map[string]any
	if r.Body != nil {
		raw, _ := io.ReadAll(r.Body)
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &body); err != nil {
				f.t.Errorf("bad JSON body on %s %s: %v", r.Method, r.URL.Path, err)
			}
		}
	}
	key := r.Method + " " + r.URL.Path
	switch key {
	case "POST /os-aggregates":
		f.calls = append(f.calls, key)
		in := body["aggregate"].(map[string]any)
		f.agg = map[string]any{"id": 7, "name": in["name"], "availability_zone": in["availability_zone"], "hosts": []any{}, "metadata": map[string]any{}}
		if az, ok := in["availability_zone"].(string); ok && az != "" {
			f.agg["metadata"].(map[string]any)["availability_zone"] = az
		}
		writeAggregate(w, f.agg)
	case "GET /os-aggregates/7":
		f.calls = append(f.calls, key)
		if f.agg == nil {
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"itemNotFound": {"code": 404, "message": "Aggregate 7 could not be found."}}`)
			return
		}
		writeAggregate(w, f.agg)
	case "PUT /os-aggregates/7":
		f.calls = append(f.calls, key)
		f.bodies["update"] = append(f.bodies["update"], body)
		in := body["aggregate"].(map[string]any)
		for k, v := range in {
			f.agg[k] = v
		}
		writeAggregate(w, f.agg)
	case "DELETE /os-aggregates/7":
		f.calls = append(f.calls, key)
		f.agg = nil
		w.WriteHeader(http.StatusOK)
	case "POST /os-aggregates/7/action":
		for action, args := range body {
			a := args.(map[string]any)
			f.bodies[action] = append(f.bodies[action], a)
			switch action {
			case "add_host":
				host := a["host"].(string)
				f.calls = append(f.calls, key+" add_host "+host)
				f.adds++
				if f.addHost != nil {
					if code, msg := f.addHost(host, f.adds); code != http.StatusOK {
						w.WriteHeader(code)
						fmt.Fprint(w, msg)
						return
					}
				}
				f.agg["hosts"] = append(f.agg["hosts"].([]any), host)
			case "remove_host":
				host := a["host"].(string)
				f.calls = append(f.calls, key+" remove_host "+host)
				kept := []any{}
				for _, h := range f.agg["hosts"].([]any) {
					if h != host {
						kept = append(kept, h)
					}
				}
				f.agg["hosts"] = kept
			case "set_metadata":
				f.calls = append(f.calls, key+" set_metadata")
				meta := f.agg["metadata"].(map[string]any)
				for k, v := range a["metadata"].(map[string]any) {
					if v == nil {
						delete(meta, k)
					} else {
						meta[k] = v
					}
				}
			default:
				f.t.Errorf("unexpected aggregate action %q", action)
			}
		}
		writeAggregate(w, f.agg)
	default:
		f.t.Errorf("unexpected request %s", key)
		w.WriteHeader(http.StatusNotImplemented)
	}
}

func writeAggregate(w http.ResponseWriter, agg map[string]any) {
	out, _ := json.Marshal(map[string]any{"aggregate": agg})
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(out)
}

func (f *fakeAggregateNova) callLog() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

// aggregateTestResource builds the resource against a fake through PR #69's
// fakeConfig, which gives the client a transport of its own.
func aggregateTestResource(url string) *aggregateResource {
	return &aggregateResource{config: fakeConfig(url)}
}

func stringSet(t *testing.T, vals ...string) types.Set {
	t.Helper()
	elems := make([]attr.Value, 0, len(vals))
	for _, v := range vals {
		elems = append(elems, types.StringValue(v))
	}
	s, d := types.SetValue(types.StringType, elems)
	if d.HasError() {
		t.Fatal(d)
	}
	return s
}

func aggregateSchemaState(ctx context.Context, r *aggregateResource) (tfsdk.State, tfsdk.Plan) {
	var sch resource.SchemaResponse
	r.Schema(ctx, resource.SchemaRequest{}, &sch)
	null := tftypes.NewValue(sch.Schema.Type().TerraformType(ctx), nil)
	return tfsdk.State{Schema: sch.Schema, Raw: null}, tfsdk.Plan{Schema: sch.Schema, Raw: null}
}

func equalCalls(t *testing.T, got, want []string) {
	t.Helper()
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("calls:\n  %s\nwant:\n  %s", strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}
}

// Create makes the aggregate, adds hosts one at a time in a stable order,
// then sets metadata once; the zone Nova mirrors into metadata stays out of
// the metadata attribute.
func TestAggregateCreateAddsHostsThenMetadata(t *testing.T) {
	ctx := context.Background()
	f, srv := newFakeAggregateNova(t, nil)
	r := aggregateTestResource(srv.URL)
	state, plan := aggregateSchemaState(ctx, r)
	planned := aggregateModel{
		ID: types.StringUnknown(), Name: types.StringValue("fd-a"), Zone: types.StringValue("cluster-1"),
		Metadata: stringMap(t, map[string]string{"pf9_failure_domain": "true", "pf9_fd_label": "A"}),
		Hosts:    stringSet(t, "host-b", "host-a"), Region: types.StringUnknown(),
	}
	if d := plan.Set(ctx, &planned); d.HasError() {
		t.Fatal(d)
	}
	resp := resource.CreateResponse{State: state}
	r.Create(ctx, resource.CreateRequest{Plan: plan}, &resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("create: %v", resp.Diagnostics)
	}
	equalCalls(t, f.callLog(), []string{
		"POST /os-aggregates",
		"POST /os-aggregates/7/action add_host host-a",
		"POST /os-aggregates/7/action add_host host-b",
		"POST /os-aggregates/7/action set_metadata",
		"GET /os-aggregates/7",
	})
	var got aggregateModel
	if d := resp.State.Get(ctx, &got); d.HasError() {
		t.Fatal(d)
	}
	if got.ID.ValueString() != "7" || got.Zone.ValueString() != "cluster-1" || got.Region.ValueString() != "region-one" {
		t.Fatalf("id=%s zone=%s region=%s; want 7, cluster-1, region-one", got.ID, got.Zone, got.Region)
	}
	if !got.Hosts.Equal(stringSet(t, "host-a", "host-b")) {
		t.Fatalf("hosts = %s", got.Hosts)
	}
	if !got.Metadata.Equal(stringMap(t, map[string]string{"pf9_failure_domain": "true", "pf9_fd_label": "A"})) {
		t.Fatalf("metadata = %s; availability_zone must not leak in from Nova's mirror", got.Metadata)
	}
}

// A host that fails to join (here a zone conflict) fails Create, but the
// aggregate Nova already made stays in state so Terraform taints it and the
// next apply deletes it, instead of failing forever on the taken name.
func TestAggregateCreateKeepsAggregateWhenAddHostFails(t *testing.T) {
	ctx := context.Background()
	f, srv := newFakeAggregateNova(t, nil)
	f.addHost = func(string, int) (int, string) {
		return http.StatusConflict, `{"conflictingRequest": {"code": 409, "message": "Cannot add host to aggregate 7. Reason: One or more hosts already in availability zone(s) ['other']."}}`
	}
	r := aggregateTestResource(srv.URL)
	state, plan := aggregateSchemaState(ctx, r)
	planned := aggregateModel{
		ID: types.StringUnknown(), Name: types.StringValue("agg"), Zone: types.StringValue("cluster-1"),
		Metadata: stringMap(t, map[string]string{}), Hosts: stringSet(t, "host-a"), Region: types.StringUnknown(),
	}
	if d := plan.Set(ctx, &planned); d.HasError() {
		t.Fatal(d)
	}
	resp := resource.CreateResponse{State: state}
	r.Create(ctx, resource.CreateRequest{Plan: plan}, &resp)
	if !resp.Diagnostics.HasError() {
		t.Fatal("create succeeded; want the add_host conflict reported")
	}
	if resp.State.Raw.IsNull() || !resp.State.Raw.IsFullyKnown() {
		t.Fatalf("state after a failed add_host = %v; want the created aggregate recorded with known values", resp.State.Raw)
	}
	var got aggregateModel
	if d := resp.State.Get(ctx, &got); d.HasError() {
		t.Fatal(d)
	}
	if got.ID.ValueString() != "7" || !got.Hosts.Equal(stringSet(t)) {
		t.Fatalf("recorded id=%s hosts=%s; want 7 and no hosts (what Nova holds)", got.ID, got.Hosts)
	}
}

func shortHostRetry(t *testing.T, interval, timeout time.Duration) {
	t.Helper()
	oldI, oldT := aggregateHostRetryInterval, aggregateHostRetryTimeout
	aggregateHostRetryInterval, aggregateHostRetryTimeout = interval, timeout
	t.Cleanup(func() { aggregateHostRetryInterval, aggregateHostRetryTimeout = oldI, oldT })
}

// bareCompute is a compute client for the fake with a transport of its own:
// closing a test server closes the default transport's idle connections.
func bareCompute(url string) *gophercloud.ServiceClient {
	return &gophercloud.ServiceClient{
		ProviderClient: &gophercloud.ProviderClient{HTTPClient: http.Client{Transport: &http.Transport{}}},
		Endpoint:       url + "/",
	}
}

const computeHostNotFound = `{"itemNotFound": {"code": 404, "message": "Compute host host-a could not be found."}}`

func TestAddAggregateHost(t *testing.T) {
	shortHostRetry(t, time.Millisecond, 200*time.Millisecond)
	ctx := context.Background()

	t.Run("retries while the compute host is unregistered", func(t *testing.T) {
		f, srv := newFakeAggregateNova(t, map[string]any{"id": 7, "name": "a", "hosts": []any{}, "metadata": map[string]any{}})
		f.addHost = func(_ string, n int) (int, string) {
			if n < 3 {
				return http.StatusNotFound, computeHostNotFound
			}
			return http.StatusOK, ""
		}
		if err := addAggregateHost(ctx, bareCompute(srv.URL), 7, "host-a"); err != nil {
			t.Fatalf("addAggregateHost: %v", err)
		}
		if f.adds != 3 {
			t.Fatalf("add_host called %d times, want 3", f.adds)
		}
	})
	t.Run("does not retry a missing aggregate", func(t *testing.T) {
		f, srv := newFakeAggregateNova(t, map[string]any{"id": 7, "name": "a", "hosts": []any{}, "metadata": map[string]any{}})
		f.addHost = func(string, int) (int, string) {
			return http.StatusNotFound, `{"itemNotFound": {"code": 404, "message": "Aggregate 7 could not be found."}}`
		}
		if err := addAggregateHost(ctx, bareCompute(srv.URL), 7, "host-a"); err == nil {
			t.Fatal("want an error for a deleted aggregate")
		}
		if f.adds != 1 {
			t.Fatalf("add_host called %d times, want 1: AggregateNotFound is also a 404 and must not be retried", f.adds)
		}
	})
	t.Run("a 500 after Nova recorded the host is success", func(t *testing.T) {
		f, srv := newFakeAggregateNova(t, map[string]any{"id": 7, "name": "a", "hosts": []any{"host-a"}, "metadata": map[string]any{}})
		f.addHost = func(string, int) (int, string) {
			return http.StatusInternalServerError, `{"computeFault": {"code": 500}}`
		}
		if err := addAggregateHost(ctx, bareCompute(srv.URL), 7, "host-a"); err != nil {
			t.Fatalf("addAggregateHost: %v; PCD's Nova commits the host before the Placement step that failed", err)
		}
	})
	t.Run("a 500 without the host is an error", func(t *testing.T) {
		f, srv := newFakeAggregateNova(t, map[string]any{"id": 7, "name": "a", "hosts": []any{}, "metadata": map[string]any{}})
		f.addHost = func(string, int) (int, string) {
			return http.StatusInternalServerError, `{"computeFault": {"code": 500}}`
		}
		if err := addAggregateHost(ctx, bareCompute(srv.URL), 7, "host-a"); err == nil {
			t.Fatal("want an error")
		}
	})
	t.Run("gives up with the wait_until_converged hint", func(t *testing.T) {
		f, srv := newFakeAggregateNova(t, map[string]any{"id": 7, "name": "a", "hosts": []any{}, "metadata": map[string]any{}})
		f.addHost = func(string, int) (int, string) { return http.StatusNotFound, computeHostNotFound }
		err := addAggregateHost(ctx, bareCompute(srv.URL), 7, "host-a")
		if err == nil || !strings.Contains(err.Error(), "wait_until_converged") {
			t.Fatalf("err = %v; want the timeout to point at wait_until_converged", err)
		}
	})
}

// Update removes hosts first, then renames or re-zones, then adds hosts, then
// sends one set_metadata where removed keys are null.
func TestAggregateUpdateOrder(t *testing.T) {
	ctx := context.Background()
	f, srv := newFakeAggregateNova(t, map[string]any{
		"id": 7, "name": "old", "availability_zone": "zone-1", "hosts": []any{"host-old"},
		"metadata": map[string]any{"availability_zone": "zone-1", "keep": "1", "gone": "x"},
	})
	r := aggregateTestResource(srv.URL)
	state, plan := aggregateSchemaState(ctx, r)
	prior := aggregateModel{
		ID: types.StringValue("7"), Name: types.StringValue("old"), Zone: types.StringValue("zone-1"),
		Metadata: stringMap(t, map[string]string{"keep": "1", "gone": "x"}), Hosts: stringSet(t, "host-old"),
		Region: types.StringValue("region-one"),
	}
	planned := prior
	planned.Name = types.StringValue("new")
	planned.Zone = types.StringValue("zone-2")
	planned.Hosts = stringSet(t, "host-new")
	planned.Metadata = stringMap(t, map[string]string{"keep": "1", "added": "y"})
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
	equalCalls(t, f.callLog(), []string{
		"POST /os-aggregates/7/action remove_host host-old",
		"PUT /os-aggregates/7",
		"POST /os-aggregates/7/action add_host host-new",
		"POST /os-aggregates/7/action set_metadata",
		"GET /os-aggregates/7",
	})
	put := f.bodies["update"][0]["aggregate"].(map[string]any)
	if put["name"] != "new" || put["availability_zone"] != "zone-2" {
		t.Fatalf("PUT body = %v", put)
	}
	meta := f.bodies["set_metadata"][0]["metadata"].(map[string]any)
	if v, ok := meta["gone"]; !ok || v != nil {
		t.Fatalf("set_metadata = %v; a removed key must be sent as null so Nova deletes it", meta)
	}
	if meta["added"] != "y" {
		t.Fatalf("set_metadata = %v; the new key is missing", meta)
	}
}

// A rename alone sends only the name; nothing else is touched.
func TestAggregateUpdateRenameOnly(t *testing.T) {
	ctx := context.Background()
	f, srv := newFakeAggregateNova(t, map[string]any{"id": 7, "name": "old", "hosts": []any{}, "metadata": map[string]any{}})
	r := aggregateTestResource(srv.URL)
	state, plan := aggregateSchemaState(ctx, r)
	prior := aggregateModel{ID: types.StringValue("7"), Name: types.StringValue("old"), Zone: types.StringNull(),
		Metadata: stringMap(t, map[string]string{}), Hosts: stringSet(t), Region: types.StringValue("region-one")}
	planned := prior
	planned.Name = types.StringValue("new")
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
	equalCalls(t, f.callLog(), []string{"PUT /os-aggregates/7", "GET /os-aggregates/7"})
	if _, sent := f.bodies["update"][0]["aggregate"].(map[string]any)["availability_zone"]; sent {
		t.Fatalf("PUT body %v carries availability_zone; Nova rejects an empty zone", f.bodies["update"][0])
	}
}

func TestAggregateMetadataUpdate(t *testing.T) {
	got := aggregateMetadataUpdate(map[string]string{"a": "1", "b": "2"}, map[string]string{"a": "1", "c": "3"})
	if len(got) != 3 || got["a"] != "1" || got["c"] != "3" {
		t.Fatalf("got %v", got)
	}
	if v, ok := got["b"]; !ok || v != nil {
		t.Fatalf("removed key b = %v, %v; want present and nil", v, ok)
	}
	if got := aggregateMetadataUpdate(nil, map[string]string{"x": "1"}); len(got) != 1 || got["x"] != "1" {
		t.Fatalf("create body = %v", got)
	}
}

// Delete strips the hosts the aggregate has now, including one added outside
// Terraform, because Nova refuses to delete an aggregate with hosts.
func TestAggregateDeleteRemovesLiveHosts(t *testing.T) {
	ctx := context.Background()
	f, srv := newFakeAggregateNova(t, map[string]any{"id": 7, "name": "a", "hosts": []any{"host-tf", "host-ui"}, "metadata": map[string]any{}})
	r := aggregateTestResource(srv.URL)
	state, _ := aggregateSchemaState(ctx, r)
	prior := aggregateModel{ID: types.StringValue("7"), Name: types.StringValue("a"), Zone: types.StringNull(),
		Metadata: stringMap(t, map[string]string{}), Hosts: stringSet(t, "host-tf"), Region: types.StringValue("region-one")}
	if d := state.Set(ctx, &prior); d.HasError() {
		t.Fatal(d)
	}
	resp := resource.DeleteResponse{State: state}
	r.Delete(ctx, resource.DeleteRequest{State: state}, &resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("delete: %v", resp.Diagnostics)
	}
	equalCalls(t, f.callLog(), []string{
		"GET /os-aggregates/7",
		"POST /os-aggregates/7/action remove_host host-tf",
		"POST /os-aggregates/7/action remove_host host-ui",
		"DELETE /os-aggregates/7",
	})
}

func TestAggregateDeleteAlreadyGone(t *testing.T) {
	ctx := context.Background()
	f, srv := newFakeAggregateNova(t, nil)
	r := aggregateTestResource(srv.URL)
	state, _ := aggregateSchemaState(ctx, r)
	prior := aggregateModel{ID: types.StringValue("7"), Name: types.StringValue("a"), Zone: types.StringNull(),
		Metadata: stringMap(t, map[string]string{}), Hosts: stringSet(t), Region: types.StringValue("region-one")}
	if d := state.Set(ctx, &prior); d.HasError() {
		t.Fatal(d)
	}
	resp := resource.DeleteResponse{State: state}
	r.Delete(ctx, resource.DeleteRequest{State: state}, &resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("delete of a gone aggregate: %v", resp.Diagnostics)
	}
	equalCalls(t, f.callLog(), []string{"GET /os-aggregates/7"})
}

func TestAggregateRead(t *testing.T) {
	ctx := context.Background()
	read := func(t *testing.T, agg map[string]any) resource.ReadResponse {
		t.Helper()
		_, srv := newFakeAggregateNova(t, agg)
		r := aggregateTestResource(srv.URL)
		state, _ := aggregateSchemaState(ctx, r)
		prior := aggregateModel{ID: types.StringValue("7"), Name: types.StringNull(), Zone: types.StringNull(),
			Metadata: types.MapNull(types.StringType), Hosts: types.SetNull(types.StringType), Region: types.StringNull()}
		if d := state.Set(ctx, &prior); d.HasError() {
			t.Fatal(d)
		}
		resp := resource.ReadResponse{State: state}
		r.Read(ctx, resource.ReadRequest{State: state}, &resp)
		return resp
	}
	t.Run("gone removes it from state", func(t *testing.T) {
		resp := read(t, nil)
		if resp.Diagnostics.HasError() || !resp.State.Raw.IsNull() {
			t.Fatalf("diags=%v state=%v; want no error and no state", resp.Diagnostics, resp.State.Raw)
		}
	})
	t.Run("a cluster aggregate is refused", func(t *testing.T) {
		resp := read(t, map[string]any{"id": 7, "name": "cluster-1", "availability_zone": "cluster-1", "hosts": []any{},
			"metadata": map[string]any{"pf9-managed": "true", "availability_zone": "cluster-1"}})
		if !resp.Diagnostics.HasError() || !strings.Contains(resp.Diagnostics[0].Detail(), "pcd_cluster") {
			t.Fatalf("diags = %v; want an error pointing at pcd_cluster", resp.Diagnostics)
		}
	})
	t.Run("the compute aggregate is refused", func(t *testing.T) {
		resp := read(t, map[string]any{"id": 7, "name": "compute", "hosts": []any{}, "metadata": map[string]any{}})
		if !resp.Diagnostics.HasError() {
			t.Fatal("want an error for the aggregate named compute")
		}
	})
	t.Run("an aggregate without a zone reads a null zone", func(t *testing.T) {
		resp := read(t, map[string]any{"id": 7, "name": "a", "availability_zone": nil, "hosts": nil, "metadata": map[string]any{}})
		var got aggregateModel
		if d := resp.State.Get(ctx, &got); d.HasError() {
			t.Fatal(d)
		}
		if !got.Zone.IsNull() || !got.Hosts.Equal(stringSet(t)) {
			t.Fatalf("zone=%s hosts=%s; want null and an empty set", got.Zone, got.Hosts)
		}
	})
}

// A row with no ID names no aggregate: Read drops it with a warning and
// Delete skips it, and neither sends a request (PR #69's state guards).
func TestAggregateRowWithoutID(t *testing.T) {
	ctx := context.Background()
	f := newFakeNova(t, novaRoutes{})
	r := &aggregateResource{config: f.config}
	state, _ := aggregateSchemaState(ctx, r)
	prior := aggregateModel{ID: types.StringNull(), Name: types.StringValue("a"), Zone: types.StringNull(),
		Metadata: types.MapNull(types.StringType), Hosts: types.SetNull(types.StringType), Region: types.StringNull()}
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

// A 200 that holds no aggregate object is an error, not a missing aggregate:
// dropping the row would lose an aggregate that exists.
func TestAggregateReadWithoutObject(t *testing.T) {
	ctx := context.Background()
	f := newFakeNova(t, novaRoutes{"GET /os-aggregates/7": reply(http.StatusOK, `{}`)})
	r := &aggregateResource{config: f.config}
	state, _ := aggregateSchemaState(ctx, r)
	prior := aggregateModel{ID: types.StringValue("7"), Name: types.StringValue("a"), Zone: types.StringNull(),
		Metadata: stringMap(t, map[string]string{}), Hosts: stringSet(t), Region: types.StringValue("region-one")}
	if d := state.Set(ctx, &prior); d.HasError() {
		t.Fatal(d)
	}
	resp := runRead(r, state)
	if !resp.Diagnostics.HasError() || resp.State.Raw.IsNull() {
		t.Fatalf("diags=%v state=%v; want an error and the row kept", resp.Diagnostics, resp.State.Raw)
	}
}

func TestAggregateImportStateRejectsNonInteger(t *testing.T) {
	ctx := context.Background()
	r := &aggregateResource{}
	state, _ := aggregateSchemaState(ctx, r)
	for _, id := range []string{"abc", "-1", ""} {
		resp := resource.ImportStateResponse{State: state}
		r.ImportState(ctx, resource.ImportStateRequest{ID: id}, &resp)
		if !resp.Diagnostics.HasError() {
			t.Errorf("import ID %q accepted; want an error", id)
		}
	}
	resp := resource.ImportStateResponse{State: state}
	r.ImportState(ctx, resource.ImportStateRequest{ID: "42"}, &resp)
	var id types.String
	resp.Diagnostics.Append(resp.State.GetAttribute(ctx, path.Root("id"), &id)...)
	if resp.Diagnostics.HasError() || id.ValueString() != "42" {
		t.Fatalf("import 42: id=%s diags=%v", id, resp.Diagnostics)
	}
}

func TestAggregateValidateConfigRejectsReservedKeys(t *testing.T) {
	ctx := context.Background()
	r := &aggregateResource{}
	_, plan := aggregateSchemaState(ctx, r)
	for key, wantErr := range map[string]bool{"availability_zone": true, "pf9-managed": true, "pf9_failure_domain": false, "pf9_fd_label": false} {
		cfg := aggregateModel{ID: types.StringNull(), Name: types.StringValue("a"), Zone: types.StringNull(),
			Metadata: stringMap(t, map[string]string{key: "v"}), Hosts: types.SetNull(types.StringType), Region: types.StringNull()}
		if d := plan.Set(ctx, &cfg); d.HasError() {
			t.Fatal(d)
		}
		resp := resource.ValidateConfigResponse{}
		r.ValidateConfig(ctx, resource.ValidateConfigRequest{Config: tfsdk.Config(plan)}, &resp)
		if resp.Diagnostics.HasError() != wantErr {
			t.Errorf("metadata key %q: error=%v, want %v (%v)", key, resp.Diagnostics.HasError(), wantErr, resp.Diagnostics)
		}
	}
}

func TestAggregateByName(t *testing.T) {
	all := []aggregates.Aggregate{{ID: 1, Name: "cluster-1"}, {ID: 7, Name: "fd-a"}}
	got, err := aggregateByName(all, "fd-a")
	if err != nil || got.ID != 7 {
		t.Fatalf("got %v, %v", got, err)
	}
	if _, err := aggregateByName(all, "missing"); err == nil || !strings.Contains(err.Error(), "cluster-1") {
		t.Fatalf("err = %v; want the known names listed", err)
	}
}
