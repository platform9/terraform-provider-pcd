// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package loadbalancer

import (
	"context"
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

// fakeConfig points a clients.Config at a test server, so the Octavia client the
// resources build resolves to it. The locator ignores the endpoint options, so
// it answers for every service type and availability. Note that every request
// path carries a /v2.0/ prefix: openstack.NewLoadBalancerV2 sets the client's
// ResourceBase to the endpoint plus "v2.0/", unlike the Cinder and Glance
// clients the other packages' fakes serve. EndpointOverrides must stay empty,
// since an override blanks ResourceBase and drops the prefix.
func fakeConfig(url string) *clients.Config {
	return &clients.Config{
		Region: "region-one",
		Provider: &gophercloud.ProviderClient{
			EndpointLocator: func(gophercloud.EndpointOpts) (string, error) { return url + "/", nil },
		},
	}
}

// A load balancer whose build ends in ERROR stays in Octavia. Create used to
// return without state, so Terraform forgot the load balancer, the next apply
// created another, and the first had to be deleted through the API. Create must
// return the error with the load balancer in state (Terraform then taints it),
// and the refresh and delete a destroy runs must remove it even though Octavia
// reports ERROR on the first poll after the DELETE.
func TestLoadBalancerCreateKeepsALoadBalancerThatFailedToBuild(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	var mu sync.Mutex
	deleteCalled, getsAfterDelete := false, 0
	octavia := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch r.Method + " " + r.URL.Path {
		case "POST /v2.0/lbaas/loadbalancers":
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{"loadbalancer": {"id": "lb-1", "provisioning_status": "PENDING_CREATE"}}`)
		case "GET /v2.0/lbaas/loadbalancers/lb-1":
			if deleteCalled {
				getsAfterDelete++
				if getsAfterDelete > 1 {
					w.WriteHeader(http.StatusNotFound)
					return
				}
			}
			// Octavia keeps answering ERROR on the first poll after the DELETE
			// is accepted; the delete waiter must poll past it, not bail.
			fmt.Fprint(w, `{"loadbalancer": {"id": "lb-1", "name": "tf-lb", "description": "",
				"admin_state_up": true, "vip_subnet_id": "subnet-1", "vip_network_id": "net-1",
				"vip_address": "10.0.0.5", "vip_port_id": "port-1", "flavor_id": "",
				"provider": "ovn", "provisioning_status": "ERROR", "operating_status": "OFFLINE",
				"tags": []}}`)
		case "DELETE /v2.0/lbaas/loadbalancers/lb-1":
			// DeleteOpts{Cascade: true} sends ?cascade=true, which is a query
			// string and so is not part of r.URL.Path.
			if got := r.URL.Query().Get("cascade"); got != "true" {
				t.Errorf("delete sent cascade=%q, want true", got)
			}
			deleteCalled = true
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotImplemented)
		}
	}))
	defer octavia.Close()

	r := &loadBalancerResource{config: fakeConfig(octavia.URL)}
	var sch resource.SchemaResponse
	r.Schema(ctx, resource.SchemaRequest{}, &sch)
	s := sch.Schema

	// vip_subnet_id must be a known, non-empty string: isSet treats unknown as
	// unset, and a plan with neither VIP attribute set trips the "Invalid VIP"
	// guard before Create makes any request. admin_state_up must be known too,
	// since Create dereferences it unconditionally and an unknown Bool would
	// silently send admin_state_up: false.
	planned := loadBalancerModel{
		ID:                 types.StringUnknown(),
		Name:               types.StringValue("tf-lb"),
		Description:        types.StringUnknown(),
		AdminStateUp:       types.BoolValue(true),
		VipSubnetID:        types.StringValue("subnet-1"),
		VipNetworkID:       types.StringUnknown(),
		VipAddress:         types.StringUnknown(),
		VipPortID:          types.StringUnknown(),
		FlavorID:           types.StringUnknown(),
		Provider:           types.StringValue("ovn"),
		Tags:               types.SetUnknown(types.StringType),
		ProvisioningStatus: types.StringUnknown(),
		OperatingStatus:    types.StringUnknown(),
		Region:             types.StringUnknown(),
	}
	plan := tfsdk.Plan{Schema: s, Raw: tftypes.NewValue(s.Type().TerraformType(ctx), nil)}
	if d := plan.Set(ctx, &planned); d.HasError() {
		t.Fatalf("building the plan: %v", d)
	}

	createResp := resource.CreateResponse{State: tfsdk.State{Schema: s, Raw: tftypes.NewValue(s.Type().TerraformType(ctx), nil)}}
	r.Create(ctx, resource.CreateRequest{Plan: plan}, &createResp)
	if !createResp.Diagnostics.HasError() {
		t.Fatal("create succeeded; want the ERROR provisioning status reported")
	}
	if createResp.State.Raw.IsNull() {
		t.Fatal("create returned no state: Terraform forgets lb-1 and the next apply creates a second load balancer")
	}
	if !createResp.State.Raw.IsFullyKnown() {
		t.Fatalf("create state holds unknown values, which Terraform refuses: %v", createResp.State.Raw)
	}
	var got loadBalancerModel
	if d := createResp.State.Get(ctx, &got); d.HasError() {
		t.Fatalf("reading the create state: %v", d)
	}
	if got.ID.ValueString() != "lb-1" {
		t.Fatalf("create state id = %s, want lb-1: a row without an ID names nothing a destroy could delete", got.ID)
	}
	if got.Name.ValueString() != "tf-lb" || got.VipSubnetID.ValueString() != "subnet-1" {
		t.Fatalf("create state name=%s vip_subnet_id=%s; want tf-lb, subnet-1", got.Name, got.VipSubnetID)
	}
	if got.AdminStateUp.IsNull() || !got.AdminStateUp.ValueBool() {
		t.Fatalf("create state admin_state_up = %s, want true", got.AdminStateUp)
	}

	// terraform destroy (or the replacing apply) refreshes the tainted load
	// balancer first.
	readResp := resource.ReadResponse{State: createResp.State}
	r.Read(ctx, resource.ReadRequest{State: createResp.State}, &readResp)
	if readResp.Diagnostics.HasError() {
		t.Fatalf("refresh of the failed load balancer: %v", readResp.Diagnostics)
	}
	if readResp.State.Raw.IsNull() {
		t.Fatal("refresh dropped lb-1 from state; want it kept and reported in ERROR")
	}
	if d := readResp.State.Get(ctx, &got); d.HasError() {
		t.Fatalf("reading the refreshed state: %v", d)
	}
	if got.ProvisioningStatus.ValueString() != lbError {
		t.Fatalf("refreshed provisioning_status = %s, want ERROR", got.ProvisioningStatus)
	}
	if got.Region.ValueString() != "region-one" {
		t.Fatalf("refreshed region = %s, want region-one", got.Region)
	}

	deleteResp := resource.DeleteResponse{State: readResp.State}
	r.Delete(ctx, resource.DeleteRequest{State: readResp.State}, &deleteResp)
	if deleteResp.Diagnostics.HasError() {
		t.Fatalf("delete of a load balancer in ERROR: %v", deleteResp.Diagnostics)
	}
	mu.Lock()
	defer mu.Unlock()
	if !deleteCalled {
		t.Fatal("delete never called DELETE /v2.0/lbaas/loadbalancers/lb-1")
	}
	if getsAfterDelete < 2 {
		t.Fatalf("delete returned after %d polls; want it to poll past the ERROR status until Octavia answers 404", getsAfterDelete)
	}
}

// waitForLoadBalancerDeleted must take a provisioning status of DELETED as
// gone. Octavia normally answers 404 for a deleted load balancer, but the
// waiter used to end only on that 404, so a DELETED answer polled to the full
// 10-minute defaultLBTimeout. The short timeout here keeps a regression to a
// two-second failure instead of a ten-minute hang.
func TestWaitForLoadBalancerDeletedAcceptsDeletedStatus(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	octavia := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method+" "+r.URL.Path != "GET /v2.0/lbaas/loadbalancers/lb-1" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotImplemented)
			return
		}
		fmt.Fprint(w, `{"loadbalancer": {"id": "lb-1", "provisioning_status": "DELETED",
			"operating_status": "OFFLINE", "tags": []}}`)
	}))
	defer octavia.Close()

	client, err := fakeConfig(octavia.URL).LoadBalancerV2Client()
	if err != nil {
		t.Fatalf("building the client: %v", err)
	}
	if err := waitForLoadBalancerDeleted(ctx, client, "lb-1", 2*time.Second); err != nil {
		t.Fatalf("waiting on a load balancer Octavia reports DELETED: %v", err)
	}
}

// A load balancer that never leaves ERROR -- the create abandoned it there and
// the backend delete failed too -- must still fail the destroy, since the load
// balancer really is still present. The failure has to be the timeout, not the
// old first-poll bail, and it has to name the status so the operator knows why
// the wait ran long.
func TestWaitForLoadBalancerDeletedTimesOutOnAnErrorThatNeverClears(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	var mu sync.Mutex
	polls := 0
	octavia := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.Method+" "+r.URL.Path != "GET /v2.0/lbaas/loadbalancers/lb-1" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotImplemented)
			return
		}
		polls++
		fmt.Fprint(w, `{"loadbalancer": {"id": "lb-1", "provisioning_status": "ERROR",
			"operating_status": "OFFLINE", "tags": []}}`)
	}))
	defer octavia.Close()

	client, err := fakeConfig(octavia.URL).LoadBalancerV2Client()
	if err != nil {
		t.Fatalf("building the client: %v", err)
	}
	err = waitForLoadBalancerDeleted(ctx, client, "lb-1", 2*time.Second)
	if err == nil {
		t.Fatal("wait succeeded; want the still-present load balancer reported")
	}
	if strings.Contains(err.Error(), "entered ERROR provisioning status during delete") {
		t.Fatalf("wait bailed on the abandoned ERROR status instead of polling past it: %v", err)
	}
	if want := `last status "ERROR"`; !strings.Contains(err.Error(), want) {
		t.Fatalf("wait error = %q; want it to name the status with %q", err, want)
	}
	mu.Lock()
	defer mu.Unlock()
	if polls < 2 {
		t.Fatalf("wait polled %d times; want it to keep polling past the first ERROR", polls)
	}
}

// The opposite case must keep working: a healthy load balancer whose delete
// genuinely fails, so Octavia moves it from PENDING_DELETE into ERROR, still
// fails fast with the status message rather than waiting out the timeout.
func TestWaitForLoadBalancerDeletedFailsOnAnErrorEnteredDuringDelete(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	var mu sync.Mutex
	polls := 0
	octavia := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.Method+" "+r.URL.Path != "GET /v2.0/lbaas/loadbalancers/lb-1" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotImplemented)
			return
		}
		polls++
		status := "PENDING_DELETE"
		if polls > 1 {
			status = "ERROR"
		}
		fmt.Fprintf(w, `{"loadbalancer": {"id": "lb-1", "provisioning_status": %q,
			"operating_status": "OFFLINE", "tags": []}}`, status)
	}))
	defer octavia.Close()

	client, err := fakeConfig(octavia.URL).LoadBalancerV2Client()
	if err != nil {
		t.Fatalf("building the client: %v", err)
	}
	err = waitForLoadBalancerDeleted(ctx, client, "lb-1", defaultLBTimeout)
	if err == nil {
		t.Fatal("wait succeeded; want the ERROR entered during the delete reported")
	}
	if !strings.Contains(err.Error(), "entered ERROR provisioning status during delete") {
		t.Fatalf("wait error = %q; want the delete-failure message", err)
	}
}

// buildDeleteState packs model into a tfsdk.State for the given schema, the
// same way a resource's Delete receives the prior state from Terraform.
func buildDeleteState[T any](t *testing.T, ctx context.Context, sch schema.Schema, model *T) tfsdk.State {
	t.Helper()
	state := tfsdk.State{Schema: sch, Raw: tftypes.NewValue(sch.Type().TerraformType(ctx), nil)}
	if d := state.Set(ctx, model); d.HasError() {
		t.Fatalf("building delete state: %v", d)
	}
	return state
}

// A listener's delete must still be attempted while its load balancer is in
// ERROR. waitForLoadBalancerActive used to fail this wait before the DELETE
// was ever issued (gophercloud's WaitFor runs its predicate once immediately,
// so an ERROR root meant the listener's own delete call never ran).
// waitForLoadBalancerSettled treats ERROR as settled, so the DELETE is
// issued -- but Octavia's own immutability check still refuses a child
// mutation against a root that is not ACTIVE, so the DELETE itself comes
// back 409 here. That 409 is real, verified Octavia behavior (see the
// package doc and waitForLoadBalancerSettled), not a gap in the fake, so it
// is reported as a delete error, alongside a warning naming the load
// balancer to repair.
func TestListenerDeleteIssuesDeleteAndReportsOctaviasConflictWhileRootIsError(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	var mu sync.Mutex
	deleteCalled := false
	octavia := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch r.Method + " " + r.URL.Path {
		case "GET /v2.0/lbaas/loadbalancers/lb-1":
			fmt.Fprint(w, `{"loadbalancer": {"id": "lb-1", "provisioning_status": "ERROR",
				"operating_status": "OFFLINE", "tags": []}}`)
		case "DELETE /v2.0/lbaas/listeners/listener-1":
			deleteCalled = true
			w.WriteHeader(http.StatusConflict)
			fmt.Fprint(w, `{"faultstring": "Load Balancer lb-1 is immutable and cannot be updated."}`)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotImplemented)
		}
	}))
	defer octavia.Close()

	r := &listenerResource{config: fakeConfig(octavia.URL)}
	var sch resource.SchemaResponse
	r.Schema(ctx, resource.SchemaRequest{}, &sch)

	state := buildDeleteState(t, ctx, sch.Schema, &listenerModel{
		ID:                     types.StringValue("listener-1"),
		LoadbalancerID:         types.StringValue("lb-1"),
		Protocol:               types.StringValue("HTTP"),
		ProtocolPort:           types.Int64Value(80),
		Name:                   types.StringNull(),
		Description:            types.StringNull(),
		DefaultPoolID:          types.StringNull(),
		ConnectionLimit:        types.Int64Null(),
		DefaultTLSContainerRef: types.StringNull(),
		SNIContainerRefs:       types.ListNull(types.StringType),
		AdminStateUp:           types.BoolValue(true),
		TimeoutClientData:      types.Int64Null(),
		TimeoutMemberConnect:   types.Int64Null(),
		TimeoutMemberData:      types.Int64Null(),
		TimeoutTCPInspect:      types.Int64Null(),
		InsertHeaders:          types.MapNull(types.StringType),
		AllowedCIDRs:           types.ListNull(types.StringType),
		Tags:                   types.SetNull(types.StringType),
		ProvisioningStatus:     types.StringNull(),
		OperatingStatus:        types.StringNull(),
		Region:                 types.StringNull(),
	})

	resp := resource.DeleteResponse{State: state}
	r.Delete(ctx, resource.DeleteRequest{State: state}, &resp)

	mu.Lock()
	called := deleteCalled
	mu.Unlock()
	if !called {
		t.Fatal("listener delete never issued DELETE /v2.0/lbaas/listeners/listener-1: a root in ERROR must not block the attempt")
	}
	if !resp.Diagnostics.HasError() {
		t.Fatal("delete succeeded; want Octavia's 409 reported")
	}
	errs := resp.Diagnostics.Errors()
	if len(errs) == 0 || !strings.Contains(errs[0].Detail(), "409") {
		t.Fatalf("delete errors = %v; want the 409 Octavia returned", errs)
	}
	warnings := resp.Diagnostics.Warnings()
	found := false
	for _, w := range warnings {
		if strings.Contains(w.Detail(), "lb-1") {
			found = true
		}
	}
	if !found {
		t.Fatalf("delete warnings = %v; want one naming load balancer lb-1", warnings)
	}
}

// A pool's delete must still wait for the root load balancer to leave a
// transient status before issuing its own delete: PENDING_UPDATE is not
// settled, and waitForLoadBalancerSettled must keep polling through it
// exactly as waitForLoadBalancerActive did, then proceed once the root
// reaches ACTIVE.
func TestPoolDeleteWaitsThroughPendingUpdateThenDeletesOnceActive(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	var mu sync.Mutex
	polls, deleteCalled := 0, false
	octavia := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch r.Method + " " + r.URL.Path {
		case "GET /v2.0/lbaas/loadbalancers/lb-2":
			polls++
			status := "PENDING_UPDATE"
			if polls > 2 {
				status = "ACTIVE"
			}
			fmt.Fprintf(w, `{"loadbalancer": {"id": "lb-2", "provisioning_status": %q,
				"operating_status": "ONLINE", "tags": []}}`, status)
		case "DELETE /v2.0/lbaas/pools/pool-1":
			deleteCalled = true
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotImplemented)
		}
	}))
	defer octavia.Close()

	r := &poolResource{config: fakeConfig(octavia.URL)}
	var sch resource.SchemaResponse
	r.Schema(ctx, resource.SchemaRequest{}, &sch)

	state := buildDeleteState(t, ctx, sch.Schema, &poolModel{
		ID:                 types.StringValue("pool-1"),
		Name:               types.StringNull(),
		Description:        types.StringNull(),
		Protocol:           types.StringValue("HTTP"),
		LBMethod:           types.StringValue("ROUND_ROBIN"),
		LoadbalancerID:     types.StringValue("lb-2"),
		ListenerID:         types.StringNull(),
		AdminStateUp:       types.BoolValue(true),
		Persistence:        types.ObjectNull(poolPersistenceAttrTypes),
		Tags:               types.SetNull(types.StringType),
		ProjectID:          types.StringNull(),
		MonitorID:          types.StringNull(),
		ProvisioningStatus: types.StringNull(),
		OperatingStatus:    types.StringNull(),
		Region:             types.StringNull(),
	})

	resp := resource.DeleteResponse{State: state}
	r.Delete(ctx, resource.DeleteRequest{State: state}, &resp)

	if resp.Diagnostics.HasError() {
		t.Fatalf("delete of a pool whose root settles at ACTIVE: %v", resp.Diagnostics)
	}
	mu.Lock()
	defer mu.Unlock()
	if !deleteCalled {
		t.Fatal("pool delete never issued DELETE /v2.0/lbaas/pools/pool-1")
	}
	if polls < 3 {
		t.Fatalf("load balancer polled %d times; want the wait to poll past PENDING_UPDATE and settle again after the delete", polls)
	}
}

// A member's delete resolves its pool to find the root load balancer. If the
// pool is already gone -- cascade-deleted along with the load balancer, for
// example -- the member went with it: the delete must return cleanly with no
// error and must never attempt a member DELETE naming a pool that no longer
// exists.
func TestMemberDeleteReturnsCleanlyWhenItsPoolIs404(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	octavia := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "GET /v2.0/lbaas/pools/pool-404":
			w.WriteHeader(http.StatusNotFound)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotImplemented)
		}
	}))
	defer octavia.Close()

	r := &memberResource{config: fakeConfig(octavia.URL)}
	var sch resource.SchemaResponse
	r.Schema(ctx, resource.SchemaRequest{}, &sch)

	state := buildDeleteState(t, ctx, sch.Schema, &memberModel{
		ID:                 types.StringValue("member-1"),
		PoolID:             types.StringValue("pool-404"),
		Address:            types.StringValue("10.0.0.9"),
		ProtocolPort:       types.Int64Value(80),
		Name:               types.StringNull(),
		Weight:             types.Int64Null(),
		SubnetID:           types.StringNull(),
		AdminStateUp:       types.BoolValue(true),
		Backup:             types.BoolValue(false),
		MonitorAddress:     types.StringNull(),
		MonitorPort:        types.Int64Null(),
		Tags:               types.SetNull(types.StringType),
		ProvisioningStatus: types.StringNull(),
		OperatingStatus:    types.StringNull(),
		Region:             types.StringNull(),
	})

	resp := resource.DeleteResponse{State: state}
	r.Delete(ctx, resource.DeleteRequest{State: state}, &resp)

	if resp.Diagnostics.HasError() {
		t.Fatalf("delete of a member whose pool is already gone: %v", resp.Diagnostics)
	}
}

// The monitor equivalent of the member case above: a 404 resolving the
// monitor's pool means the monitor went with it.
func TestMonitorDeleteReturnsCleanlyWhenItsPoolIs404(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	octavia := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "GET /v2.0/lbaas/pools/pool-404":
			w.WriteHeader(http.StatusNotFound)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotImplemented)
		}
	}))
	defer octavia.Close()

	r := &monitorResource{config: fakeConfig(octavia.URL)}
	var sch resource.SchemaResponse
	r.Schema(ctx, resource.SchemaRequest{}, &sch)

	state := buildDeleteState(t, ctx, sch.Schema, &monitorModel{
		ID:                 types.StringValue("monitor-1"),
		PoolID:             types.StringValue("pool-404"),
		Type:               types.StringValue("HTTP"),
		Delay:              types.Int64Value(5),
		Timeout:            types.Int64Value(3),
		MaxRetries:         types.Int64Value(3),
		MaxRetriesDown:     types.Int64Value(3),
		HTTPMethod:         types.StringNull(),
		URLPath:            types.StringNull(),
		ExpectedCodes:      types.StringNull(),
		Name:               types.StringNull(),
		AdminStateUp:       types.BoolValue(true),
		Tags:               types.SetNull(types.StringType),
		ProvisioningStatus: types.StringNull(),
		OperatingStatus:    types.StringNull(),
		Region:             types.StringNull(),
	})

	resp := resource.DeleteResponse{State: state}
	r.Delete(ctx, resource.DeleteRequest{State: state}, &resp)

	if resp.Diagnostics.HasError() {
		t.Fatalf("delete of a monitor whose pool is already gone: %v", resp.Diagnostics)
	}
}

// A pool that resolves its root load balancer through its listener (rather
// than a direct loadbalancer_id) must get the same treatment: if the
// listener is already gone, the pool went with it.
func TestPoolDeleteReturnsCleanlyWhenItsListenerIs404(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	octavia := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "GET /v2.0/lbaas/listeners/listener-404":
			w.WriteHeader(http.StatusNotFound)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotImplemented)
		}
	}))
	defer octavia.Close()

	r := &poolResource{config: fakeConfig(octavia.URL)}
	var sch resource.SchemaResponse
	r.Schema(ctx, resource.SchemaRequest{}, &sch)

	state := buildDeleteState(t, ctx, sch.Schema, &poolModel{
		ID:                 types.StringValue("pool-2"),
		Name:               types.StringNull(),
		Description:        types.StringNull(),
		Protocol:           types.StringValue("HTTP"),
		LBMethod:           types.StringValue("ROUND_ROBIN"),
		LoadbalancerID:     types.StringNull(),
		ListenerID:         types.StringValue("listener-404"),
		AdminStateUp:       types.BoolValue(true),
		Persistence:        types.ObjectNull(poolPersistenceAttrTypes),
		Tags:               types.SetNull(types.StringType),
		ProjectID:          types.StringNull(),
		MonitorID:          types.StringNull(),
		ProvisioningStatus: types.StringNull(),
		OperatingStatus:    types.StringNull(),
		Region:             types.StringNull(),
	})

	resp := resource.DeleteResponse{State: state}
	r.Delete(ctx, resource.DeleteRequest{State: state}, &resp)

	if resp.Diagnostics.HasError() {
		t.Fatalf("delete of a pool whose listener is already gone: %v", resp.Diagnostics)
	}
}

// This is the case the settle wrapper exists to fix. A monitor's own DELETE
// gets back a 204 from Octavia -- accepted, not done -- but the root load
// balancer has moved to ERROR by the time the post-delete wait runs
// (unrelated activity elsewhere in the tree, for instance). Octavia's own
// revert path marks a failed child ERROR and returns the load balancer to
// ACTIVE to unlock it, so a root in ERROR here means something else failed,
// not this delete. waitForLoadBalancerActive used to report it as a failed
// delete anyway; the settle wrapper must report no error.
func TestMonitorDeleteReportsNoErrorWhenRootGoesErrorAfterASuccessfulDelete(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	var mu sync.Mutex
	deleteCalled := false
	octavia := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch r.Method + " " + r.URL.Path {
		case "GET /v2.0/lbaas/pools/pool-5":
			fmt.Fprint(w, `{"pool": {"id": "pool-5", "loadbalancers": [{"id": "lb-5"}], "listeners": []}}`)
		case "GET /v2.0/lbaas/loadbalancers/lb-5":
			status := "ACTIVE"
			if deleteCalled {
				status = "ERROR"
			}
			fmt.Fprintf(w, `{"loadbalancer": {"id": "lb-5", "provisioning_status": %q,
				"operating_status": "ONLINE", "tags": []}}`, status)
		case "DELETE /v2.0/lbaas/healthmonitors/monitor-1":
			deleteCalled = true
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotImplemented)
		}
	}))
	defer octavia.Close()

	r := &monitorResource{config: fakeConfig(octavia.URL)}
	var sch resource.SchemaResponse
	r.Schema(ctx, resource.SchemaRequest{}, &sch)

	state := buildDeleteState(t, ctx, sch.Schema, &monitorModel{
		ID:                 types.StringValue("monitor-1"),
		PoolID:             types.StringValue("pool-5"),
		Type:               types.StringValue("HTTP"),
		Delay:              types.Int64Value(5),
		Timeout:            types.Int64Value(3),
		MaxRetries:         types.Int64Value(3),
		MaxRetriesDown:     types.Int64Value(3),
		HTTPMethod:         types.StringNull(),
		URLPath:            types.StringNull(),
		ExpectedCodes:      types.StringNull(),
		Name:               types.StringNull(),
		AdminStateUp:       types.BoolValue(true),
		Tags:               types.SetNull(types.StringType),
		ProvisioningStatus: types.StringNull(),
		OperatingStatus:    types.StringNull(),
		Region:             types.StringNull(),
	})

	resp := resource.DeleteResponse{State: state}
	r.Delete(ctx, resource.DeleteRequest{State: state}, &resp)

	mu.Lock()
	called := deleteCalled
	mu.Unlock()
	if !called {
		t.Fatal("monitor delete never issued DELETE /v2.0/lbaas/healthmonitors/monitor-1")
	}
	if resp.Diagnostics.HasError() {
		t.Fatalf("delete succeeded against Octavia but was reported as failed: %v", resp.Diagnostics)
	}
}

// The following two tests drive Create down the path
// TestLoadBalancerCreateKeepsALoadBalancerThatFailedToBuild doesn't reach: the
// status wait succeeds (ACTIVE), and the read-back that follows it is what
// fails. readInto returns without touching its model argument on both a 404
// and a non-404 error, so a Create that unconditionally sets state from the
// plan afterward would write back the plan's own unknown values on top of the
// clean, fully-known row RecordCreated already wrote -- a state Terraform
// refuses. The guard must instead: on a 404, report the error and remove the
// resource (nothing is left to keep); on any other error, report it and leave
// the recorded row in place (the load balancer still exists).

// A load balancer create whose status wait reaches ACTIVE, but whose
// read-back then finds the load balancer already gone, must report the error
// and drop it from state.
func TestLoadBalancerCreateDropsStateWhenReadBackAfterActive404s(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	var mu sync.Mutex
	gets := 0
	octavia := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch r.Method + " " + r.URL.Path {
		case "POST /v2.0/lbaas/loadbalancers":
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{"loadbalancer": {"id": "lb-1", "provisioning_status": "PENDING_CREATE"}}`)
		case "GET /v2.0/lbaas/loadbalancers/lb-1":
			gets++
			if gets == 1 {
				// Satisfies waitForLoadBalancerActive on the very first poll.
				fmt.Fprint(w, `{"loadbalancer": {"id": "lb-1", "name": "tf-lb-gone", "description": "",
					"admin_state_up": true, "vip_subnet_id": "subnet-1", "vip_network_id": "",
					"vip_address": "10.0.0.5", "vip_port_id": "port-1", "flavor_id": "",
					"provider": "ovn", "provisioning_status": "ACTIVE", "operating_status": "ONLINE",
					"tags": []}}`)
				return
			}
			// The read-back that immediately follows the wait finds it gone.
			w.WriteHeader(http.StatusNotFound)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotImplemented)
		}
	}))
	defer octavia.Close()

	r := &loadBalancerResource{config: fakeConfig(octavia.URL)}
	var sch resource.SchemaResponse
	r.Schema(ctx, resource.SchemaRequest{}, &sch)
	s := sch.Schema

	planned := loadBalancerModel{
		ID:                 types.StringUnknown(),
		Name:               types.StringValue("tf-lb-gone"),
		Description:        types.StringUnknown(),
		AdminStateUp:       types.BoolValue(true),
		VipSubnetID:        types.StringValue("subnet-1"),
		VipNetworkID:       types.StringUnknown(),
		VipAddress:         types.StringUnknown(),
		VipPortID:          types.StringUnknown(),
		FlavorID:           types.StringUnknown(),
		Provider:           types.StringValue("ovn"),
		Tags:               types.SetUnknown(types.StringType),
		ProvisioningStatus: types.StringUnknown(),
		OperatingStatus:    types.StringUnknown(),
		Region:             types.StringUnknown(),
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
		t.Fatal("create left a row in state for a load balancer a 404 read-back confirmed gone: " +
			"a genuine 404 right after ACTIVE means there is nothing left to keep")
	}
	mu.Lock()
	defer mu.Unlock()
	if gets < 2 {
		t.Fatalf("GET called %d times; want the wait's ACTIVE poll and the read-back poll", gets)
	}
}

