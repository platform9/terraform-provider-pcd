// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package loadbalancer

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"

	"github.com/platform9/terraform-provider-pcd/internal/clients"
	"github.com/platform9/terraform-provider-pcd/internal/tfstate"
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
// listener lookup 404s, the parent chain is gone and there is nothing left
// to resolve, so the delete must return cleanly. This is not because
// deleting a listener deletes its pool -- Octavia's get_delete_listener_flow
// does not; pools are shareable, and only the load balancer's own cascade
// delete removes them.
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

// The tests from here on drive the four children of a load balancer
// (listener, pool, member and health monitor) through a failed step after
// their create, and through the rows provider v0.1.14 left in state when such
// a step failed. They share an offline Octavia: a test describes it as a set of
// routes, replaces the one whose failure it exercises, and drives the
// resource's own Create, Read, Update and Delete against it.

// octaviaRoutes maps a request, written "METHOD /path" (for example
// "GET /v2.0/lbaas/listeners/listener-1"), to the handler that answers it.
type octaviaRoutes map[string]http.HandlerFunc

// fakeOctavia is a test server that answers from its routes and logs every
// request it receives, so a test can assert what a resource sent, including
// that it sent nothing. It serves one request at a time, so a route may keep a
// counter without a lock of its own. A request no route names fails the test.
type fakeOctavia struct {
	// config builds Octavia clients that reach this server.
	config *clients.Config

	mu       sync.Mutex
	requests []string
}

// newFakeOctavia starts a fakeOctavia serving routes. The server closes when
// the test ends.
func newFakeOctavia(t *testing.T, routes octaviaRoutes) *fakeOctavia {
	t.Helper()
	f := &fakeOctavia{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		call := r.Method + " " + r.URL.Path
		f.requests = append(f.requests, call)
		if handle, ok := routes[call]; ok {
			handle(w, r)
			return
		}
		// The status only stops the resource from waiting on a call the
		// test did not expect; t.Errorf has already failed the test.
		t.Errorf("unexpected request %s", call)
		w.WriteHeader(http.StatusNotImplemented)
	}))
	t.Cleanup(srv.Close)
	f.config = fakeConfig(srv.URL)
	return f
}

// received returns the requests served so far, in order, each written
// "METHOD /path".
func (f *fakeOctavia) received() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.requests...)
}

// reply answers with status and, unless body is empty, a JSON body. A route
// must answer with a status gophercloud accepts for that call when it models
// success: Octavia answers a create with 201, a get or an update with 200, and
// a delete with 204.
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
// Octavia can. The body is not JSON.
func badGateway(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html")
	w.WriteHeader(http.StatusBadGateway)
	fmt.Fprint(w, nginxBadGatewayPage)
}

// badGatewayFirst answers the first request with badGateway and every later
// one with then: a call that fails once and works when it is retried, for
// example by the refresh that follows a failed create.
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

// rowWithoutID is the row provider v0.1.14 and earlier left in state when the
// read-back after a successful create failed: Create returned plan unchanged,
// and Terraform saved its unknowns as null, the ID among them.
func rowWithoutID(t *testing.T, plan tfsdk.Plan) tfsdk.State {
	t.Helper()
	state := tfsdk.State{Schema: plan.Schema, Raw: plan.Raw.Copy()}
	if d := tfstate.NullUnknowns(&state); d.HasError() {
		t.Fatalf("building the row: %v", d)
	}
	return state
}

