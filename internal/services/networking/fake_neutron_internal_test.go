// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package networking

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-go/tftypes"

	"github.com/platform9/terraform-provider-pcd/internal/clients"
)

// This file holds the offline Neutron fake and the framework plumbing that the
// networking resources' failed-create tests share. A test describes Neutron as
// a set of routes, replaces the one whose failure it exercises, and drives the
// resource's own Create, Read, Update and Delete against it.

// fakeConfig points a clients.Config at a test server, so the Neutron client the
// resources build resolves to it. The locator ignores the endpoint options, so
// it answers for every service type and availability. Every request path carries
// a /v2.0/ prefix: openstack.NewNetworkV2 sets the client's ResourceBase to the
// endpoint plus "v2.0/". EndpointOverrides must stay empty, since an override
// blanks ResourceBase and drops the prefix.
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

// neutronRoutes maps a request, written "METHOD /path" (for example
// "GET /v2.0/security-groups/sg-1"), to the handler that answers it.
type neutronRoutes map[string]http.HandlerFunc

// fakeNeutron is a test server that answers from its routes and logs every
// request it receives, so a test can assert what a resource sent, including
// that it sent nothing. It serves one request at a time, so a route may keep a
// counter without a lock of its own. A request no route names fails the test.
type fakeNeutron struct {
	// config builds Neutron clients that reach this server.
	config *clients.Config

	mu       sync.Mutex
	requests []string
}

// newFakeNeutron starts a fakeNeutron serving routes. The server closes when
// the test ends.
func newFakeNeutron(t *testing.T, routes neutronRoutes) *fakeNeutron {
	t.Helper()
	f := &fakeNeutron{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		call := r.Method + " " + r.URL.Path
		f.requests = append(f.requests, call)
		if handle, ok := routes[call]; ok {
			handle(w, r)
			return
		}
		// 501 is no status Neutron sends for any call; it only stops the
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
func (f *fakeNeutron) received() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.requests...)
}

// reply answers with status and, unless body is empty, a JSON body. A route
// must answer with a status gophercloud accepts for that call when it models
// success: Neutron answers a create with 201, a get, an update or a tags PUT
// with 200, and a delete with 204.
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
// Neutron can. The body is not JSON.
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
