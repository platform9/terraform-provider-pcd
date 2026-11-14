// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package resmgr

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

// hamgr's VM high-availability configuration route for cluster-1, as the
// resource reaches it through the shared resmgr fake: fakeConfig answers the
// catalog with the test server and HamgrV1Client adds v1/.
const vmhaPath = "/v1/vmha/config/cluster-1"

// vmhaPosts records the body of every POST it answers, and answers as hamgr
// does when it accepts a write: 200 with {"status":"ok"}.
type vmhaPosts struct{ bodies []string }

func (p *vmhaPosts) handle(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	p.bodies = append(p.bodies, string(raw))
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, `{"status":"ok"}`)
}

// vmhaText answers with status and a plain-text body, as hamgr's handler and
// router both do for a 404.
func vmhaText(status int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(status)
		fmt.Fprint(w, body)
	}
}

func vmhaModel(id, speed, path types.String) *clusterVMHAConfigModel {
	return &clusterVMHAConfigModel{ID: id, ClusterName: types.StringValue("cluster-1"), EvacuationSpeed: speed, NFSHeartbeatPath: path}
}

func vmhaRow(t *testing.T, s tfsdk.State) clusterVMHAConfigModel {
	t.Helper()
	var m clusterVMHAConfigModel
	if d := s.Get(context.Background(), &m); d.HasError() {
		t.Fatalf("reading state: %v", d)
	}
	return m
}

// Create sends only what the configuration sets, so hamgr keeps the rest,
// then records what hamgr holds, the path it already had included.
func TestVMHAConfigCreatePostsOnlyWhatIsSet(t *testing.T) {
	posts := &vmhaPosts{}
	f := newRoutedResmgr(t, resmgrRoutes{
		"POST " + vmhaPath: posts.handle,
		"GET " + vmhaPath:  reply(http.StatusOK, `{"mode": "balanced", "host_config": {"nfs_heartbeat_path": "/mnt/hb", "host_query_timeout": 30}}`),
	})
	r := &clusterVMHAConfigResource{config: f.config}
	s := schemaOf(t, r)
	resp := runCreate(r, newPlan(t, s, vmhaModel(types.StringUnknown(), types.StringValue("balanced"), types.StringUnknown())))
	if resp.Diagnostics.HasError() {
		t.Fatalf("create: %v", resp.Diagnostics)
	}
	if got := f.received(); !slices.Equal(got, []string{"POST " + vmhaPath, "GET " + vmhaPath}) {
		t.Fatalf("requests = %v", got)
	}
	if len(posts.bodies) != 1 || posts.bodies[0] != `{"mode":"balanced"}` {
		t.Fatalf("POST bodies = %q, want only the mode", posts.bodies)
	}
	got := vmhaRow(t, resp.State)
	if got.ID.ValueString() != "cluster-1" || got.EvacuationSpeed.ValueString() != "balanced" || got.NFSHeartbeatPath.ValueString() != "/mnt/hb" {
		t.Fatalf("state = %+v", got)
	}
}

// An empty path turns the storage heartbeat off: it is sent as "", the way
// the PCD UI's Edit dialog clears it, and read back as "" when hamgr then
// returns no path.
func TestVMHAConfigUpdateClearsTheHeartbeatPath(t *testing.T) {
	posts := &vmhaPosts{}
	f := newRoutedResmgr(t, resmgrRoutes{
		"POST " + vmhaPath: posts.handle,
		"GET " + vmhaPath:  reply(http.StatusOK, `{"mode": "aggressive", "host_config": {}}`),
	})
	r := &clusterVMHAConfigResource{config: f.config}
	s := schemaOf(t, r)
	prior := newState(t, s, vmhaModel(types.StringValue("cluster-1"), types.StringValue("aggressive"), types.StringValue("/mnt/hb")))
	plan := newPlan(t, s, vmhaModel(types.StringValue("cluster-1"), types.StringUnknown(), types.StringValue("")))
	resp := runUpdate(r, plan, prior)
	if resp.Diagnostics.HasError() {
		t.Fatalf("update: %v", resp.Diagnostics)
	}
	f.received()
	if len(posts.bodies) != 1 || posts.bodies[0] != `{"host_config":{"nfs_heartbeat_path":""}}` {
		t.Fatalf("POST bodies = %q, want only the cleared path", posts.bodies)
	}
	got := vmhaRow(t, resp.State)
	if got.EvacuationSpeed.ValueString() != "aggressive" || got.NFSHeartbeatPath.IsNull() || got.NFSHeartbeatPath.ValueString() != "" {
		t.Fatalf("state = %+v, want aggressive and an empty path", got)
	}
}

// A cluster with no mode stored reads null for evacuation_speed: hamgr's
// default applies, and the resource does not guess it.
func TestVMHAConfigReadsAMissingModeAsNull(t *testing.T) {
	f := newRoutedResmgr(t, resmgrRoutes{"GET " + vmhaPath: reply(http.StatusOK, `{"host_config": {"nfs_heartbeat_path": "/mnt/hb"}}`)})
	r := &clusterVMHAConfigResource{config: f.config}
	s := schemaOf(t, r)
	resp := runRead(r, newState(t, s, vmhaModel(types.StringValue("cluster-1"), types.StringValue("balanced"), types.StringValue("/mnt/hb"))))
	if resp.Diagnostics.HasError() {
		t.Fatalf("read: %v", resp.Diagnostics)
	}
	if got := vmhaRow(t, resp.State); !got.EvacuationSpeed.IsNull() || got.NFSHeartbeatPath.ValueString() != "/mnt/hb" {
		t.Fatalf("state = %+v, want a null speed and the path", got)
	}
}

