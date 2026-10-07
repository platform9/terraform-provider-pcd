// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package blockstorage

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"

	"github.com/platform9/terraform-provider-pcd/internal/clients"
)

// newFakeCinder starts a test server that answers from routes, each keyed
// "METHOD /path", and returns a clients.Config whose Cinder client reaches it.
// A request no route names fails the test. The server closes when the test
// ends.
func newFakeCinder(t *testing.T, routes map[string]http.HandlerFunc) *clients.Config {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if handle, ok := routes[r.Method+" "+r.URL.Path]; ok {
			handle(w, r)
			return
		}
		t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusNotImplemented)
	}))
	t.Cleanup(srv.Close)
	return fakeConfig(srv.URL)
}

// reply answers with status and a JSON body.
func reply(status int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		fmt.Fprint(w, body)
	}
}

// noPanic runs f and fails the test if f panics. A panic in a resource method
// crashes the provider plugin, which fails the whole apply.
func noPanic(t *testing.T, f func()) {
	t.Helper()
	defer func() {
		if p := recover(); p != nil {
			t.Fatalf("panicked: %v", p)
		}
	}()
	f()
}

// newSnapshotState returns a state of r's schema that holds m.
func newSnapshotState(t *testing.T, r *snapshotResource, m *snapshotModel) tfsdk.State {
	t.Helper()
	ctx := context.Background()
	var sch resource.SchemaResponse
	r.Schema(ctx, resource.SchemaRequest{}, &sch)
	s := sch.Schema
	state := tfsdk.State{Schema: s, Raw: tftypes.NewValue(s.Type().TerraformType(ctx), nil)}
	if d := state.Set(ctx, m); d.HasError() {
		t.Fatalf("building the state: %v", d)
	}
	return state
}

// snapshotRow is an available snapshot as Terraform recorded it.
func snapshotRow() *snapshotModel {
	return &snapshotModel{
		ID:          types.StringValue("snap-1"),
		VolumeID:    types.StringValue("vol-1"),
		Name:        types.StringValue("nightly"),
		Description: types.StringValue(""),
		Force:       types.BoolValue(false),
		Metadata:    types.MapValueMust(types.StringType, map[string]attr.Value{}),
		Size:        types.Int64Value(1),
		Status:      types.StringValue("available"),
		Region:      types.StringValue("region-one"),
	}
}

// runDataSourceRead calls d's Read the way the framework does, with a config
// that sets the string attributes in set and leaves every other one null, and
// a response state that starts as a null object.
func runDataSourceRead(t *testing.T, d datasource.DataSource, set map[string]string) datasource.ReadResponse {
	t.Helper()
	ctx := context.Background()
	var schemaResp datasource.SchemaResponse
	d.Schema(ctx, datasource.SchemaRequest{}, &schemaResp)
	s := schemaResp.Schema
	typ := s.Type().TerraformType(ctx).(tftypes.Object)
	vals := make(map[string]tftypes.Value, len(typ.AttributeTypes))
	for name, at := range typ.AttributeTypes {
		vals[name] = tftypes.NewValue(at, nil)
	}
	for name, v := range set {
		vals[name] = tftypes.NewValue(tftypes.String, v)
	}
	config := tfsdk.Config{Schema: s, Raw: tftypes.NewValue(typ, vals)}
	resp := datasource.ReadResponse{State: tfsdk.State{Schema: s, Raw: tftypes.NewValue(typ, nil)}}
	d.Read(ctx, datasource.ReadRequest{Config: config}, &resp)
	return resp
}

// reportsNoObject reports whether diags holds an error that names the answer
// without the object.
func reportsNoObject(diags diag.Diagnostics) bool {
	for _, d := range diags.Errors() {
		if strings.Contains(d.Detail(), clients.ErrNoObject.Error()) {
			return true
		}
	}
	return false
}