// stringAttr returns the string attribute name of state.
func stringAttr(t *testing.T, state tfsdk.State, name string) types.String {
	t.Helper()
	var v types.String
	if d := state.GetAttribute(context.Background(), path.Root(name), &v); d.HasError() {
		t.Fatalf("reading %s from the state: %v", name, d)
	}
	return v
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

// runDelete calls Delete with a response state that starts as the prior state.
func runDelete(r resource.Resource, prior tfsdk.State) resource.DeleteResponse {
	resp := resource.DeleteResponse{State: prior}
	r.Delete(context.Background(), resource.DeleteRequest{State: prior}, &resp)
	return resp
}

const (
	lbsPath       = "/v2.0/lbaas/loadbalancers"
	lbPath        = lbsPath + "/lb-1"
	listenersPath = "/v2.0/lbaas/listeners"
	listenerPath  = listenersPath + "/listener-1"
	poolsPath     = "/v2.0/lbaas/pools"
	poolPath      = poolsPath + "/pool-1"
	membersPath   = poolPath + "/members"
	memberPath    = membersPath + "/member-1"
	monitorsPath  = "/v2.0/lbaas/healthmonitors"
	monitorPath   = monitorsPath + "/monitor-1"
)

// lbJSON is lb-1, the load balancer the children belong to, as Octavia returns
// it with the given provisioning status.
func lbJSON(status string) string {
	return fmt.Sprintf(`{"id": "lb-1", "name": "web-lb", "description": "",
		"admin_state_up": true, "vip_subnet_id": "subnet-1", "vip_network_id": "net-1",
		"vip_address": "10.0.0.5", "vip_port_id": "port-1", "flavor_id": "", "provider": "ovn",
		"provisioning_status": %q, "operating_status": "ONLINE", "tags": []}`, status)
}

// The children as Octavia returns them once they are ACTIVE. PCD's ovn
// provider is Layer 4, so they use TCP.
const (
	listenerJSON = `{"id": "listener-1", "name": "web-listener", "description": "",
		"protocol": "TCP", "protocol_port": 80, "default_pool_id": null,
		"loadbalancers": [{"id": "lb-1"}], "connection_limit": -1, "admin_state_up": true,
		"timeout_client_data": 50000, "timeout_member_connect": 5000,
		"timeout_member_data": 50000, "timeout_tcp_inspect": 0, "project_id": "proj-1",
		"provisioning_status": "ACTIVE", "operating_status": "ONLINE", "tags": []}`
	poolJSON = `{"id": "pool-1", "name": "web-pool", "description": "", "protocol": "TCP",
		"lb_algorithm": "SOURCE_IP_PORT", "loadbalancers": [{"id": "lb-1"}], "listeners": [],
		"admin_state_up": true, "session_persistence": null, "healthmonitor_id": null,
		"members": [], "project_id": "proj-1", "provisioning_status": "ACTIVE",
		"operating_status": "ONLINE", "tags": []}`
	memberJSON = `{"id": "member-1", "name": "web-1", "address": "10.0.0.10",
		"protocol_port": 8080, "weight": 1, "subnet_id": "subnet-1", "admin_state_up": true,
		"backup": false, "monitor_address": null, "monitor_port": null, "project_id": "proj-1",
		"provisioning_status": "ACTIVE", "operating_status": "NO_MONITOR", "tags": []}`
	monitorJSON = `{"id": "monitor-1", "name": "web-monitor", "type": "TCP", "delay": 5,
		"timeout": 3, "max_retries": 3, "max_retries_down": 3, "http_method": null,
		"url_path": null, "expected_codes": null, "admin_state_up": true,
		"pools": [{"id": "pool-1"}], "project_id": "proj-1", "provisioning_status": "ACTIVE",
		"operating_status": "ONLINE", "tags": []}`
)

// lbChild describes one child of a load balancer for the scenarios below,
// which run the same steps against each.
type lbChild struct {
	name     string // the subtest name
	resource func(*clients.Config) resource.Resource
	id       string
	// path is the child's own URL. A request for an empty ID reaches
	// collection instead, which Octavia answers with list.
	path, collection, list string
	// routes answers every call the child's Create, Read and Update make, the
	// way Octavia does.
	routes func() octaviaRoutes
	// planned is the plan for a new child, known the child as a refresh leaves
	// it, and renamed the plan for an update that renames it. Each points to
	// the child's model.
	planned, known, renamed any
}

func lbChildren() []lbChild {
	emptyTags := types.SetValueMust(types.StringType, []attr.Value{})

	knownListener := listenerModel{
		ID:                     types.StringValue("listener-1"),
		LoadbalancerID:         types.StringValue("lb-1"),
		Protocol:               types.StringValue("TCP"),
		ProtocolPort:           types.Int64Value(80),
		Name:                   types.StringValue("web-listener"),
		Description:            types.StringValue(""),
		DefaultPoolID:          types.StringValue(""),
		ConnectionLimit:        types.Int64Value(-1),
		DefaultTLSContainerRef: types.StringNull(),
		SNIContainerRefs:       types.ListNull(types.StringType),
		AdminStateUp:           types.BoolValue(true),
		TimeoutClientData:      types.Int64Value(50000),
		TimeoutMemberConnect:   types.Int64Value(5000),
		TimeoutMemberData:      types.Int64Value(50000),
		TimeoutTCPInspect:      types.Int64Value(0),
		InsertHeaders:          types.MapNull(types.StringType),
		AllowedCIDRs:           types.ListNull(types.StringType),
		Tags:                   emptyTags,
		ProvisioningStatus:     types.StringValue("ACTIVE"),
		OperatingStatus:        types.StringValue("ONLINE"),
		Region:                 types.StringValue("region-one"),
	}
	// The config sets loadbalancer_id, protocol, protocol_port and name.
	plannedListener := knownListener
	plannedListener.ID = types.StringUnknown()
	plannedListener.Description = types.StringUnknown()
	plannedListener.DefaultPoolID = types.StringUnknown()
	plannedListener.ConnectionLimit = types.Int64Unknown()
	plannedListener.TimeoutClientData = types.Int64Unknown()
	plannedListener.TimeoutMemberConnect = types.Int64Unknown()
	plannedListener.TimeoutMemberData = types.Int64Unknown()
	plannedListener.TimeoutTCPInspect = types.Int64Unknown()
	plannedListener.Tags = types.SetUnknown(types.StringType)
	plannedListener.ProvisioningStatus = types.StringUnknown()
	plannedListener.OperatingStatus = types.StringUnknown()
	plannedListener.Region = types.StringUnknown()
	renamedListener := knownListener
	renamedListener.Name = types.StringValue("web-listener-renamed")

	knownPool := poolModel{
		ID:                 types.StringValue("pool-1"),
		Name:               types.StringValue("web-pool"),
		Description:        types.StringValue(""),
		Protocol:           types.StringValue("TCP"),
		LBMethod:           types.StringValue("SOURCE_IP_PORT"),
		LoadbalancerID:     types.StringValue("lb-1"),
		ListenerID:         types.StringValue(""),
		AdminStateUp:       types.BoolValue(true),
		Persistence:        types.ObjectNull(poolPersistenceAttrTypes),
		Tags:               emptyTags,
		ProjectID:          types.StringValue("proj-1"),
		MonitorID:          types.StringValue(""),
		ProvisioningStatus: types.StringValue("ACTIVE"),
		OperatingStatus:    types.StringValue("ONLINE"),
		Region:             types.StringValue("region-one"),
	}
	// The config sets name, protocol, lb_method and loadbalancer_id.
	plannedPool := knownPool
	plannedPool.ID = types.StringUnknown()
	plannedPool.Description = types.StringUnknown()
	plannedPool.ListenerID = types.StringUnknown()
	plannedPool.Tags = types.SetUnknown(types.StringType)
	plannedPool.ProjectID = types.StringUnknown()
	plannedPool.MonitorID = types.StringUnknown()
	plannedPool.ProvisioningStatus = types.StringUnknown()
	plannedPool.OperatingStatus = types.StringUnknown()
	plannedPool.Region = types.StringUnknown()
	renamedPool := knownPool
	renamedPool.Name = types.StringValue("web-pool-renamed")

	knownMember := memberModel{
		ID:                 types.StringValue("member-1"),
		PoolID:             types.StringValue("pool-1"),
		Address:            types.StringValue("10.0.0.10"),
		ProtocolPort:       types.Int64Value(8080),
		Name:               types.StringValue("web-1"),
		Weight:             types.Int64Value(1),
		SubnetID:           types.StringValue("subnet-1"),
		AdminStateUp:       types.BoolValue(true),
		Backup:             types.BoolValue(false),
		MonitorAddress:     types.StringValue(""),
		MonitorPort:        types.Int64Value(0),
		Tags:               emptyTags,
		ProvisioningStatus: types.StringValue("ACTIVE"),
		OperatingStatus:    types.StringValue("NO_MONITOR"),
		Region:             types.StringValue("region-one"),
	}
	// The config sets pool_id, address, protocol_port and name.
	plannedMember := knownMember
	plannedMember.ID = types.StringUnknown()
	plannedMember.Weight = types.Int64Unknown()
	plannedMember.SubnetID = types.StringUnknown()
	plannedMember.Backup = types.BoolUnknown()
	plannedMember.MonitorAddress = types.StringUnknown()
	plannedMember.MonitorPort = types.Int64Unknown()
	plannedMember.Tags = types.SetUnknown(types.StringType)
	plannedMember.ProvisioningStatus = types.StringUnknown()
	plannedMember.OperatingStatus = types.StringUnknown()
	plannedMember.Region = types.StringUnknown()
	// backup has no plan modifier, so a config that leaves it unset plans it
	// unknown in every update.
	renamedMember := knownMember
	renamedMember.Name = types.StringValue("web-1-renamed")
	renamedMember.Backup = types.BoolUnknown()

	knownMonitor := monitorModel{
		ID:                 types.StringValue("monitor-1"),
		PoolID:             types.StringValue("pool-1"),
		Type:               types.StringValue("TCP"),
		Delay:              types.Int64Value(5),
		Timeout:            types.Int64Value(3),
		MaxRetries:         types.Int64Value(3),
		MaxRetriesDown:     types.Int64Value(3),
		HTTPMethod:         types.StringValue(""),
		URLPath:            types.StringValue(""),
		ExpectedCodes:      types.StringValue(""),
		Name:               types.StringValue("web-monitor"),
		AdminStateUp:       types.BoolValue(true),
		Tags:               emptyTags,
		ProvisioningStatus: types.StringValue("ACTIVE"),
		OperatingStatus:    types.StringValue("ONLINE"),
		Region:             types.StringValue("region-one"),
	}
	// The config sets pool_id, type, delay, timeout, max_retries and name.
	plannedMonitor := knownMonitor
	plannedMonitor.ID = types.StringUnknown()
	plannedMonitor.MaxRetriesDown = types.Int64Unknown()
	plannedMonitor.HTTPMethod = types.StringUnknown()
	plannedMonitor.URLPath = types.StringUnknown()
	plannedMonitor.ExpectedCodes = types.StringUnknown()
	plannedMonitor.Tags = types.SetUnknown(types.StringType)
	plannedMonitor.ProvisioningStatus = types.StringUnknown()
	plannedMonitor.OperatingStatus = types.StringUnknown()
	plannedMonitor.Region = types.StringUnknown()
	renamedMonitor := knownMonitor
	renamedMonitor.Name = types.StringValue("web-monitor-renamed")

	activeLB := reply(http.StatusOK, `{"loadbalancer": `+lbJSON(lbActive)+`}`)
	return []lbChild{
		{
			name:       "listener",
			resource:   func(c *clients.Config) resource.Resource { return &listenerResource{config: c} },
			id:         "listener-1",
			path:       listenerPath,
			collection: listenersPath + "/",
			list:       `{"listeners": [` + listenerJSON + `], "listeners_links": []}`,
			routes: func() octaviaRoutes {
				return octaviaRoutes{
					"GET " + lbPath:         activeLB,
					"POST " + listenersPath: reply(http.StatusCreated, `{"listener": {"id": "listener-1", "provisioning_status": "PENDING_CREATE"}}`),
					"GET " + listenerPath:   reply(http.StatusOK, `{"listener": `+listenerJSON+`}`),
					"PUT " + listenerPath:   reply(http.StatusOK, `{"listener": {"id": "listener-1", "provisioning_status": "PENDING_UPDATE"}}`),
				}
			},
			planned: &plannedListener, known: &knownListener, renamed: &renamedListener,
		},
		{
			name:       "pool",
			resource:   func(c *clients.Config) resource.Resource { return &poolResource{config: c} },
			id:         "pool-1",
			path:       poolPath,
			collection: poolsPath + "/",
			list:       `{"pools": [` + poolJSON + `], "pools_links": []}`,
			routes: func() octaviaRoutes {
				return octaviaRoutes{
					"GET " + lbPath:     activeLB,
					"POST " + poolsPath: reply(http.StatusCreated, `{"pool": {"id": "pool-1", "provisioning_status": "PENDING_CREATE"}}`),
					"GET " + poolPath:   reply(http.StatusOK, `{"pool": `+poolJSON+`}`),
					"PUT " + poolPath:   reply(http.StatusOK, `{"pool": {"id": "pool-1", "provisioning_status": "PENDING_UPDATE"}}`),
				}
			},
			planned: &plannedPool, known: &knownPool, renamed: &renamedPool,
		},
		{
			name:       "member",
			resource:   func(c *clients.Config) resource.Resource { return &memberResource{config: c} },
			id:         "member-1",
			path:       memberPath,
			collection: membersPath + "/",
			list:       `{"members": [` + memberJSON + `], "members_links": []}`,
			routes: func() octaviaRoutes {
				return octaviaRoutes{
					"GET " + poolPath:     reply(http.StatusOK, `{"pool": `+poolJSON+`}`),
					"GET " + lbPath:       activeLB,
					"POST " + membersPath: reply(http.StatusCreated, `{"member": {"id": "member-1", "provisioning_status": "PENDING_CREATE"}}`),
					"GET " + memberPath:   reply(http.StatusOK, `{"member": `+memberJSON+`}`),
					"PUT " + memberPath:   reply(http.StatusOK, `{"member": {"id": "member-1", "provisioning_status": "PENDING_UPDATE"}}`),
				}
			},
			planned: &plannedMember, known: &knownMember, renamed: &renamedMember,
		},
		{
			name:       "monitor",
			resource:   func(c *clients.Config) resource.Resource { return &monitorResource{config: c} },
			id:         "monitor-1",
			path:       monitorPath,
			collection: monitorsPath + "/",
			list:       `{"healthmonitors": [` + monitorJSON + `], "healthmonitors_links": []}`,
			routes: func() octaviaRoutes {
				return octaviaRoutes{
					"GET " + poolPath:      reply(http.StatusOK, `{"pool": `+poolJSON+`}`),
					"GET " + lbPath:        activeLB,
					"POST " + monitorsPath: reply(http.StatusCreated, `{"healthmonitor": {"id": "monitor-1", "provisioning_status": "PENDING_CREATE"}}`),
					"GET " + monitorPath:   reply(http.StatusOK, `{"healthmonitor": `+monitorJSON+`}`),
					"PUT " + monitorPath:   reply(http.StatusOK, `{"healthmonitor": {"id": "monitor-1", "provisioning_status": "PENDING_UPDATE"}}`),
				}
			},
			planned: &plannedMonitor, known: &knownMonitor, renamed: &renamedMonitor,
		},
	}
}

// checkRefreshed fails the test unless state holds the child as Octavia
// reports it once it is ACTIVE.
func checkRefreshed(t *testing.T, c lbChild, state tfsdk.State) {
	t.Helper()
	if state.Raw.IsNull() {
		t.Fatalf("state holds no %s", c.name)
	}
	if !state.Raw.IsFullyKnown() {
		t.Fatalf("state holds unknown values, which Terraform refuses: %v", state.Raw)
	}
	id, status, region := stringAttr(t, state, "id"), stringAttr(t, state, "provisioning_status"), stringAttr(t, state, "region")
	if id.ValueString() != c.id || status.ValueString() != lbActive || region.ValueString() != "region-one" {
		t.Fatalf("state id=%s provisioning_status=%s region=%s; want %s, ACTIVE, region-one", id, status, region, c.id)
	}
}

// A create whose every step succeeds must return the child fully known.
func TestLBChildCreateFillsEveryAttribute(t *testing.T) {
	t.Parallel()
	for _, c := range lbChildren() {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			r := c.resource(newFakeOctavia(t, c.routes()).config)

			resp := runCreate(r, newPlan(t, schemaOf(t, r), c.planned))
			if resp.Diagnostics.HasError() {
				t.Fatalf("create: %v", resp.Diagnostics)
			}
			checkRefreshed(t, c, resp.State)
		})
	}
}