// A load balancer create whose status wait reaches ACTIVE, but whose
// read-back then fails with a server error (not a 404), must report the
// error and leave the row RecordCreated wrote in place: the load balancer
// still exists, and dropping it would recreate the orphan this change
// removes.
func TestLoadBalancerCreateKeepsStateWhenReadBackAfterActiveFails(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	var mu sync.Mutex
	gets := 0
	octavia := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch r.Method + " " + r.URL.Path {
		case "POST /v2.0/lbaas/loadbalancers":
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{"loadbalancer": {"id": "lb-1", "provisioning_status": "PENDING_CREATE"}}`)
		case "GET /v2.0/lbaas/loadbalancers/lb-1":
			gets++
			if gets == 1 {
				// Satisfies waitForLoadBalancerActive on the very first poll.
				fmt.Fprint(w, `{"loadbalancer": {"id": "lb-1", "name": "tf-lb-flaky", "description": "",
					"admin_state_up": true, "vip_subnet_id": "subnet-1", "vip_network_id": "",
					"vip_address": "10.0.0.5", "vip_port_id": "port-1", "flavor_id": "",
					"provider": "ovn", "provisioning_status": "ACTIVE", "operating_status": "ONLINE",
					"tags": []}}`)
				return
			}
			// The read-back that immediately follows the wait hits a transient
			// server error; the load balancer itself is still there.
			w.WriteHeader(http.StatusInternalServerError)
			fmt.Fprint(w, `{"faultstring": "octavia temporarily unavailable"}`)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotImplemented)
		}
	}))
	defer octavia.Close()

	r := &loadBalancerResource{config: fakeConfig(octavia.URL)}
	var sch resource.SchemaResponse
	r.Schema(ctx, resource.SchemaRequest{}, &sch)
	s := sch.Schema

	planned := loadBalancerModel{
		ID:                 types.StringUnknown(),
		Name:               types.StringValue("tf-lb-flaky"),
		Description:        types.StringUnknown(),
		AdminStateUp:       types.BoolValue(true),
		VipSubnetID:        types.StringValue("subnet-1"),
		VipNetworkID:       types.StringUnknown(),
		VipAddress:         types.StringUnknown(),
		VipPortID:          types.StringUnknown(),
		FlavorID:           types.StringUnknown(),
		Provider:           types.StringValue("ovn"),
		Tags:               types.SetUnknown(types.StringType),
		ProvisioningStatus: types.StringUnknown(),
		OperatingStatus:    types.StringUnknown(),
		Region:             types.StringUnknown(),
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
		t.Fatal("create dropped lb-1 from state even though the load balancer still exists behind the failed read-back")
	}
	if !createResp.State.Raw.IsFullyKnown() {
		t.Fatalf("create state holds unknown values, which Terraform refuses: %v", createResp.State.Raw)
	}
	var got loadBalancerModel
	if d := createResp.State.Get(ctx, &got); d.HasError() {
		t.Fatalf("reading the create state: %v", d)
	}
	if got.ID.ValueString() != "lb-1" {
		t.Fatalf("create state id = %s, want lb-1: a row without one names nothing a destroy could delete", got.ID)
	}
	mu.Lock()
	defer mu.Unlock()
	if gets < 2 {
		t.Fatalf("GET called %d times; want the wait's ACTIVE poll and the failed read-back poll", gets)
	}
}
