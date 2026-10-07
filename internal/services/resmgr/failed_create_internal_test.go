// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package resmgr

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"

	"github.com/platform9/terraform-provider-pcd/internal/clients"
)

// This file holds an offline resmgr fake and the framework plumbing for the
// tests of what pcd_cluster_blueprint, pcd_cluster and pcd_host_config leave
// in state when the read-back after a successful write fails. A test describes
// resmgr as a set of routes, replaces the one whose failure it exercises, and
// drives the resource's own Create, Read and Update against it.

// fakeConfig points a clients.Config at a test server, so the resmgr clients
// the resources build resolve to it. The locator ignores the endpoint options,
// so it answers for every service type. resmgrClient appends the API version to
// the root the locator returns, so every request path carries a /v2/ prefix.
// EndpointOverrides stays empty, so the client is built from the locator alone.
// Its client gets a transport of its own, not http.DefaultTransport: every
// httptest.Server.Close closes the default transport's idle connections,
// which can cut a request another parallel test is sending.
func fakeConfig(url string) *clients.Config {
	return &clients.Config{
		Region: "region-one",
		Provider: &gophercloud.ProviderClient{
			HTTPClient:      http.Client{Transport: &http.Transport{}},
			EndpointLocator: func(gophercloud.EndpointOpts) (string, error) { return url + "/", nil },
		},
	}
}

// resmgrRoutes maps a request, written "METHOD /path" (for example
// "GET /v2/blueprint/bp-1"), to the handler that answers it.
type resmgrRoutes map[string]http.HandlerFunc

// routedResmgr is a test server that answers from its routes and logs every
// request it receives, so a test can assert what a resource sent. Unlike
// fakeResmgr, which models one host's dns role, it knows no calls of its own.
// It serves one request at a time, so a route may keep a counter without a
// lock of its own. A request no route names fails the test.
type routedResmgr struct {
	// config builds resmgr clients that reach this server.
	config *clients.Config

	mu       sync.Mutex
	requests []string
}

// newRoutedResmgr starts a routedResmgr serving routes. The server closes when
// the test ends.
func newRoutedResmgr(t *testing.T, routes resmgrRoutes) *routedResmgr {
	t.Helper()
	f := &routedResmgr{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		call := r.Method + " " + r.URL.Path
		f.requests = append(f.requests, call)
		if handle, ok := routes[call]; ok {
			handle(w, r)
			return
		}
		// The test has already failed; 501 only answers the call, so the
		// resource does not wait on it.
		t.Errorf("unexpected request %s", call)
		w.WriteHeader(http.StatusNotImplemented)
	}))
	t.Cleanup(srv.Close)
	f.config = fakeConfig(srv.URL)
	return f
}

// received returns the requests served so far, in order, each written
// "METHOD /path".
func (f *routedResmgr) received() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.requests...)
}

// reply answers with status and, unless body is empty, a JSON body. A route
// that models success must answer with a status the provider accepts for that
// call: 200 for a GET, and 200 for a POST or PUT, which postJSON and putJSON
// accept along with other 2xx codes. The provider ignores the body of a write.
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

// badGateway answers 502 with nginx's HTML page, as the ingress in front of
// resmgr can. The body is not JSON.
func badGateway(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html")
	w.WriteHeader(http.StatusBadGateway)
	fmt.Fprint(w, nginxBadGatewayPage)
}