// Octavia keeps a child whose read-back fails once its load balancer is ACTIVE
// again. Create used to write the plan's unknowns, which Terraform saved as
// null, ID included, so the child was orphaned and the next refresh read the
// collection URL and crashed. Create must return the error with the child's ID
// in state (Terraform then taints it), and a refresh must fill in what the
// read-back would have.
func TestLBChildCreateKeepsStateWhenReadBackFails(t *testing.T) {
	t.Parallel()
	for _, c := range lbChildren() {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			routes := c.routes()
			routes["GET "+c.path] = badGatewayFirst(routes["GET "+c.path])
			r := c.resource(newFakeOctavia(t, routes).config)

			createResp := runCreate(r, newPlan(t, schemaOf(t, r), c.planned))
			if !createResp.Diagnostics.HasError() {
				t.Fatal("create succeeded; want the 502 on the read-back reported")
			}
			if createResp.State.Raw.IsNull() {
				t.Fatalf("create returned no state: Terraform forgets %s and the next apply creates another", c.id)
			}
			if !createResp.State.Raw.IsFullyKnown() {
				t.Fatalf("create state holds unknown values, which Terraform saves as null: %v", createResp.State.Raw)
			}
			if id := stringAttr(t, createResp.State, "id"); id.ValueString() != c.id {
				t.Fatalf("create state id = %s, want %s: a row without one names nothing a destroy could delete", id, c.id)
			}

			// The replacing apply, or a destroy, refreshes the tainted child first.
			readResp := runRead(r, createResp.State)
			if readResp.Diagnostics.HasError() {
				t.Fatalf("refresh of the recorded %s: %v", c.name, readResp.Diagnostics)
			}
			checkRefreshed(t, c, readResp.State)
		})
	}
}