// hamgr's handler answers 404 "no config found for cluster" when it holds
// nothing for the cluster: the configuration is gone. The router's own 404
// means this hamgr has no configuration API, and a 200 without a
// configuration describes nothing: both are errors, not a gone object.
func TestVMHAConfigReadNotFound(t *testing.T) {
	for _, tc := range []struct {
		name       string
		handler    http.HandlerFunc
		wantGone   bool
		wantDetail string
	}{
		{name: "no configuration", handler: vmhaText(http.StatusNotFound, "no config found for cluster"), wantGone: true},
		{name: "no configuration API", handler: vmhaText(http.StatusNotFound, "404 page not found\n"), wantDetail: "hamgr does not serve this URL"},
		{name: "200 without a configuration", handler: reply(http.StatusOK, "null"), wantDetail: `hamgr answered without a configuration for cluster "cluster-1"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRoutedResmgr(t, resmgrRoutes{"GET " + vmhaPath: tc.handler})
			r := &clusterVMHAConfigResource{config: f.config}
			s := schemaOf(t, r)
			resp := runRead(r, newState(t, s, vmhaModel(types.StringValue("cluster-1"), types.StringValue("balanced"), types.StringValue(""))))
			if tc.wantGone {
				if resp.Diagnostics.HasError() || !resp.State.Raw.IsNull() {
					t.Fatalf("diags = %v, state null = %v; want the row removed without an error", resp.Diagnostics, resp.State.Raw.IsNull())
				}
				return
			}
			if !resp.Diagnostics.HasError() || !strings.Contains(resp.Diagnostics[0].Detail(), tc.wantDetail) {
				t.Fatalf("diags = %v, want an error that says %q", resp.Diagnostics, tc.wantDetail)
			}
			if resp.State.Raw.IsNull() {
				t.Fatal("the row was removed; an answer without a configuration is not a deleted configuration")
			}
		})
	}
}

// hamgr's refusal reaches the user with its own message.
func TestVMHAConfigCreateSurfacesARefusal(t *testing.T) {
	f := newRoutedResmgr(t, resmgrRoutes{
		"POST " + vmhaPath: vmhaText(http.StatusBadRequest, `invalid mode "fast"; must be one of conservative, balanced, aggressive`),
	})
	r := &clusterVMHAConfigResource{config: f.config}
	s := schemaOf(t, r)
	resp := runCreate(r, newPlan(t, s, vmhaModel(types.StringUnknown(), types.StringValue("balanced"), types.StringUnknown())))
	if !resp.Diagnostics.HasError() || !strings.Contains(resp.Diagnostics[0].Detail(), "must be one of conservative") {
		t.Fatalf("diags = %v", resp.Diagnostics)
	}
	if !resp.State.Raw.IsNull() {
		t.Fatal("a refused write was recorded in state")
	}
}

// hamgr has no DELETE: destroy sends nothing and says the settings stay.
func TestVMHAConfigDeleteCallsNothing(t *testing.T) {
	f := newRoutedResmgr(t, resmgrRoutes{})
	r := &clusterVMHAConfigResource{config: f.config}
	s := schemaOf(t, r)
	prior := newState(t, s, vmhaModel(types.StringValue("cluster-1"), types.StringValue("aggressive"), types.StringValue("")))
	resp := resource.DeleteResponse{State: prior}
	r.Delete(context.Background(), resource.DeleteRequest{State: prior}, &resp)
	if resp.Diagnostics.HasError() || resp.Diagnostics.WarningsCount() != 1 ||
		resp.Diagnostics[0].Summary() != "VM high-availability settings left in place" {
		t.Fatalf("diags = %v, want one warning", resp.Diagnostics)
	}
	if got := f.received(); len(got) != 0 {
		t.Fatalf("requests = %v, want none", got)
	}
}

// The import ID is the cluster name.
func TestVMHAConfigImportState(t *testing.T) {
	r := &clusterVMHAConfigResource{}
	s := schemaOf(t, r)
	resp := resource.ImportStateResponse{State: tfsdk.State{Schema: s, Raw: tftypes.NewValue(s.Type().TerraformType(context.Background()), nil)}}
	r.ImportState(context.Background(), resource.ImportStateRequest{ID: "cluster-1"}, &resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("import: %v", resp.Diagnostics)
	}
	if got := vmhaRow(t, resp.State); got.ID.ValueString() != "cluster-1" || got.ClusterName.ValueString() != "cluster-1" {
		t.Fatalf("state = %+v", got)
	}
}

// A configuration that sets neither setting has nothing to manage.
func TestVMHAConfigNeedsASetting(t *testing.T) {
	r := &clusterVMHAConfigResource{}
	s := schemaOf(t, r)
	ctx := context.Background()
	for _, tc := range []struct {
		name    string
		model   *clusterVMHAConfigModel
		wantErr bool
	}{
		{"neither", vmhaModel(types.StringNull(), types.StringNull(), types.StringNull()), true},
		{"speed", vmhaModel(types.StringNull(), types.StringValue("balanced"), types.StringNull()), false},
		{"empty path", vmhaModel(types.StringNull(), types.StringNull(), types.StringValue("")), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plan := newPlan(t, s, tc.model)
			cfg := tfsdk.Config{Schema: s, Raw: plan.Raw}
			var resp resource.ValidateConfigResponse
			for _, v := range r.ConfigValidators(ctx) {
				v.ValidateResource(ctx, resource.ValidateConfigRequest{Config: cfg}, &resp)
			}
			if resp.Diagnostics.HasError() != tc.wantErr {
				t.Fatalf("diags = %v, want error %v", resp.Diagnostics, tc.wantErr)
			}
		})
	}
}

// Read stores what hamgr holds, not what the prior state says.
func TestVMHAConfigReadStoresChangedValues(t *testing.T) {
	f := newRoutedResmgr(t, resmgrRoutes{"GET " + vmhaPath: reply(http.StatusOK, `{"mode": "aggressive", "host_config": {"nfs_heartbeat_path": "/mnt/other"}}`)})
	r := &clusterVMHAConfigResource{config: f.config}
	s := schemaOf(t, r)
	resp := runRead(r, newState(t, s, vmhaModel(types.StringValue("cluster-1"), types.StringValue("balanced"), types.StringValue("/mnt/hb"))))
	if resp.Diagnostics.HasError() {
		t.Fatalf("read: %v", resp.Diagnostics)
	}
	if got := vmhaRow(t, resp.State); got.ID.ValueString() != "cluster-1" || got.EvacuationSpeed.ValueString() != "aggressive" ||
		got.NFSHeartbeatPath.ValueString() != "/mnt/other" {
		t.Fatalf("state = %+v, want aggressive and /mnt/other", got)
	}
	if got := f.received(); !slices.Equal(got, []string{"GET " + vmhaPath}) {
		t.Fatalf("requests = %v", got)
	}
}

// A state that import wrote holds only id and cluster_name: Read fills both
// settings from hamgr. A stored {} (hamgr keeps only keys this resource does
// not manage) is a configuration: a null speed and an empty path.
func TestVMHAConfigReadAfterImport(t *testing.T) {
	for _, tc := range []struct {
		name, body          string
		wantSpeed, wantPath types.String
	}{
		{"both settings", `{"mode": "balanced", "host_config": {"nfs_heartbeat_path": "/mnt/hb"}}`, types.StringValue("balanced"), types.StringValue("/mnt/hb")},
		{"neither setting", `{}`, types.StringNull(), types.StringValue("")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRoutedResmgr(t, resmgrRoutes{"GET " + vmhaPath: reply(http.StatusOK, tc.body)})
			r := &clusterVMHAConfigResource{config: f.config}
			s := schemaOf(t, r)
			resp := runRead(r, newState(t, s, vmhaModel(types.StringValue("cluster-1"), types.StringNull(), types.StringNull())))
			if resp.Diagnostics.HasError() {
				t.Fatalf("read: %v", resp.Diagnostics)
			}
			got := vmhaRow(t, resp.State)
			if got.ID.ValueString() != "cluster-1" || got.ClusterName.ValueString() != "cluster-1" ||
				!got.EvacuationSpeed.Equal(tc.wantSpeed) || !got.NFSHeartbeatPath.Equal(tc.wantPath) {
				t.Fatalf("state = %+v, want speed %s and path %s", got, tc.wantSpeed, tc.wantPath)
			}
		})
	}
}

// vmhaBadClusterNames are names that would address another hamgr route once
// put into /v1/vmha/config/<cluster>, or none at all.
var vmhaBadClusterNames = []string{"", "a/b", "a?b", "a b", ".", ".."}

// Import refuses a cluster name that cannot address its configuration, leaves
// the state null and sends nothing.
func TestVMHAConfigImportRefusesABadName(t *testing.T) {
	for _, id := range vmhaBadClusterNames {
		t.Run(fmt.Sprintf("%q", id), func(t *testing.T) {
			f := newRoutedResmgr(t, resmgrRoutes{})
			r := &clusterVMHAConfigResource{config: f.config}
			s := schemaOf(t, r)
			resp := resource.ImportStateResponse{State: tfsdk.State{Schema: s, Raw: tftypes.NewValue(s.Type().TerraformType(context.Background()), nil)}}
			r.ImportState(context.Background(), resource.ImportStateRequest{ID: id}, &resp)
			if !resp.Diagnostics.HasError() || resp.Diagnostics[0].Summary() != "Import needs a cluster name" {
				t.Fatalf("diags = %v, want the import refused", resp.Diagnostics)
			}
			if !resp.State.Raw.IsNull() {
				t.Fatalf("state = %v, want it left null", resp.State.Raw)
			}
			if got := f.received(); len(got) != 0 {
				t.Fatalf("requests = %v, want none", got)
			}
		})
	}
}

// A state whose cluster_name cannot address its configuration fails Read
// before any request and keeps the row.
func TestVMHAConfigReadRefusesABadName(t *testing.T) {
	for _, name := range vmhaBadClusterNames {
		t.Run(fmt.Sprintf("%q", name), func(t *testing.T) {
			f := newRoutedResmgr(t, resmgrRoutes{})
			r := &clusterVMHAConfigResource{config: f.config}
			s := schemaOf(t, r)
			m := vmhaModel(types.StringValue(name), types.StringValue("balanced"), types.StringValue(""))
			m.ClusterName = types.StringValue(name)
			resp := runRead(r, newState(t, s, m))
			if !resp.Diagnostics.HasError() || !strings.Contains(resp.Diagnostics[0].Detail(), "cannot address a VM high-availability configuration") {
				t.Fatalf("diags = %v, want the name refused", resp.Diagnostics)
			}
			if resp.State.Raw.IsNull() {
				t.Fatal("the row was removed")
			}
			if got := f.received(); len(got) != 0 {
				t.Fatalf("requests = %v, want none", got)
			}
		})
	}
}

// Create records the row once hamgr has accepted the write, before its
// read-back: a failed read-back leaves the row in state with its ID.
func TestVMHAConfigCreateRecordsTheRowBeforeTheReadBack(t *testing.T) {
	posts := &vmhaPosts{}
	f := newRoutedResmgr(t, resmgrRoutes{
		"POST " + vmhaPath: posts.handle,
		"GET " + vmhaPath:  vmhaText(http.StatusNotFound, "no config found for cluster"),
	})
	r := &clusterVMHAConfigResource{config: f.config}
	s := schemaOf(t, r)
	resp := runCreate(r, newPlan(t, s, vmhaModel(types.StringUnknown(), types.StringValue("balanced"), types.StringUnknown())))
	if !resp.Diagnostics.HasError() || !strings.Contains(resp.Diagnostics[0].Detail(), "right after writing it") {
		t.Fatalf("diags = %v, want the read-back error", resp.Diagnostics)
	}
	for _, want := range []string{"hamgr accepted the settings", "tainted", "the next apply writes the settings again", "terraform untaint"} {
		if !strings.Contains(resp.Diagnostics[0].Detail(), want) {
			t.Fatalf("detail = %q, want it to say %q", resp.Diagnostics[0].Detail(), want)
		}
	}
	if resp.State.Raw.IsNull() {
		t.Fatal("the written configuration was left out of state")
	}
	if got := vmhaRow(t, resp.State); got.ID.ValueString() != "cluster-1" {
		t.Fatalf("state id = %s, want cluster-1", got.ID)
	}
}

// Create refuses a cluster name that cannot address its configuration before
// any request.
func TestVMHAConfigCreateRefusesABadName(t *testing.T) {
	f := newRoutedResmgr(t, resmgrRoutes{})
	r := &clusterVMHAConfigResource{config: f.config}
	s := schemaOf(t, r)
	m := vmhaModel(types.StringUnknown(), types.StringValue("balanced"), types.StringUnknown())
	m.ClusterName = types.StringValue("a/b")
	resp := runCreate(r, newPlan(t, s, m))
	if !resp.Diagnostics.HasError() || !strings.Contains(resp.Diagnostics[0].Detail(), "cannot address a VM high-availability configuration") {
		t.Fatalf("diags = %v, want the name refused", resp.Diagnostics)
	}
	if got := f.received(); len(got) != 0 {
		t.Fatalf("requests = %v, want none", got)
	}
	if !resp.State.Raw.IsNull() {
		t.Fatal("a refused name was recorded in state")
	}
}

// A configuration that sets both settings sends both in one POST.
func TestVMHAConfigCreatePostsBothSettings(t *testing.T) {
	posts := &vmhaPosts{}
	f := newRoutedResmgr(t, resmgrRoutes{
		"POST " + vmhaPath: posts.handle,
		"GET " + vmhaPath:  reply(http.StatusOK, `{"mode": "aggressive", "host_config": {"nfs_heartbeat_path": "/mnt/hb"}}`),
	})
	r := &clusterVMHAConfigResource{config: f.config}
	s := schemaOf(t, r)
	resp := runCreate(r, newPlan(t, s, vmhaModel(types.StringUnknown(), types.StringValue("aggressive"), types.StringValue("/mnt/hb"))))
	if resp.Diagnostics.HasError() {
		t.Fatalf("create: %v", resp.Diagnostics)
	}
	f.received()
	if len(posts.bodies) != 1 || posts.bodies[0] != `{"mode":"aggressive","host_config":{"nfs_heartbeat_path":"/mnt/hb"}}` {
		t.Fatalf("POST bodies = %q, want both settings", posts.bodies)
	}
}

// vmhaClusterRefusal is hamgr's answer to a write for a cluster missing from
// its cached copy of resmgr's cluster list, as http.Error sends it.
var vmhaClusterRefusal = vmhaText(http.StatusNotFound, `cluster "cluster-1" not found in resmgr`+"\n")

// vmhaSequence answers the first request with the first handler, the second
// with the second, and every later one with the last.
func vmhaSequence(handlers ...http.HandlerFunc) http.HandlerFunc {
	calls := 0
	return func(w http.ResponseWriter, r *http.Request) {
		h := handlers[min(calls, len(handlers)-1)]
		calls++
		h(w, r)
	}
}

// shortVMHAClusterWait shortens the wait for hamgr to learn of a cluster for
// one test and restores it, and the per-attempt bound
// vmhaClusterKnownCheckTimeout that a test may then shorten as well, when the
// test ends.
func shortVMHAClusterWait(t *testing.T, interval, timeout time.Duration) {
	t.Helper()
	oldInterval, oldTimeout, oldCheck := vmhaClusterKnownInterval, vmhaClusterKnownTimeout, vmhaClusterKnownCheckTimeout
	vmhaClusterKnownInterval, vmhaClusterKnownTimeout = interval, timeout
	t.Cleanup(func() {
		vmhaClusterKnownInterval, vmhaClusterKnownTimeout, vmhaClusterKnownCheckTimeout = oldInterval, oldTimeout, oldCheck
	})
}

func vmhaCount(requests []string, call string) int {
	n := 0
	for _, r := range requests {
		if r == call {
			n++
		}
	}
	return n
}

// hamgr refuses a write for a cluster created moments ago until it rereads
// resmgr's cluster list. When resmgr has the cluster, Create retries until
// hamgr accepts the write.
func TestVMHAConfigCreateWaitsForHamgrToLearnOfTheCluster(t *testing.T) {
	shortVMHAClusterWait(t, time.Millisecond, time.Minute)
	posts := &vmhaPosts{}
	f := newRoutedResmgr(t, resmgrRoutes{
		"POST " + vmhaPath:   vmhaSequence(vmhaClusterRefusal, vmhaClusterRefusal, posts.handle),
		"GET " + clusterPath: reply(http.StatusOK, clusterJSON),
		"GET " + vmhaPath:    reply(http.StatusOK, `{"mode": "balanced", "host_config": {}}`),
	})
	r := &clusterVMHAConfigResource{config: f.config}
	s := schemaOf(t, r)
	resp := runCreate(r, newPlan(t, s, vmhaModel(types.StringUnknown(), types.StringValue("balanced"), types.StringUnknown())))
	if resp.Diagnostics.HasError() {
		t.Fatalf("create: %v", resp.Diagnostics)
	}
	got := f.received()
	if vmhaCount(got, "POST "+vmhaPath) != 3 || vmhaCount(got, "GET "+clusterPath) != 1 {
		t.Fatalf("requests = %v, want three POSTs and one resmgr GET", got)
	}
	if row := vmhaRow(t, resp.State); row.ID.ValueString() != "cluster-1" || row.EvacuationSpeed.ValueString() != "balanced" {
		t.Fatalf("state = %+v", row)
	}
}

// When hamgr refuses the cluster and resmgr cannot confirm it either, Create
// fails at once after one POST and records nothing. resmgr answers a missing
// cluster with a 404 or with 200 and null, depending on its build, and looks a
// name up regardless of case, while hamgr compares names exactly: an answer
// for a cluster whose name differs is not the cluster asked for.
func TestVMHAConfigCreateFailsWhenResmgrCannotConfirmTheCluster(t *testing.T) {
	for _, tc := range []struct {
		name, cluster string
		resmgr        http.HandlerFunc
		wantDetail    []string
	}{
		{"resmgr answers 404", "cluster-1", reply(http.StatusNotFound, `{"message": "not found"}`),
			[]string{`PCD's resource manager has no cluster named "cluster-1"`, "not found in resmgr"}},
		{"resmgr answers 200 null", "cluster-1", reply(http.StatusOK, "null"),
			[]string{`PCD's resource manager has no cluster named "cluster-1"`, "not found in resmgr"}},
		{"resmgr answers for another case", "Cluster-1", reply(http.StatusOK, clusterJSON),
			[]string{`PCD's resource manager has a cluster named "cluster-1", not "Cluster-1"`, "exactly", "not found in resmgr"}},
		{"resmgr answers without a name", "cluster-1", reply(http.StatusOK, `{"id":"x"}`),
			[]string{"its answer names no cluster", "not found in resmgr"}},
		{"resmgr fails", "cluster-1", reply(http.StatusInternalServerError, `{"message": "boom"}`),
			[]string{"not found in resmgr", "500"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// A wait that started would retry the POST within a millisecond.
			shortVMHAClusterWait(t, time.Millisecond, time.Second)
			hamgrPath, resmgrPath := "/v1/vmha/config/"+tc.cluster, clustersPath+"/"+tc.cluster
			f := newRoutedResmgr(t, resmgrRoutes{
				"POST " + hamgrPath: vmhaText(http.StatusNotFound, fmt.Sprintf("cluster %q not found in resmgr\n", tc.cluster)),
				"GET " + resmgrPath: tc.resmgr,
			})
			r := &clusterVMHAConfigResource{config: f.config}
			s := schemaOf(t, r)
			m := vmhaModel(types.StringUnknown(), types.StringValue("balanced"), types.StringUnknown())
			m.ClusterName = types.StringValue(tc.cluster)
			resp := runCreate(r, newPlan(t, s, m))
			if !resp.Diagnostics.HasError() {
				t.Fatal("create succeeded")
			}
			for _, want := range tc.wantDetail {
				if !strings.Contains(resp.Diagnostics[0].Detail(), want) {
					t.Fatalf("detail = %q, want it to contain %q", resp.Diagnostics[0].Detail(), want)
				}
			}
			if got := f.received(); !slices.Equal(got, []string{"POST " + hamgrPath, "GET " + resmgrPath}) {
				t.Fatalf("requests = %v, want one POST and one resmgr GET", got)
			}
			if !resp.State.Raw.IsNull() {
				t.Fatal("a refused write was recorded in state")
			}
		})
	}
}

// vmhaNumberedRefusals answers every call with hamgr's refusal, numbered so a
// test can tell which answer an error carries: "(answer 1)", "(answer 2)"...
func vmhaNumberedRefusals() http.HandlerFunc {
	calls := 0
	return func(w http.ResponseWriter, r *http.Request) {
		calls++
		vmhaText(http.StatusNotFound, fmt.Sprintf(`cluster "cluster-1" not found in resmgr (answer %d)`, calls))(w, r)
	}
}

// vmhaHang answers nothing until the request is abandoned: a write hamgr has
// not answered when the wait or the attempt ends. It reads the body first:
// the server notices a closed connection only once the body is consumed.
func vmhaHang(_ http.ResponseWriter, r *http.Request) {
	_, _ = io.Copy(io.Discard, r.Body)
	<-r.Context().Done()
}

// A wait that ends between attempts fails with what it waited for, for how
// long, and the last answer, and records nothing: every write so far was
// refused. The interval is longer than the wait, so the deadline arrives
// while the wait is between attempts.
func TestVMHAConfigCreateWaitTimesOut(t *testing.T) {
	shortVMHAClusterWait(t, time.Hour, 50*time.Millisecond)
	f := newRoutedResmgr(t, resmgrRoutes{
		"POST " + vmhaPath:   vmhaClusterRefusal,
		"GET " + clusterPath: reply(http.StatusOK, clusterJSON),
	})
	r := &clusterVMHAConfigResource{config: f.config}
	s := schemaOf(t, r)
	resp := runCreate(r, newPlan(t, s, vmhaModel(types.StringUnknown(), types.StringValue("balanced"), types.StringUnknown())))
	if !resp.Diagnostics.HasError() {
		t.Fatal("create succeeded")
	}
	detail := resp.Diagnostics[0].Detail()
	for _, want := range []string{`cluster "cluster-1"`, "50ms", "not found in resmgr", "although the resource manager has it",
		"cannot reach the resource manager", "run the apply again", "check hamgr"} {
		if !strings.Contains(detail, want) {
			t.Fatalf("detail = %q, want it to contain %q", detail, want)
		}
	}
	if strings.Contains(detail, "unknown") {
		t.Fatalf("detail = %q; every write was refused, so no outcome is unknown", detail)
	}
	if !resp.State.Raw.IsNull() {
		t.Fatal("a refused write was recorded in state")
	}
}

// A deadline that arrives while a retried write is unanswered cannot say
// nothing was written: the outcome of that write is unknown. The error
// carries hamgr's last answer, not its first.
func TestVMHAConfigCreateWaitTimesOutDuringAWrite(t *testing.T) {
	shortVMHAClusterWait(t, time.Millisecond, time.Second)
	refusals := vmhaNumberedRefusals()
	f := newRoutedResmgr(t, resmgrRoutes{
		"POST " + vmhaPath:   vmhaSequence(refusals, refusals, vmhaHang),
		"GET " + clusterPath: reply(http.StatusOK, clusterJSON),
	})
	r := &clusterVMHAConfigResource{config: f.config}
	s := schemaOf(t, r)
	resp := runCreate(r, newPlan(t, s, vmhaModel(types.StringUnknown(), types.StringValue("balanced"), types.StringUnknown())))
	if !resp.Diagnostics.HasError() {
		t.Fatal("create succeeded")
	}
	detail := resp.Diagnostics[0].Detail()
	for _, want := range []string{`cluster "cluster-1"`, "1s", "although the resource manager has it",
		"the outcome of the last write is unknown", "the next apply sends the settings again", "(answer 2)"} {
		if !strings.Contains(detail, want) {
			t.Fatalf("detail = %q, want it to contain %q", detail, want)
		}
	}
	// After a failed Create nothing is in state, so the next apply sends the
	// settings whether or not hamgr holds them.
	if strings.Contains(detail, "if hamgr does not hold them") {
		t.Fatalf("detail = %q; a failed Create is re-sent unconditionally", detail)
	}
	if strings.Contains(detail, "(answer 1)") || strings.Contains(detail, "nothing was written") {
		t.Fatalf("detail = %q, want only the last answer and no claim that nothing was written", detail)
	}
	if got := f.received(); vmhaCount(got, "POST "+vmhaPath) != 3 {
		t.Fatalf("requests = %v, want three POSTs", got)
	}
	if !resp.State.Raw.IsNull() {
		t.Fatal("a write of unknown outcome was recorded in state")
	}
}

// Any answer other than the refusal ends the wait at once with that answer.
func TestVMHAConfigCreateWaitStopsOnAnotherError(t *testing.T) {
	shortVMHAClusterWait(t, time.Millisecond, time.Minute)
	f := newRoutedResmgr(t, resmgrRoutes{
		"POST " + vmhaPath:   vmhaSequence(vmhaClusterRefusal, vmhaText(http.StatusInternalServerError, "database is locked")),
		"GET " + clusterPath: reply(http.StatusOK, clusterJSON),
	})
	r := &clusterVMHAConfigResource{config: f.config}
	s := schemaOf(t, r)
	resp := runCreate(r, newPlan(t, s, vmhaModel(types.StringUnknown(), types.StringValue("balanced"), types.StringUnknown())))
	if !resp.Diagnostics.HasError() || !strings.Contains(resp.Diagnostics[0].Detail(), "database is locked") {
		t.Fatalf("diags = %v, want hamgr's 500", resp.Diagnostics)
	}
	if got := f.received(); vmhaCount(got, "POST "+vmhaPath) != 2 {
		t.Fatalf("requests = %v, want two POSTs", got)
	}
	if !resp.State.Raw.IsNull() {
		t.Fatal("a refused write was recorded in state")
	}
}

// The router's 404 is not the cluster refusal: no resmgr request, no retry.
func TestVMHAConfigCreateDoesNotWaitOnTheRouter404(t *testing.T) {
	shortVMHAClusterWait(t, time.Millisecond, time.Minute)
	f := newRoutedResmgr(t, resmgrRoutes{"POST " + vmhaPath: vmhaText(http.StatusNotFound, "404 page not found\n")})
	r := &clusterVMHAConfigResource{config: f.config}
	s := schemaOf(t, r)
	resp := runCreate(r, newPlan(t, s, vmhaModel(types.StringUnknown(), types.StringValue("balanced"), types.StringUnknown())))
	if !resp.Diagnostics.HasError() {
		t.Fatal("create succeeded")
	}
	for _, want := range []string{"hamgr does not serve this URL", "If endpoint_overrides.hamgr is set, check it", "may predate"} {
		if !strings.Contains(resp.Diagnostics[0].Detail(), want) {
			t.Fatalf("detail = %q, want it to say %q", resp.Diagnostics[0].Detail(), want)
		}
	}
	if got := f.received(); !slices.Equal(got, []string{"POST " + vmhaPath}) {
		t.Fatalf("requests = %v, want the one POST", got)
	}
}

// vmhaAfterSecondPOST runs then once the provider has read and closed hamgr's
// answer to the second POST, on the goroutine that made the request.
type vmhaAfterSecondPOST struct {
	base  http.RoundTripper
	posts int
	then  func()
}

func (a *vmhaAfterSecondPOST) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := a.base.RoundTrip(req)
	if req.Method == http.MethodPost {
		a.posts++
		if a.posts == 2 && err == nil {
			resp.Body = &vmhaOnClose{ReadCloser: resp.Body, then: a.then}
		}
	}
	return resp, err
}

