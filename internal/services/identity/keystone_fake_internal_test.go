// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package identity

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/gophercloud/gophercloud/v2"

	"github.com/platform9/terraform-provider-pcd/internal/clients"
)

// fakeConfig points a clients.Config at a test server. gophercloud's identity
// client appends "v3/" to whatever the locator returns, so the fake's paths
// start with /v3. Its client gets a transport of its own, not
// http.DefaultTransport: every httptest.Server.Close closes the default
// transport's idle connections, which can cut a request another parallel test
// is sending.
func fakeConfig(url string) *clients.Config {
	return &clients.Config{
		Region: "region-one",
		Provider: &gophercloud.ProviderClient{
			HTTPClient:      http.Client{Transport: &http.Transport{}},
			EndpointLocator: func(gophercloud.EndpointOpts) (string, error) { return url + "/", nil },
		},
	}
}

// keystoneCall is one request a fake Keystone received.
type keystoneCall struct {
	method, path, body string
}

// fakeKeystone records every request and answers from routes, keyed by
// "<METHOD> <path>". Each route returns a status and a JSON body.
type fakeKeystone struct {
	t      *testing.T
	mu     sync.Mutex
	calls  []keystoneCall
	routes map[string]func() (int, string)
	srv    *httptest.Server
}

func newKeystoneFake(t *testing.T, routes map[string]func() (int, string)) *fakeKeystone {
	t.Helper()
	f := &fakeKeystone{t: t, routes: routes}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		key := r.Method + " " + r.URL.Path
		if r.URL.RawQuery != "" {
			key += "?" + r.URL.RawQuery
		}
		f.mu.Lock()
		f.calls = append(f.calls, keystoneCall{method: r.Method, path: key, body: string(b)})
		route, ok := f.routes[key]
		f.mu.Unlock()
		if !ok {
			t.Errorf("unexpected request %s", key)
			w.WriteHeader(http.StatusNotImplemented)
			return
		}
		status, body := route()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		fmt.Fprint(w, body)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

// paths lists the requests received, as "<METHOD> <path>".
func (f *fakeKeystone) paths() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.calls))
	for _, c := range f.calls {
		out = append(out, c.path)
	}
	return out
}

// bodyOf returns the decoded JSON body of the first request to key.
func (f *fakeKeystone) bodyOf(key string) map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		if c.path == key {
			var m map[string]any
			if err := json.Unmarshal([]byte(c.body), &m); err != nil {
				f.t.Fatalf("body of %s is not JSON: %q", key, c.body)
			}
			return m
		}
	}
	f.t.Fatalf("no request to %s; got %v", key, f.calls)
	return nil
}

// bareClient is a ServiceClient whose endpoint is the fake's root, for helpers
// that take a client directly (no /v3 prefix on the paths). Like fakeConfig,
// it has a transport of its own.
func (f *fakeKeystone) bareClient() *gophercloud.ServiceClient {
	return &gophercloud.ServiceClient{
		ProviderClient: &gophercloud.ProviderClient{HTTPClient: http.Client{Transport: &http.Transport{}}},
		Endpoint:       f.srv.URL + "/",
	}
}

func keystoneReply(status int, body string) func() (int, string) {
	return func() (int, string) { return status, body }
}

// tokenBody is a GET /auth/tokens response for a token whose user lives in
// dom-user and whose project lives in dom-project.
const tokenBody = `{"token": {"methods": ["password"],
	"user": {"id": "u-provider", "name": "admin", "domain": {"id": "dom-user", "name": "users"}},
	"project": {"id": "p-1", "name": "service", "domain": {"id": "dom-project", "name": "projects"}},
	"roles": [], "expires_at": "2099-01-01T00:00:00.000000Z", "issued_at": "2026-01-01T00:00:00.000000Z"}}`