// The known gap, kept on purpose: when the wait for the load balancer fails
// after the child's POST, Create still returns no state. Octavia refuses a
// child's DELETE while its load balancer is in ERROR, so a child recorded
// before a wait that failed on ERROR would be tainted and impossible to
// destroy. Create records the child only once that wait has succeeded.
func TestLBChildCreateLeavesNoStateWhenTheParentWaitFails(t *testing.T) {
	t.Parallel()
	for _, c := range lbChildren() {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			routes := c.routes()
			polls := 0
			routes["GET "+lbPath] = func(w http.ResponseWriter, r *http.Request) {
				polls++
				status := lbActive
				if polls > 1 {
					// lb-1 went to ERROR while it built the child.
					status = lbError
				}
				reply(http.StatusOK, `{"loadbalancer": `+lbJSON(status)+`}`)(w, r)
			}
			octavia := newFakeOctavia(t, routes)
			r := c.resource(octavia.config)

			resp := runCreate(r, newPlan(t, schemaOf(t, r), c.planned))
			if !resp.Diagnostics.HasError() {
				t.Fatal("create succeeded; want the load balancer's ERROR reported")
			}
			if !resp.State.Raw.IsNull() {
				t.Fatalf("create recorded a %s whose load balancer is in ERROR, which no destroy could delete: %v",
					c.name, resp.State.Raw)
			}
			sent := octavia.received()
			if !slices.ContainsFunc(sent, func(call string) bool { return strings.HasPrefix(call, "POST ") }) {
				t.Fatalf("create sent %v; want the %s's POST before the failed wait", sent, c.name)
			}
			if slices.Contains(sent, "GET "+c.path) {
				t.Fatalf("create sent %v; want no read-back after the failed wait", sent)
			}
		})
	}
}

