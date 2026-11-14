// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package mors

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

	"github.com/gophercloud/gophercloud/v2"
	"github.com/hashicorp/terraform-plugin-framework/diag"

	"github.com/platform9/terraform-provider-pcd/internal/clients"
)

// fakeReply is one canned answer: a status code and a raw body.
type fakeReply struct {
	status int
	body   string
}

// fakeCall is one request the fake received: "METHOD /path" and the decoded
// JSON body (nil when the request had none).
type fakeCall struct {
	key  string
	body map[string]any
}

// fakeMors is an httptest server that answers each "METHOD /path" from a queue
// of canned replies (the last one repeats) and records every request. It also
// serves Nova's GET /servers/<id>, which the instance lease reads, because the
// fake's catalog answers every service type with the same URL.
type fakeMors struct {
	t      *testing.T
	srv    *httptest.Server
	mu     sync.Mutex
	routes map[string][]fakeReply
	calls  []fakeCall
}

func newFakeMors(t *testing.T, routes map[string][]fakeReply) *fakeMors {
	t.Helper()
	f := &fakeMors{t: t, routes: routes}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeMors) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := r.Method + " " + r.URL.Path
	raw, _ := io.ReadAll(r.Body)
	var body map[string]any
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &body); err != nil {
			f.t.Errorf("%s: request body is not a JSON object: %s", key, raw)
		}
	}
	f.calls = append(f.calls, fakeCall{key: key, body: body})
	queue, ok := f.routes[key]
	if !ok || len(queue) == 0 {
		f.t.Errorf("unexpected request %s", key)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotImplemented)
		return
	}
	reply := queue[0]
	if len(queue) > 1 {
		f.routes[key] = queue[1:]
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(reply.status)
	fmt.Fprint(w, reply.body)
}

// client returns a mors client the way MorsV1Client builds one. Its provider
// client gets a transport of its own, not http.DefaultTransport: every
// httptest.Server.Close closes the default transport's idle connections,
// which can cut a request another parallel test is sending.
func (f *fakeMors) client() *gophercloud.ServiceClient {
	return &gophercloud.ServiceClient{
		ProviderClient: &gophercloud.ProviderClient{HTTPClient: http.Client{Transport: &http.Transport{}}},
		Endpoint:       f.srv.URL + "/v1/",
	}
}

// bodyOf returns the JSON body of the first request with key.
func (f *fakeMors) bodyOf(key string) map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		if c.key == key {
			return c.body
		}
	}
	f.t.Fatalf("no %s request was made; got %v", key, f.calls)
	return nil
}

func TestGetJSONStatusHandling(t *testing.T) {
	f := newFakeMors(t, map[string][]fakeReply{
		"GET /v1/tenant/present": {{http.StatusOK, `{"vm_lease_policy": {"tenant_uuid": "present"}}`}},
		"GET /v1/tenant/missing": {{http.StatusNotFound, `{"success": false}`}},
	})
	var out map[string]any
	if err := getJSON(context.Background(), f.client(), f.client().ServiceURL("tenant", "present"), &out); err != nil {
		t.Fatalf("200: %v", err)
	}
	if _, ok := out["vm_lease_policy"]; !ok {
		t.Fatalf("200 body not decoded: %v", out)
	}
	err := getJSON(context.Background(), f.client(), f.client().ServiceURL("tenant", "missing"), &out)
	if !isNotFound(err) {
		t.Fatalf("404: err = %v, want isNotFound; Read would fail instead of dropping a deleted object", err)
	}
}

// mors answers a successful write with 200, which gophercloud does not accept
// for POST or PUT by default. Its refusals carry the reason in the body, which
// must reach the user.
func TestWriteJSONStatusHandling(t *testing.T) {
	for _, tc := range []struct {
		name     string
		method   string
		status   int
		body     string
		wantErr  string
		conflict bool
	}{
		{name: "POST 200", method: http.MethodPost, status: http.StatusOK, body: `{"success": true}`},
		{name: "PUT 200", method: http.MethodPut, status: http.StatusOK, body: `{"success": true}`},
		{name: "POST 409 is a conflict", method: http.MethodPost, status: http.StatusConflict, body: `{"error": "Already exists"}`, wantErr: "Already exists", conflict: true},
		{name: "PUT 422 surfaces the reason", method: http.MethodPut, status: http.StatusUnprocessableEntity, body: `{"error": "Expiry time must be in the future"}`, wantErr: "Expiry time must be in the future"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeMors(t, map[string][]fakeReply{tc.method + " /v1/tenant/p1": {{tc.status, tc.body}}})
			err := writeJSON(context.Background(), f.client(), tc.method, f.client().ServiceURL("tenant", "p1"), map[string]any{"k": "v"})
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if got := f.bodyOf(tc.method + " /v1/tenant/p1"); got["k"] != "v" {
					t.Fatalf("body sent = %v, want {k: v}", got)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want it to carry %q", err, tc.wantErr)
			}
			if isConflict(err) != tc.conflict {
				t.Fatalf("isConflict = %v, want %v", isConflict(err), tc.conflict)
			}
		})
	}
	if err := writeJSON(context.Background(), &gophercloud.ServiceClient{}, http.MethodPatch, "http://unused/", nil); err == nil {
		t.Fatal("PATCH accepted; mors has no PATCH routes")
	}
}

func TestDeleteObjectStatusHandling(t *testing.T) {
	for _, tc := range []struct {
		name    string
		status  int
		wantErr bool
	}{
		{name: "200 is deleted", status: http.StatusOK},
		{name: "204 is deleted", status: http.StatusNoContent},
		// Already gone is what destroy wants.
		{name: "404 is already gone", status: http.StatusNotFound},
		{name: "500 is an error", status: http.StatusInternalServerError, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeMors(t, map[string][]fakeReply{"DELETE /v1/tenant/p1": {{tc.status, `{}`}}})
			err := deleteObject(context.Background(), f.client(), f.client().ServiceURL("tenant", "p1"))
			if (err != nil) != tc.wantErr {
				t.Fatalf("status %d: err = %v, wantErr %v", tc.status, err, tc.wantErr)
			}
		})
	}
}

// A resource configured before the provider has its credentials gets nil and
// must not fail; anything other than *clients.Config is a provider bug.
func TestConfigureClient(t *testing.T) {
	var diags diag.Diagnostics
	if got := configureClient(nil, &diags); got != nil || diags.HasError() {
		t.Fatalf("nil provider data: got %v, %v; want nil and no error", got, diags)
	}
	cfg := &clients.Config{Region: "region-one"}
	if got := configureClient(cfg, &diags); got != cfg || diags.HasError() {
		t.Fatalf("*clients.Config: got %v, %v; want the same config and no error", got, diags)
	}
	if got := configureClient("not a config", &diags); got != nil || !diags.HasError() {
		t.Fatalf("wrong type: got %v, %v; want nil and an error", got, diags)
	}
}