type vmhaOnClose struct {
	io.ReadCloser
	then func()
}

func (b *vmhaOnClose) Close() error {
	err := b.ReadCloser.Close()
	b.then()
	return err
}

// A wait the user interrupts says so. Interrupted between attempts, every
// write so far was refused and nothing was written; interrupted while a
// retried write is unanswered, that write's outcome is unknown. Neither case
// depends on how fast the runner is: between attempts, the cancel comes once
// the provider has closed hamgr's second refusal, and the interval is then
// set to an hour, so only the cancel can end the wait; during a write, the
// second POST's handler cancels and does not answer.
func TestVMHAConfigCreateWaitInterrupted(t *testing.T) {
	for _, tc := range []struct {
		name          string
		inFlight      bool
		want, notWant string
	}{
		{"between attempts", false, "nothing was written", "unknown"},
		{"during a write", true, "the outcome of the last write is unknown, and the next apply sends the settings again (", "nothing was written"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			shortVMHAClusterWait(t, time.Millisecond, time.Hour)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			second := vmhaClusterRefusal
			if tc.inFlight {
				second = func(w http.ResponseWriter, r *http.Request) {
					cancel()
					vmhaHang(w, r)
				}
			}
			f := newRoutedResmgr(t, resmgrRoutes{
				"POST " + vmhaPath:   vmhaSequence(vmhaClusterRefusal, second, vmhaClusterRefusal),
				"GET " + clusterPath: reply(http.StatusOK, clusterJSON),
			})
			if !tc.inFlight {
				f.config.Provider.HTTPClient.Transport = &vmhaAfterSecondPOST{
					base: f.config.Provider.HTTPClient.Transport,
					then: func() { vmhaClusterKnownInterval = time.Hour; cancel() },
				}
			}
			r := &clusterVMHAConfigResource{config: f.config}
			s := schemaOf(t, r)
			plan := newPlan(t, s, vmhaModel(types.StringUnknown(), types.StringValue("balanced"), types.StringUnknown()))
			resp := resource.CreateResponse{State: tfsdk.State{Schema: s, Raw: tftypes.NewValue(s.Type().TerraformType(context.Background()), nil)}}
			done := make(chan struct{})
			go func() {
				r.Create(ctx, resource.CreateRequest{Plan: plan}, &resp)
				close(done)
			}()
			select {
			case <-done:
			case <-time.After(10 * time.Second):
				t.Fatal("Create did not return after its context was canceled")
			}
			if !resp.Diagnostics.HasError() {
				t.Fatal("create succeeded")
			}
			detail := resp.Diagnostics[0].Detail()
			if !strings.Contains(detail, "was interrupted") || !strings.Contains(detail, tc.want) || strings.Contains(detail, tc.notWant) {
				t.Fatalf("detail = %q, want the interrupted error saying %q and not %q", detail, tc.want, tc.notWant)
			}
			if got := f.received(); vmhaCount(got, "POST "+vmhaPath) != 2 {
				t.Fatalf("requests = %v, want two POSTs", got)
			}
			if !resp.State.Raw.IsNull() {
				t.Fatal("an interrupted write was recorded in state")
			}
		})
	}
}