// A read-back that finds the child gone once its load balancer is ACTIVE again
// used to be swallowed: Create reported nothing and Terraform saved a row with
// no ID. Create must report it and leave nothing in state.
func TestLBChildCreateDropsStateWhenReadBack404s(t *testing.T) {
	t.Parallel()
	for _, c := range lbChildren() {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			routes := c.routes()
			routes["GET "+c.path] = reply(http.StatusNotFound, "")
			r := c.resource(newFakeOctavia(t, routes).config)

			resp := runCreate(r, newPlan(t, schemaOf(t, r), c.planned))
			if !resp.Diagnostics.HasError() {
				t.Fatal("create succeeded; want the 404 read-back reported")
			}
			if !resp.State.Raw.IsNull() {
				t.Fatalf("create left a row for a %s the read-back found gone: %v", c.name, resp.State.Raw)
			}
		})
	}
}

// The row v0.1.14 left has no ID. Read used to send GET for an empty ID, which
// reaches the collection URL; Octavia answers that with the list, and readInto
// dereferenced the nil child gophercloud decoded from it. Read must drop the
// row with a warning that names the child, without sending anything.
func TestLBChildReadDropsARowWithNoID(t *testing.T) {
	t.Parallel()
	for _, c := range lbChildren() {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			routes := c.routes()
			routes["GET "+c.collection] = reply(http.StatusOK, c.list)
			octavia := newFakeOctavia(t, routes)
			r := c.resource(octavia.config)
			row := rowWithoutID(t, newPlan(t, schemaOf(t, r), c.planned))

			resp := runRead(r, row)
			if sent := octavia.received(); len(sent) != 0 {
				t.Fatalf("refresh of a row with no ID sent %v; it names nothing to read", sent)
			}
			if resp.Diagnostics.HasError() || resp.Diagnostics.WarningsCount() != 1 {
				t.Fatalf("diagnostics = %v, want exactly one warning", resp.Diagnostics)
			}
			name := stringAttr(t, row, "name").ValueString()
			if detail := resp.Diagnostics[0].Detail(); !strings.Contains(detail, fmt.Sprintf("%q", name)) {
				t.Fatalf("warning detail %q does not name the %s to look for", detail, c.name)
			}
			if !resp.State.Raw.IsNull() {
				t.Fatalf("refresh kept a row with no ID: %v", resp.State.Raw)
			}
		})
	}
}