// badGatewayFirst answers the first request with badGateway and every later
// one with then: a call that fails once and works when it is retried, for
// example by the refresh that follows a failed read-back.
func badGatewayFirst(then http.HandlerFunc) http.HandlerFunc {
	calls := 0
	return func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			badGateway(w, r)
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

// runCreate calls Create with a response state that starts as a null object,
// so a Create that returns early leaves no state.
func runCreate(r resource.Resource, plan tfsdk.Plan) resource.CreateResponse {
	resp := resource.CreateResponse{State: tfsdk.State{Schema: plan.Schema, Raw: tftypes.NewValue(plan.Schema.Type().TerraformType(context.Background()), nil)}}
	r.Create(context.Background(), resource.CreateRequest{Plan: plan}, &resp)
	return resp
}

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

const (
	blueprintName  = "bp-1"
	blueprintVNCIP = "10.0.0.5"
	// blueprintBackends is a storage_backends_json value as jsonencode writes
	// it: compact, with sorted keys.
	blueprintBackends = `{"nfs":{"nfs-1":{"config":{"nfs_mount_points":"10.0.0.2:/export"},"driver":"NFS"}}}`

	blueprintsPath = "/v2/blueprint"
	blueprintPath  = blueprintsPath + "/" + blueprintName
)

// blueprintJSON is bp-1 as resmgr returns it once the vnc_floating_ip PUT has
// persisted the IP.
const blueprintJSON = `{"name": "bp-1", "networkingType": "ovn", "enableDistributedRouting": true,
	"dnsDomainName": "pcd.local", "imageLibraryStorage": "", "imageLibrarySharedStorage": false,
	"instanceSharedStorage": false, "vmStorage": "/opt/data/instances", "vncFloatingIp": "10.0.0.5",
	"virtualNetworking": {"enabled": true, "underlayType": "geneve", "vnidRange": "1000:2000"},
	"storageBackends": {"nfs": {"nfs-1": {"driver": "NFS", "config": {"nfs_mount_points": "10.0.0.2:/export"}}}}}`

// blueprintRoutes answers every call blueprintResource makes for bp-1. Each
// test replaces the route whose failure it exercises.
func blueprintRoutes() resmgrRoutes {
	return resmgrRoutes{
		"POST " + blueprintsPath: reply(http.StatusOK, ""),
		"PUT " + blueprintPath:   reply(http.StatusOK, ""),
		"GET " + blueprintPath:   reply(http.StatusOK, blueprintJSON),
	}
}

// geneveNetworking is a virtual_networking value with geneve tunnels and the
// given vnid_range.
func geneveNetworking(vnidRange types.String) types.Object {
	return types.ObjectValueMust(virtualNetworkingAttrTypes, map[string]attr.Value{
		"enabled":       types.BoolValue(true),
		"underlay_type": types.StringValue("geneve"),
		"vnid_range":    vnidRange,
	})
}

// blueprintPlan is the plan the framework computes for a new blueprint whose
// config sets name, storage_backends_json, vnc_floating_ip, and every leaf of
// virtual_networking. Every attribute the config leaves unset is unknown.
func blueprintPlan(t *testing.T, r *blueprintResource) tfsdk.Plan {
	t.Helper()
	return newPlan(t, schemaOf(t, r), &blueprintResourceModel{
		Name:                      types.StringValue(blueprintName),
		NetworkingType:            types.StringUnknown(),
		EnableDistributedRouting:  types.BoolUnknown(),
		DNSDomainName:             types.StringUnknown(),
		VirtualNetworking:         geneveNetworking(types.StringValue("1000:2000")),
		ImageLibraryStorage:       types.StringUnknown(),
		ImageLibrarySharedStorage: types.BoolUnknown(),
		InstanceSharedStorage:     types.BoolUnknown(),
		VMStorage:                 types.StringUnknown(),
		VNCFloatingIP:             types.StringValue(blueprintVNCIP),
		StorageBackendsJSON:       types.StringValue(blueprintBackends),
	})
}

// blueprintRow returns the blueprint state holds.
func blueprintRow(t *testing.T, state tfsdk.State) blueprintResourceModel {
	t.Helper()
	var m blueprintResourceModel
	if d := state.Get(context.Background(), &m); d.HasError() {
		t.Fatalf("reading the state: %v", d)
	}
	return m
}

// A create whose every step succeeds must return the blueprint fully known,
// after the PUT that persists vnc_floating_ip.
func TestBlueprintCreateFillsEveryAttribute(t *testing.T) {
	t.Parallel()
	fake := newRoutedResmgr(t, blueprintRoutes())
	r := &blueprintResource{config: fake.config}

	resp := runCreate(r, blueprintPlan(t, r))
	if resp.Diagnostics.HasError() {
		t.Fatalf("create: %v", resp.Diagnostics)
	}
	if !resp.State.Raw.IsFullyKnown() {
		t.Fatalf("create state holds unknown values, which Terraform refuses: %v", resp.State.Raw)
	}
	got := blueprintRow(t, resp.State)
	if got.NetworkingType.ValueString() != "ovn" || got.VNCFloatingIP.ValueString() != blueprintVNCIP ||
		!got.VirtualNetworking.Equal(geneveNetworking(types.StringValue("1000:2000"))) {
		t.Fatalf("create state networking_type=%s vnc_floating_ip=%s virtual_networking=%s; want ovn, %s, geneve 1000:2000",
			got.NetworkingType, got.VNCFloatingIP, got.VirtualNetworking, blueprintVNCIP)
	}
	want := []string{"POST " + blueprintsPath, "PUT " + blueprintPath, "GET " + blueprintPath}
	if sent := fake.received(); !slices.Equal(sent, want) {
		t.Fatalf("create sent %v, want %v", sent, want)
	}
}

// resmgr keeps a blueprint whose vnc_floating_ip PUT fails. Create used to
// return the error before it set any state, so Terraform forgot the blueprint
// and the next apply tried to create it again. Create must return the error
// with the blueprint in state (Terraform then taints it).
func TestBlueprintCreateKeepsStateWhenVNCPutFails(t *testing.T) {
	t.Parallel()
	routes := blueprintRoutes()
	routes["PUT "+blueprintPath] = badGateway
	fake := newRoutedResmgr(t, routes)
	r := &blueprintResource{config: fake.config}

	resp := runCreate(r, blueprintPlan(t, r))
	if !resp.Diagnostics.HasError() {
		t.Fatal("create succeeded; want the 502 on the vnc_floating_ip PUT reported")
	}
	if resp.State.Raw.IsNull() {
		t.Fatal("create returned no state: Terraform forgets bp-1 and the next apply creates it again")
	}
	if !resp.State.Raw.IsFullyKnown() {
		t.Fatalf("create state holds unknown values, which Terraform saves as null: %v", resp.State.Raw)
	}
	if got := blueprintRow(t, resp.State); got.Name.ValueString() != blueprintName {
		t.Fatalf("create state name = %s, want bp-1: the name is the blueprint's key", got.Name)
	}
	want := []string{"POST " + blueprintsPath, "PUT " + blueprintPath}
	if sent := fake.received(); !slices.Equal(sent, want) {
		t.Fatalf("create sent %v, want %v", sent, want)
	}
}

// A failed read-back after the POST stays a warning for a blueprint: an error
// taints it, and the next apply would then delete and re-create the region's
// blueprint over one failed GET. Create used to return the plan's unknowns,
// which Terraform reports as errors and saves as null in a tainted row. Create
// must warn without an error and return a fully known row, and a refresh must
// fill in what the read-back would have.
func TestBlueprintCreateKeepsStateWhenReadBackFails(t *testing.T) {
	t.Parallel()
	routes := blueprintRoutes()
	routes["GET "+blueprintPath] = badGatewayFirst(routes["GET "+blueprintPath])
	r := &blueprintResource{config: newRoutedResmgr(t, routes).config}

	createResp := runCreate(r, blueprintPlan(t, r))
	if createResp.Diagnostics.HasError() || createResp.Diagnostics.WarningsCount() != 1 {
		t.Fatalf("diagnostics = %v, want exactly one warning and no error", createResp.Diagnostics)
	}
	if createResp.State.Raw.IsNull() {
		t.Fatal("create returned no state: Terraform forgets bp-1 and the next apply creates it again")
	}
	if !createResp.State.Raw.IsFullyKnown() {
		t.Fatalf("create state holds unknown values, which Terraform refuses and then taints the blueprint over: %v",
			createResp.State.Raw)
	}
	got := blueprintRow(t, createResp.State)
	if got.Name.ValueString() != blueprintName || got.VNCFloatingIP.ValueString() != blueprintVNCIP ||
		got.StorageBackendsJSON.ValueString() != blueprintBackends {
		t.Fatalf("create state name=%s vnc_floating_ip=%s storage_backends_json=%s; want the configured values",
			got.Name, got.VNCFloatingIP, got.StorageBackendsJSON)
	}

	// The next plan refreshes the blueprint first.
	readResp := runRead(r, createResp.State)
	if readResp.Diagnostics.HasError() {
		t.Fatalf("refresh of the recorded blueprint: %v", readResp.Diagnostics)
	}
	if readResp.State.Raw.IsNull() {
		t.Fatal("refresh dropped bp-1 from state")
	}
	got = blueprintRow(t, readResp.State)
	if got.NetworkingType.ValueString() != "ovn" || got.VMStorage.ValueString() != "/opt/data/instances" ||
		!got.VirtualNetworking.Equal(geneveNetworking(types.StringValue("1000:2000"))) {
		t.Fatalf("refreshed networking_type=%s vm_storage=%s virtual_networking=%s; want ovn, /opt/data/instances, geneve 1000:2000",
			got.NetworkingType, got.VMStorage, got.VirtualNetworking)
	}
}

// An update whose read-back fails stays a warning, but must not return the
// plan's unknowns: a virtual_networking block that leaves vnid_range unset
// plans that leaf unknown on every update, and Terraform refuses it in state.
func TestBlueprintUpdateKeepsStateKnownWhenReadBackFails(t *testing.T) {
	t.Parallel()
	routes := blueprintRoutes()
	// Update reads the stored blueprint before its PUT; the read-back after the
	// PUT is the GET that fails.
	gets := 0
	readBefore := routes["GET "+blueprintPath]
	routes["GET "+blueprintPath] = func(w http.ResponseWriter, r *http.Request) {
		gets++
		if gets == 1 {
			readBefore(w, r)
			return
		}
		badGateway(w, r)
	}
	r := &blueprintResource{config: newRoutedResmgr(t, routes).config}
	s := schemaOf(t, r)

	prior := blueprintResourceModel{
		Name:                      types.StringValue(blueprintName),
		NetworkingType:            types.StringValue("ovn"),
		EnableDistributedRouting:  types.BoolValue(true),
		DNSDomainName:             types.StringValue("pcd.local"),
		VirtualNetworking:         geneveNetworking(types.StringValue("1000:2000")),
		ImageLibraryStorage:       types.StringValue(""),
		ImageLibrarySharedStorage: types.BoolValue(false),
		InstanceSharedStorage:     types.BoolValue(false),
		VMStorage:                 types.StringValue("/opt/data/instances"),
		VNCFloatingIP:             types.StringValue(blueprintVNCIP),
		StorageBackendsJSON:       types.StringValue(blueprintBackends),
	}
	// A new dns_domain_name. The config sets virtual_networking without
	// vnid_range, so the plan leaves that leaf unknown.
	planned := prior
	planned.DNSDomainName = types.StringValue("pcd.example")
	planned.VirtualNetworking = geneveNetworking(types.StringUnknown())

	resp := runUpdate(r, newPlan(t, s, &planned), newState(t, s, &prior))
	if resp.Diagnostics.HasError() || resp.Diagnostics.WarningsCount() != 1 {
		t.Fatalf("diagnostics = %v, want exactly one warning and no error", resp.Diagnostics)
	}
	if !resp.State.Raw.IsFullyKnown() {
		t.Fatalf("update state holds unknown values, which Terraform refuses: %v", resp.State.Raw)
	}
	if got := blueprintRow(t, resp.State); got.DNSDomainName.ValueString() != "pcd.example" {
		t.Fatalf("update state dns_domain_name = %s, want pcd.example: the PUT applied it", got.DNSDomainName)
	}
}

const (
	clusterName = "cluster-1"

	clustersPath = "/v2/clusters"
	clusterPath  = clustersPath + "/" + clusterName
)

// clusterJSON is cluster-1 as resmgr returns it after a create whose config set
// only auto_resource_rebalancing = { enabled = false }: toAPI sends every
// group, resmgr stores them as sent, and it adds its default
// rebalancingFrequencyMins to a rebalancing block sent without one.
const clusterJSON = `{"name": "cluster-1", "vmHighAvailability": {"enabled": false},
	"autoResourceRebalancing": {"enabled": false, "rebalancingFrequencyMins": 10},
	"gpu": {"enabled": false, "mode": ""}, "cpu": {"mode": null, "model": null}}`

// clusterRoutes answers every call clusterResource makes for cluster-1. Each
// test replaces the route whose failure it exercises.
func clusterRoutes() resmgrRoutes {
	return resmgrRoutes{
		"POST " + clustersPath: reply(http.StatusOK, ""),
		"PUT " + clusterPath:   reply(http.StatusOK, ""),
		"GET " + clusterPath:   reply(http.StatusOK, clusterJSON),
	}
}

// clusterPlan is the plan the framework computes for a new cluster whose
// config sets name and auto_resource_rebalancing = { enabled = false }. The
// blocks the config leaves unset are unknown, and so are the rebalancing
// leaves it leaves unset.
func clusterPlan(t *testing.T, r *clusterResource) tfsdk.Plan {
	t.Helper()
	return newPlan(t, schemaOf(t, r), &clusterModel{
		Name:                    types.StringValue(clusterName),
		VMHighAvailability:      types.ObjectUnknown(haAttrTypes),
		AutoResourceRebalancing: rb(types.BoolValue(false), types.StringUnknown(), types.Int64Unknown()),
		GPU:                     types.ObjectUnknown(clusterGPUAttrTypes),
		CPU:                     types.ObjectUnknown(clusterCPUAttrTypes),
	})
}

// clusterRow returns the cluster state holds.
func clusterRow(t *testing.T, state tfsdk.State) clusterModel {
	t.Helper()
	var m clusterModel
	if d := state.Get(context.Background(), &m); d.HasError() {
		t.Fatalf("reading the state: %v", d)
	}
	return m
}

// gpuDisabled is the gpu block toAPI sends when the config leaves it unset,
// as resmgr then reports it.
var gpuDisabled = types.ObjectValueMust(clusterGPUAttrTypes, map[string]attr.Value{
	"enabled": types.BoolValue(false),
	"mode":    types.StringValue(""),
})

// A create whose every step succeeds must return the cluster fully known.
func TestClusterCreateFillsEveryAttribute(t *testing.T) {
	t.Parallel()
	fake := newRoutedResmgr(t, clusterRoutes())
	r := &clusterResource{config: fake.config}

	resp := runCreate(r, clusterPlan(t, r))
	if resp.Diagnostics.HasError() {
		t.Fatalf("create: %v", resp.Diagnostics)
	}
	if !resp.State.Raw.IsFullyKnown() {
		t.Fatalf("create state holds unknown values, which Terraform refuses: %v", resp.State.Raw)
	}
	got := clusterRow(t, resp.State)
	if !got.AutoResourceRebalancing.Equal(serverDisabled) || !got.GPU.Equal(gpuDisabled) {
		t.Fatalf("create state auto_resource_rebalancing=%s gpu=%s; want %s and %s",
			got.AutoResourceRebalancing, got.GPU, serverDisabled, gpuDisabled)
	}
	want := []string{"POST " + clustersPath, "GET " + clusterPath}
	if sent := fake.received(); !slices.Equal(sent, want) {
		t.Fatalf("create sent %v, want %v", sent, want)
	}
}

// A failed read-back after the POST stays a warning for a cluster: an error
// taints it, and the next apply would then delete and re-create the cluster
// over one failed GET. Create used to return the plan's unknowns, which
// Terraform reports as errors and saves as null in a tainted row. Create must
// warn without an error and return a fully known row, and a refresh must fill
// in what the read-back would have.
func TestClusterCreateKeepsStateWhenReadBackFails(t *testing.T) {
	t.Parallel()
	routes := clusterRoutes()
	routes["GET "+clusterPath] = badGatewayFirst(routes["GET "+clusterPath])
	r := &clusterResource{config: newRoutedResmgr(t, routes).config}

	createResp := runCreate(r, clusterPlan(t, r))
	if createResp.Diagnostics.HasError() || createResp.Diagnostics.WarningsCount() != 1 {
		t.Fatalf("diagnostics = %v, want exactly one warning and no error", createResp.Diagnostics)
	}
	if createResp.State.Raw.IsNull() {
		t.Fatal("create returned no state: Terraform forgets cluster-1 and the next apply creates it again")
	}
	if !createResp.State.Raw.IsFullyKnown() {
		t.Fatalf("create state holds unknown values, which Terraform refuses and then taints the cluster over: %v",
			createResp.State.Raw)
	}
	if got := clusterRow(t, createResp.State); got.Name.ValueString() != clusterName {
		t.Fatalf("create state name = %s, want cluster-1: the name is the cluster's key", got.Name)
	}

	// The next plan refreshes the cluster first.
	readResp := runRead(r, createResp.State)
	if readResp.Diagnostics.HasError() {
		t.Fatalf("refresh of the recorded cluster: %v", readResp.Diagnostics)
	}
	if readResp.State.Raw.IsNull() {
		t.Fatal("refresh dropped cluster-1 from state")
	}
	got := clusterRow(t, readResp.State)
	if !got.AutoResourceRebalancing.Equal(serverDisabled) || !got.GPU.Equal(gpuDisabled) {
		t.Fatalf("refreshed auto_resource_rebalancing=%s gpu=%s; want %s and %s",
			got.AutoResourceRebalancing, got.GPU, serverDisabled, gpuDisabled)
	}
}

// An update whose read-back fails stays a warning, but must not return the
// plan's unknowns: a block that leaves a leaf unset plans that leaf unknown on
// every update, and Terraform refuses it in state.
func TestClusterUpdateKeepsStateKnownWhenReadBackFails(t *testing.T) {
	t.Parallel()
	routes := clusterRoutes()
	routes["GET "+clusterPath] = badGateway
	r := &clusterResource{config: newRoutedResmgr(t, routes).config}
	s := schemaOf(t, r)

	prior := clusterModel{
		Name:                    types.StringValue(clusterName),
		VMHighAvailability:      types.ObjectValueMust(haAttrTypes, map[string]attr.Value{"enabled": types.BoolValue(false)}),
		AutoResourceRebalancing: serverDisabled,
		GPU:                     gpuDisabled,
		CPU: types.ObjectValueMust(clusterCPUAttrTypes, map[string]attr.Value{
			"mode":  types.StringNull(),
			"model": types.StringNull(),
		}),
	}
	// VM high availability turned on. The config still sets only enabled in
	// auto_resource_rebalancing, so the plan leaves its other leaves unknown.
	planned := prior
	planned.VMHighAvailability = types.ObjectValueMust(haAttrTypes, map[string]attr.Value{"enabled": types.BoolValue(true)})
	planned.AutoResourceRebalancing = rb(types.BoolValue(false), types.StringUnknown(), types.Int64Unknown())

	resp := runUpdate(r, newPlan(t, s, &planned), newState(t, s, &prior))
	if resp.Diagnostics.HasError() || resp.Diagnostics.WarningsCount() != 1 {
		t.Fatalf("diagnostics = %v, want exactly one warning and no error", resp.Diagnostics)
	}
	if !resp.State.Raw.IsFullyKnown() {
		t.Fatalf("update state holds unknown values, which Terraform refuses: %v", resp.State.Raw)
	}
	if got := clusterRow(t, resp.State); !got.VMHighAvailability.Equal(planned.VMHighAvailability) {
		t.Fatalf("update state vm_high_availability = %s, want %s: the PUT applied it",
			got.VMHighAvailability, planned.VMHighAvailability)
	}
}