// Update writes through the same wait.
func TestVMHAConfigUpdateWaitsForHamgrToLearnOfTheCluster(t *testing.T) {
	shortVMHAClusterWait(t, time.Millisecond, time.Minute)
	posts := &vmhaPosts{}
	f := newRoutedResmgr(t, resmgrRoutes{
		"POST " + vmhaPath:   vmhaSequence(vmhaClusterRefusal, posts.handle),
		"GET " + clusterPath: reply(http.StatusOK, clusterJSON),
		"GET " + vmhaPath:    reply(http.StatusOK, `{"mode": "aggressive", "host_config": {}}`),
	})
	r := &clusterVMHAConfigResource{config: f.config}
	s := schemaOf(t, r)
	prior := newState(t, s, vmhaModel(types.StringValue("cluster-1"), types.StringValue("balanced"), types.StringValue("")))
	plan := newPlan(t, s, vmhaModel(types.StringValue("cluster-1"), types.StringValue("aggressive"), types.StringUnknown()))
	resp := runUpdate(r, plan, prior)
	if resp.Diagnostics.HasError() {
		t.Fatalf("update: %v", resp.Diagnostics)
	}
	if got := f.received(); vmhaCount(got, "POST "+vmhaPath) != 2 || len(posts.bodies) != 1 || posts.bodies[0] != `{"mode":"aggressive"}` {
		t.Fatalf("requests = %v, bodies = %q", got, posts.bodies)
	}
	if row := vmhaRow(t, resp.State); row.EvacuationSpeed.ValueString() != "aggressive" {
		t.Fatalf("state = %+v", row)
	}
}