// A 200 for the child's own URL whose body holds no child object makes
// gophercloud return no child and no error. Read must report that as an error
// and keep the row, and not take it as not-found: that would drop a child that
// exists from state.
func TestLBChildReadRefusesAnAnswerWithoutTheChild(t *testing.T) {
	t.Parallel()
	for _, c := range lbChildren() {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			routes := c.routes()
			routes["GET "+c.path] = reply(http.StatusOK, c.list)
			r := c.resource(newFakeOctavia(t, routes).config)
			row := newState(t, schemaOf(t, r), c.known)

			resp := runRead(r, row)
			if !resp.Diagnostics.HasError() {
				t.Fatalf("refresh accepted an answer without the %s: diagnostics %v", c.name, resp.Diagnostics)
			}
			if !resp.State.Raw.Equal(row.Raw) {
				t.Fatalf("refresh state = %v, want the row kept: %v", resp.State.Raw, row.Raw)
			}
		})
	}
}

// An update whose read-back fails must return the error and keep the prior
// state, which the next plan compares against. Update used to write the plan,
// which for a member holds an unknown backup that Terraform refuses.
func TestLBChildUpdateKeepsStateWhenReadBackFails(t *testing.T) {
	t.Parallel()
	for _, c := range lbChildren() {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			routes := c.routes()
			routes["GET "+c.path] = badGateway
			r := c.resource(newFakeOctavia(t, routes).config)
			s := schemaOf(t, r)
			prior := newState(t, s, c.known)

			resp := runUpdate(r, newPlan(t, s, c.renamed), prior)
			if !resp.Diagnostics.HasError() {
				t.Fatal("update succeeded; want the 502 on the read-back reported")
			}
			if !resp.State.Raw.Equal(prior.Raw) {
				t.Fatalf("update state = %v, want the prior state %v", resp.State.Raw, prior.Raw)
			}
		})
	}
}

