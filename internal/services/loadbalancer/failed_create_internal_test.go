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
