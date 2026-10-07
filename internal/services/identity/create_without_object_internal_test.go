// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package identity

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"

	"github.com/platform9/terraform-provider-pcd/internal/clients"
)

// newFakeKeystone starts a test server that answers from routes, each keyed
// "METHOD /path", and returns a clients.Config whose Keystone client reaches
// it. NewIdentityV3 appends v3/ to the root the locator returns, so every path
// carries a /v3/ prefix. A request no route names fails the test. The client
// gets a transport of its own, so closing this server cannot cut a request
// another parallel test is sending.
func newFakeKeystone(t *testing.T, routes map[string]http.HandlerFunc) *clients.Config {
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
	return &clients.Config{
		Region: "region-one",
		Provider: &gophercloud.ProviderClient{
			HTTPClient:      http.Client{Transport: &http.Transport{}},
			EndpointLocator: func(gophercloud.EndpointOpts) (string, error) { return srv.URL + "/", nil },
		},
	}
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

// runCreate calls Create the way the framework does, with a response state
// that starts as a null object.
func runCreate(r resource.Resource, model any) (resource.CreateResponse, error) {
	ctx := context.Background()
	var schemaResp resource.SchemaResponse
	r.Schema(ctx, resource.SchemaRequest{}, &schemaResp)
	s := schemaResp.Schema
	plan := tfsdk.Plan{Schema: s, Raw: tftypes.NewValue(s.Type().TerraformType(ctx), nil)}
	if d := plan.Set(ctx, model); d.HasError() {
		return resource.CreateResponse{}, fmt.Errorf("building the plan: %v", d)
	}
	resp := resource.CreateResponse{State: tfsdk.State{Schema: s, Raw: tftypes.NewValue(s.Type().TerraformType(ctx), nil)}}
	r.Create(ctx, resource.CreateRequest{Plan: plan}, &resp)
	return resp, nil
}

// tokenWithUser is Keystone's answer to a token validation, naming the token's
// user.
const tokenWithUser = `{"token": {"user": {"id": "user-1", "name": "admin", "domain": {"id": "default"}}}}`

// gophercloud decodes an answer whose body lacks the object's key to a nil
// object and no error. These creates used to dereference it and crash the
// provider. Each must report an error and leave nothing in state.
func TestCreateRefusesAnAnswerWithoutTheObject(t *testing.T) {
	t.Parallel()
	creates := []struct {
		name        string
		routes      map[string]http.HandlerFunc
		newResource func(*clients.Config) resource.Resource
		planned     any
	}{
		{"project", map[string]http.HandlerFunc{"POST /v3/projects": reply(http.StatusCreated, `{}`)},
			func(c *clients.Config) resource.Resource { return &projectResource{config: c} },
			&projectModel{ID: types.StringUnknown(), Name: types.StringValue("workload"), Description: types.StringUnknown(),
				DomainID: types.StringUnknown(), Enabled: types.BoolValue(true), IsDomain: types.BoolValue(false),
				ParentID: types.StringUnknown(), Tags: types.SetUnknown(types.StringType), Region: types.StringUnknown()}},
		{"group", map[string]http.HandlerFunc{"POST /v3/groups": reply(http.StatusCreated, `{}`)},
			func(c *clients.Config) resource.Resource { return &groupResource{config: c} },
			&groupModel{ID: types.StringUnknown(), Name: types.StringValue("operators"), Description: types.StringUnknown(),
				DomainID: types.StringUnknown(), Region: types.StringUnknown()}},
		{"role", map[string]http.HandlerFunc{"POST /v3/roles": reply(http.StatusCreated, `{}`)},
			func(c *clients.Config) resource.Resource { return &roleResource{config: c} },
			&roleModel{ID: types.StringUnknown(), Name: types.StringValue("auditor"), DomainID: types.StringUnknown(),
				Region: types.StringUnknown()}},
		{"user", map[string]http.HandlerFunc{"POST /v3/users": reply(http.StatusCreated, `{}`)},
			func(c *clients.Config) resource.Resource { return &userResource{config: c} },
			&userModel{ID: types.StringUnknown(), Name: types.StringValue("deployer"), Description: types.StringUnknown(),
				DomainID: types.StringUnknown(), DefaultProjectID: types.StringUnknown(), Enabled: types.BoolValue(true),
				Password: types.StringNull(), Region: types.StringUnknown()}},
		{"application_credential", map[string]http.HandlerFunc{
			"GET /v3/auth/tokens":                           reply(http.StatusOK, tokenWithUser),
			"POST /v3/users/user-1/application_credentials": reply(http.StatusCreated, `{}`),
		},
			func(c *clients.Config) resource.Resource { return &appCredResource{config: c} },
			appCredPlanned()},
		{"application_credential, token without its user", map[string]http.HandlerFunc{
			"GET /v3/auth/tokens": reply(http.StatusOK, `{"token": {}}`),
		},
			func(c *clients.Config) resource.Resource { return &appCredResource{config: c} },
			appCredPlanned()},
	}
	for _, c := range creates {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			r := c.newResource(newFakeKeystone(t, c.routes))
			var resp resource.CreateResponse
			var err error
			noPanic(t, func() { resp, err = runCreate(r, c.planned) })
			if err != nil {
				t.Fatal(err)
			}
			if !resp.Diagnostics.HasError() {
				t.Fatal("create succeeded; want an error for an answer without the object")
			}
			if !resp.State.Raw.IsNull() {
				t.Fatalf("create left a row: %v", resp.State.Raw)
			}
		})
	}
}

// appCredPlanned is the plan for a new application credential whose config
// sets only its name.
func appCredPlanned() *appCredModel {
	return &appCredModel{ID: types.StringUnknown(), Name: types.StringValue("ci"), Description: types.StringUnknown(),
		Secret: types.StringUnknown(), ProjectID: types.StringUnknown(), Roles: types.SetUnknown(types.StringType),
		ExpiresAt: types.StringNull(), Unrestricted: types.BoolValue(false), Region: types.StringUnknown()}
}