// An update whose read-back finds the child gone used to be swallowed, and the
// plan was written as if the read-back had succeeded. Update must report it and
// keep the prior state; the next refresh drops the row.
func TestLBChildUpdateKeepsStateWhenReadBack404s(t *testing.T) {
	t.Parallel()
	for _, c := range lbChildren() {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			routes := c.routes()
			routes["GET "+c.path] = reply(http.StatusNotFound, "")
			r := c.resource(newFakeOctavia(t, routes).config)
			s := schemaOf(t, r)
			prior := newState(t, s, c.known)

			resp := runUpdate(r, newPlan(t, s, c.renamed), prior)
			if !resp.Diagnostics.HasError() {
				t.Fatal("update succeeded; want the 404 read-back reported")
			}
			if !resp.State.Raw.Equal(prior.Raw) {
				t.Fatalf("update state = %v, want the prior state %v", resp.State.Raw, prior.Raw)
			}
		})
	}
}

// terraform destroy -refresh=false reaches Delete without Read dropping the
// v0.1.14 row first. Delete used to wait on the load balancer and then send
// DELETE for an empty ID, which reaches the collection URL. Delete must warn
// and send nothing, so Terraform forgets the row.
func TestLBChildDeleteSkipsARowWithNoID(t *testing.T) {
	t.Parallel()
	for _, c := range lbChildren() {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			octavia := newFakeOctavia(t, c.routes())
			r := c.resource(octavia.config)

			resp := runDelete(r, rowWithoutID(t, newPlan(t, schemaOf(t, r), c.planned)))
			if sent := octavia.received(); len(sent) != 0 {
				t.Fatalf("delete of a row with no ID sent %v; it names nothing to delete", sent)
			}
			if resp.Diagnostics.HasError() || resp.Diagnostics.WarningsCount() != 1 {
				t.Fatalf("diagnostics = %v, want exactly one warning", resp.Diagnostics)
			}
		})
	}
}