// Update sends only the settings whose planned value differs from the state:
// a body that carries mode makes hamgr reset the cluster's cooldown and
// post_delay, so an unchanged speed must not be sent again.
func TestVMHAConfigUpdateSendsOnlyWhatChanges(t *testing.T) {
	for _, tc := range []struct {
		name        string
		speed, path types.String
		wantBody    string
	}{
		{"path changes", types.StringValue("balanced"), types.StringValue("/mnt/new"), `{"host_config":{"nfs_heartbeat_path":"/mnt/new"}}`},
		{"speed changes", types.StringValue("aggressive"), types.StringValue("/mnt/hb"), `{"mode":"aggressive"}`},
		{"both change", types.StringValue("aggressive"), types.StringValue("/mnt/new"), `{"mode":"aggressive","host_config":{"nfs_heartbeat_path":"/mnt/new"}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			posts := &vmhaPosts{}
			f := newRoutedResmgr(t, resmgrRoutes{
				"POST " + vmhaPath: posts.handle,
				"GET " + vmhaPath: reply(http.StatusOK, fmt.Sprintf(`{"mode": %q, "cooldown": 20, "host_config": {"nfs_heartbeat_path": %q}}`,
					tc.speed.ValueString(), tc.path.ValueString())),
			})
			r := &clusterVMHAConfigResource{config: f.config}
			s := schemaOf(t, r)
			prior := newState(t, s, vmhaModel(types.StringValue("cluster-1"), types.StringValue("balanced"), types.StringValue("/mnt/hb")))
			resp := runUpdate(r, newPlan(t, s, vmhaModel(types.StringValue("cluster-1"), tc.speed, tc.path)), prior)
			if resp.Diagnostics.HasError() {
				t.Fatalf("update: %v", resp.Diagnostics)
			}
			if got := f.received(); !slices.Equal(got, []string{"POST " + vmhaPath, "GET " + vmhaPath}) {
				t.Fatalf("requests = %v", got)
			}
			if len(posts.bodies) != 1 || posts.bodies[0] != tc.wantBody {
				t.Fatalf("POST bodies = %q, want %s", posts.bodies, tc.wantBody)
			}
			if got := vmhaRow(t, resp.State); !got.EvacuationSpeed.Equal(tc.speed) || !got.NFSHeartbeatPath.Equal(tc.path) {
				t.Fatalf("state = %+v", got)
			}
		})
	}
}

// An Update whose settings match the state sends nothing and only reads back.
func TestVMHAConfigUpdateWithNothingToSendOnlyReads(t *testing.T) {
	f := newRoutedResmgr(t, resmgrRoutes{"GET " + vmhaPath: reply(http.StatusOK, `{"mode": "balanced", "host_config": {"nfs_heartbeat_path": "/mnt/hb"}}`)})
	r := &clusterVMHAConfigResource{config: f.config}
	s := schemaOf(t, r)
	prior := newState(t, s, vmhaModel(types.StringValue("cluster-1"), types.StringValue("balanced"), types.StringValue("/mnt/hb")))
	resp := runUpdate(r, newPlan(t, s, vmhaModel(types.StringValue("cluster-1"), types.StringValue("balanced"), types.StringUnknown())), prior)
	if resp.Diagnostics.HasError() {
		t.Fatalf("update: %v", resp.Diagnostics)
	}
	if got := f.received(); !slices.Equal(got, []string{"GET " + vmhaPath}) {
		t.Fatalf("requests = %v, want only the read-back", got)
	}
}

// A read-back that fails after an accepted Update says the settings were
// written and that the next apply reads them.
func TestVMHAConfigUpdateReadBackFailureSaysTheWriteHappened(t *testing.T) {
	posts := &vmhaPosts{}
	f := newRoutedResmgr(t, resmgrRoutes{
		"POST " + vmhaPath: posts.handle,
		"GET " + vmhaPath:  vmhaText(http.StatusServiceUnavailable, "database unavailable"),
	})
	r := &clusterVMHAConfigResource{config: f.config}
	s := schemaOf(t, r)
	prior := newState(t, s, vmhaModel(types.StringValue("cluster-1"), types.StringValue("balanced"), types.StringValue("")))
	resp := runUpdate(r, newPlan(t, s, vmhaModel(types.StringValue("cluster-1"), types.StringValue("aggressive"), types.StringValue(""))), prior)
	if !resp.Diagnostics.HasError() {
		t.Fatal("update succeeded")
	}
	for _, want := range []string{"database unavailable", "hamgr accepted the settings", "running the apply again reads them"} {
		if !strings.Contains(resp.Diagnostics[0].Detail(), want) {
			t.Fatalf("detail = %q, want it to say %q", resp.Diagnostics[0].Detail(), want)
		}
	}
	if len(posts.bodies) != 1 {
		t.Fatalf("POST bodies = %q, want one write", posts.bodies)
	}
}

// The retry waits vmhaClusterKnownInterval between attempts: a create refused
// twice takes at least two intervals. Only the lower bound is asserted.
func TestVMHAConfigWaitKeepsItsInterval(t *testing.T) {
	const interval = 20 * time.Millisecond
	shortVMHAClusterWait(t, interval, time.Minute)
	posts := &vmhaPosts{}
	f := newRoutedResmgr(t, resmgrRoutes{
		"POST " + vmhaPath:   vmhaSequence(vmhaClusterRefusal, vmhaClusterRefusal, posts.handle),
		"GET " + clusterPath: reply(http.StatusOK, clusterJSON),
		"GET " + vmhaPath:    reply(http.StatusOK, `{"mode": "balanced"}`),
	})
	r := &clusterVMHAConfigResource{config: f.config}
	s := schemaOf(t, r)
	start := time.Now()
	resp := runCreate(r, newPlan(t, s, vmhaModel(types.StringUnknown(), types.StringValue("balanced"), types.StringUnknown())))
	elapsed := time.Since(start)
	if resp.Diagnostics.HasError() {
		t.Fatalf("create: %v", resp.Diagnostics)
	}
	if elapsed < 2*interval {
		t.Fatalf("create took %s, want at least two intervals of %s between three POSTs", elapsed, interval)
	}
}

// A retried write hamgr does not answer is abandoned after
// vmhaClusterKnownCheckTimeout and ends the wait with an error that says so.
func TestVMHAConfigWaitBoundsEachAttempt(t *testing.T) {
	shortVMHAClusterWait(t, time.Millisecond, 10*time.Second)
	vmhaClusterKnownCheckTimeout = 50 * time.Millisecond
	f := newRoutedResmgr(t, resmgrRoutes{
		"POST " + vmhaPath:   vmhaSequence(vmhaClusterRefusal, vmhaHang),
		"GET " + clusterPath: reply(http.StatusOK, clusterJSON),
	})
	r := &clusterVMHAConfigResource{config: f.config}
	s := schemaOf(t, r)
	resp := runCreate(r, newPlan(t, s, vmhaModel(types.StringUnknown(), types.StringValue("balanced"), types.StringUnknown())))
	if !resp.Diagnostics.HasError() || !strings.Contains(resp.Diagnostics[0].Detail(), `did not answer a write for cluster "cluster-1" within 50ms`) {
		t.Fatalf("diags = %v, want the unanswered-write error", resp.Diagnostics)
	}
	if got := f.received(); vmhaCount(got, "POST "+vmhaPath) != 2 {
		t.Fatalf("requests = %v, want two POSTs", got)
	}
	if !resp.State.Raw.IsNull() {
		t.Fatal("an unanswered write was recorded in state")
	}
}

// The schema refuses a cluster name that cannot address its configuration
// and an evacuation speed hamgr does not know.
func TestVMHAConfigSchemaValidators(t *testing.T) {
	r := &clusterVMHAConfigResource{}
	s := schemaOf(t, r)
	ctx := context.Background()
	for _, tc := range []struct {
		attr, value string
		wantErr     bool
	}{
		{"cluster_name", "a/b", true},
		{"cluster_name", "..", true},
		{"cluster_name", "cluster-1", false},
		{"evacuation_speed", "fast", true},
		{"evacuation_speed", "balanced", false},
	} {
		t.Run(tc.attr+"="+tc.value, func(t *testing.T) {
			attr, ok := s.Attributes[tc.attr].(interface {
				StringValidators() []validator.String
			})
			if !ok {
				t.Fatalf("%s has no string validators", tc.attr)
			}
			var resp validator.StringResponse
			for _, v := range attr.StringValidators() {
				v.ValidateString(ctx, validator.StringRequest{Path: path.Root(tc.attr), ConfigValue: types.StringValue(tc.value)}, &resp)
			}
			if resp.Diagnostics.HasError() != tc.wantErr {
				t.Fatalf("diags = %v, want error %v", resp.Diagnostics, tc.wantErr)
			}
		})
	}
}

// The default wait outlasts hamgr's 300-second cache of resmgr's cluster
// list, and its interval does not hammer hamgr.
func TestVMHAConfigWaitDefaults(t *testing.T) {
	if vmhaClusterKnownTimeout <= 300*time.Second {
		t.Errorf("vmhaClusterKnownTimeout = %s, want longer than hamgr's 300 s cache", vmhaClusterKnownTimeout)
	}
	if vmhaClusterKnownInterval < 5*time.Second {
		t.Errorf("vmhaClusterKnownInterval = %s, want at least 5 s", vmhaClusterKnownInterval)
	}
}

// An Update whose retried write is unanswered when the wait ends keeps the
// prior state: the next apply's refresh reads hamgr, and only a setting hamgr
// does not hold is sent again.
func TestVMHAConfigUpdateWaitTimesOutDuringAWrite(t *testing.T) {
	shortVMHAClusterWait(t, time.Millisecond, time.Second)
	f := newRoutedResmgr(t, resmgrRoutes{
		"POST " + vmhaPath:   vmhaSequence(vmhaClusterRefusal, vmhaHang),
		"GET " + clusterPath: reply(http.StatusOK, clusterJSON),
	})
	r := &clusterVMHAConfigResource{config: f.config}
	s := schemaOf(t, r)
	prior := newState(t, s, vmhaModel(types.StringValue("cluster-1"), types.StringValue("balanced"), types.StringValue("")))
	resp := runUpdate(r, newPlan(t, s, vmhaModel(types.StringValue("cluster-1"), types.StringValue("aggressive"), types.StringValue(""))), prior)
	if !resp.Diagnostics.HasError() {
		t.Fatal("update succeeded")
	}
	detail := resp.Diagnostics[0].Detail()
	if !strings.Contains(detail, "the outcome of the last write is unknown, and the next apply sends the settings again if hamgr does not hold them") {
		t.Fatalf("detail = %q, want the conditional wording", detail)
	}
	if got := f.received(); vmhaCount(got, "POST "+vmhaPath) != 2 {
		t.Fatalf("requests = %v, want two POSTs", got)
	}
}