// gophercloud decodes a snapshot answer whose body lacks the snapshot key to a
// nil snapshot and no error. Read used to dereference it and crash the
// provider. It must report an error and keep the row: the snapshot may well
// exist, so the answer is no reason to drop it from state.
func TestSnapshotReadRefusesAnAnswerWithoutTheSnapshot(t *testing.T) {
	t.Parallel()
	r := &snapshotResource{config: newFakeCinder(t, map[string]http.HandlerFunc{
		"GET /snapshots/snap-1": reply(http.StatusOK, `{}`),
	})}
	prior := newSnapshotState(t, r, snapshotRow())
	resp := resource.ReadResponse{State: prior}
	noPanic(t, func() { r.Read(context.Background(), resource.ReadRequest{State: prior}, &resp) })
	if !reportsNoObject(resp.Diagnostics) {
		t.Fatalf("read diagnostics = %v; want an error for the answer without the snapshot", resp.Diagnostics)
	}
	if !resp.State.Raw.Equal(prior.Raw) {
		t.Fatalf("read changed the row to %v; want it kept as %v", resp.State.Raw, prior.Raw)
	}
}

// Update reads the snapshot back after renaming it, and crashed the same way.
// It must report an error and keep the prior row.
func TestSnapshotUpdateRefusesAReadBackWithoutTheSnapshot(t *testing.T) {
	t.Parallel()
	r := &snapshotResource{config: newFakeCinder(t, map[string]http.HandlerFunc{
		"PUT /snapshots/snap-1": reply(http.StatusOK, `{"snapshot": {"id": "snap-1", "name": "weekly"}}`),
		"GET /snapshots/snap-1": reply(http.StatusOK, `{}`),
	})}
	prior := newSnapshotState(t, r, snapshotRow())
	renamed := snapshotRow()
	renamed.Name = types.StringValue("weekly")
	plan := tfsdk.Plan(newSnapshotState(t, r, renamed))
	resp := resource.UpdateResponse{State: prior}
	noPanic(t, func() { r.Update(context.Background(), resource.UpdateRequest{Plan: plan, State: prior}, &resp) })
	if !reportsNoObject(resp.Diagnostics) {
		t.Fatalf("update diagnostics = %v; want an error for the answer without the snapshot", resp.Diagnostics)
	}
	if !resp.State.Raw.Equal(prior.Raw) {
		t.Fatalf("update changed the row to %v; want the prior row %v", resp.State.Raw, prior.Raw)
	}
}

// The waits poll the snapshot, and used to dereference the nil snapshot as
// well. Each must fail on it instead.
func TestSnapshotWaitsRefuseAnAnswerWithoutTheSnapshot(t *testing.T) {
	t.Parallel()
	waits := []struct {
		name string
		wait func(context.Context, *gophercloud.ServiceClient) error
	}{
		{"available", func(ctx context.Context, c *gophercloud.ServiceClient) error {
			_, err := waitForSnapshotStatus(ctx, c, "snap-1", "available", 2*time.Second)
			return err
		}},
		{"deleted", func(ctx context.Context, c *gophercloud.ServiceClient) error {
			return waitForSnapshotDeleted(ctx, c, "snap-1", 2*time.Second)
		}},
	}
	for _, w := range waits {
		t.Run(w.name, func(t *testing.T) {
			t.Parallel()
			client, err := newFakeCinder(t, map[string]http.HandlerFunc{
				"GET /snapshots/snap-1": reply(http.StatusOK, `{}`),
			}).BlockStorageV3Client()
			if err != nil {
				t.Fatalf("building the client: %v", err)
			}
			noPanic(t, func() { err = w.wait(context.Background(), client) })
			if !errors.Is(err, clients.ErrNoObject) {
				t.Fatalf("wait error = %v; want it to report the answer without the snapshot", err)
			}
		})
	}
}

// The snapshot data source's lookup by ID used to dereference the nil snapshot
// as well. It must report an error and set nothing.
func TestSnapshotDataSourceRefusesAnAnswerWithoutTheSnapshot(t *testing.T) {
	t.Parallel()
	d := &snapshotDataSource{config: newFakeCinder(t, map[string]http.HandlerFunc{
		"GET /snapshots/snap-1": reply(http.StatusOK, `{}`),
	})}
	var resp datasource.ReadResponse
	noPanic(t, func() { resp = runDataSourceRead(t, d, map[string]string{"snapshot_id": "snap-1"}) })
	if !reportsNoObject(resp.Diagnostics) {
		t.Fatalf("read diagnostics = %v; want an error for the answer without the snapshot", resp.Diagnostics)
	}
	if !resp.State.Raw.IsNull() {
		t.Fatalf("read set %v; want nothing set", resp.State.Raw)
	}
}
