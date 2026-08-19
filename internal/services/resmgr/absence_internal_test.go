// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package resmgr

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gophercloud/gophercloud/v2"
)

// resmgr answers 200 with a body of `null` for an object that is gone, so absence
// has to be read out of the body rather than the status code. Decoded straight into
// a struct that is a nil unmarshal: no error, every field zero — which a Read would
// report as present-but-blank, never removing the resource from state.
func TestGetJSONTreatsANullBodyAsAbsent(t *testing.T) {
	type cluster struct {
		Name string `json:"name"`
	}

	for _, tc := range []struct {
		name       string
		status     int
		body       string
		wantAbsent bool
		wantName   string
	}{
		{name: "null body is absence", status: 200, body: "null", wantAbsent: true},
		{name: "null body with whitespace is absence", status: 200, body: "  null\n", wantAbsent: true},
		{name: "empty body is absence", status: 200, body: "", wantAbsent: true},
		{name: "404 is absence", status: 404, body: `{"message":"not found"}`, wantAbsent: true},
		{name: "an object decodes", status: 200, body: `{"name":"ts-cluster"}`, wantName: "ts-cluster"},
		// Defensive: no caller fetches a list through getJSON today, and an empty one
		// must not be mistaken for a missing object if one ever does.
		{name: "an empty list is not absence", status: 200, body: `[]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()

			client := &gophercloud.ServiceClient{
				ProviderClient: &gophercloud.ProviderClient{},
				Endpoint:       srv.URL + "/",
			}

			var out any = &cluster{}
			if tc.body == `[]` {
				out = &[]cluster{}
			}
			err := getJSON(context.Background(), client, client.ServiceURL("clusters", "ts-cluster"), out)

			if tc.wantAbsent {
				if err == nil {
					t.Fatalf("got no error, want one the Read paths can recognise as absence")
				}
				if !isNotFound(err) {
					t.Fatalf("isNotFound(%v) = false; a Read would keep a destroyed object in state", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tc.wantName != "" && out.(*cluster).Name != tc.wantName {
				t.Fatalf("name = %q, want %q", out.(*cluster).Name, tc.wantName)
			}
		})
	}
}

func TestIsNullJSON(t *testing.T) {
	for _, raw := range []string{"null", " null ", "\n", ""} {
		if !isNullJSON([]byte(raw)) {
			t.Errorf("isNullJSON(%q) = false, want true", raw)
		}
	}
	for _, raw := range []string{`{}`, `[]`, `{"name":"x"}`, `"null"`} {
		if isNullJSON([]byte(raw)) {
			t.Errorf("isNullJSON(%q) = true, want false", raw)
		}
	}
}

// hostList serves a resmgr host list, switching to `then` after the first request so a
// test can watch a binding clear.
func hostList(t *testing.T, first, then string) *gophercloud.ServiceClient {
	t.Helper()
	var n int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		body := first
		if n > 0 && then != "" {
			body = then
		}
		n++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return &gophercloud.ServiceClient{ProviderClient: &gophercloud.ProviderClient{}, Endpoint: srv.URL + "/"}
}

const twoHosts = `[{"id":"host-a","hostconfig_id":"hc-1"},{"id":"host-b","hostconfig_id":"hc-2"}]`

func TestHostsAssignedTo(t *testing.T) {
	for _, tc := range []struct {
		name, body, hostConfig string
		want                   []string
	}{
		{name: "one host carries it", body: twoHosts, hostConfig: "hc-1", want: []string{"host-a"}},
		{name: "nobody carries it", body: twoHosts, hostConfig: "hc-9"},
		{name: "an empty region", body: `[]`, hostConfig: "hc-1"},
		// getJSON reads a null body as absence; for a collection it is simply empty, and
		// mistaking the two here would report a bound host config as safe to delete.
		{name: "a null list is empty, not an error", body: `null`, hostConfig: "hc-1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := hostsAssignedTo(context.Background(), hostList(t, tc.body, ""), tc.hostConfig)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("got %v, want %v", got, tc.want)
				}
			}
		})
	}
}

func TestHostsAssignedToSurfacesAFailedCheck(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	client := &gophercloud.ServiceClient{ProviderClient: &gophercloud.ProviderClient{}, Endpoint: srv.URL + "/"}
	// The delete guard is fail-closed, so this error has to reach it rather than read as
	// "no hosts are assigned".
	if _, err := hostsAssignedTo(context.Background(), client, "hc-1"); err == nil {
		t.Fatal("got no error from a resmgr that would not answer; the guard would have deleted")
	}
}

func TestWaitUnassigned(t *testing.T) {
	interval, timeout := unassignPollInterval, unassignPollTimeout
	unassignPollInterval, unassignPollTimeout = time.Millisecond, 50*time.Millisecond
	defer func() { unassignPollInterval, unassignPollTimeout = interval, timeout }()

	t.Run("returns once the binding clears", func(t *testing.T) {
		client := hostList(t, twoHosts, `[{"id":"host-b","hostconfig_id":"hc-2"}]`)
		if err := waitUnassigned(context.Background(), client, "host-a", "hc-1"); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	// resmgr answers the unassign 204 without necessarily doing it; believing that is what
	// leaves a binding behind for a later host-config delete to make permanent.
	t.Run("fails when resmgr never applies it", func(t *testing.T) {
		err := waitUnassigned(context.Background(), hostList(t, twoHosts, ""), "host-a", "hc-1")
		if err == nil {
			t.Fatal("got no error from a binding that never cleared")
		}
		if !strings.Contains(err.Error(), "host-a") || !strings.Contains(err.Error(), "hc-1") {
			t.Fatalf("error does not say what is still bound: %v", err)
		}
	})
}

// windowed serves resmgr's post-deauth behaviour: the per-host endpoint 404s while the
// list keeps reporting whatever `list` says.
func windowed(t *testing.T, perHostStatus int, perHost, list string) *gophercloud.ServiceClient {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.TrimSuffix(r.URL.Path, "/") == "/hosts" {
			_, _ = w.Write([]byte(list))
			return
		}
		w.WriteHeader(perHostStatus)
		_, _ = w.Write([]byte(perHost))
	}))
	t.Cleanup(srv.Close)
	return &gophercloud.ServiceClient{ProviderClient: &gophercloud.ProviderClient{}, Endpoint: srv.URL + "/"}
}

// A host being deauthorised 404s on its per-host endpoint for minutes while the list still
// reports it. Believing that 404 drops a live assignment or role out of state, and the next
// apply then fails against a reality it never left.
func TestHostRecordDoesNotBelieveThePostDeauthWindow(t *testing.T) {
	const listed = `[{"id":"host-a","roles":["hypervisor"],"hostconfig_id":"hc-1"}]`

	t.Run("404 while the list still has it is not gone", func(t *testing.T) {
		host, known, err := hostRecord(context.Background(),
			windowed(t, 404, `{"message":"HostNotFound"}`, listed), "host-a")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !known {
			t.Fatal("reported the host as gone; a Read would drop a live resource from state")
		}
		if host.HostConfigID != "hc-1" || len(host.Roles) != 1 {
			t.Fatalf("the list record did not come back: %+v", host)
		}
	})

	t.Run("404 and absent from the list is gone", func(t *testing.T) {
		_, known, err := hostRecord(context.Background(),
			windowed(t, 404, `{"message":"HostNotFound"}`, `[{"id":"host-b"}]`), "host-a")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if known {
			t.Fatal("a host resmgr does not list anywhere is gone and must leave state")
		}
	})

	t.Run("an unreadable list fails closed", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.TrimSuffix(r.URL.Path, "/") == "/hosts" {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			w.WriteHeader(http.StatusNotFound)
		}))
		defer srv.Close()
		client := &gophercloud.ServiceClient{ProviderClient: &gophercloud.ProviderClient{}, Endpoint: srv.URL + "/"}
		if _, known, err := hostRecord(context.Background(), client, "host-a"); err == nil || known {
			t.Fatal("an unverified 404 must surface as an error, not as an absence")
		}
	})

	// A per-host failure that is not a 404 says nothing about whether the host exists, so it
	// has to reach the caller as an error. All three Read paths check err before known, which
	// makes this guard the thing standing between a transient 500 or an expired token and a
	// live assignment or role being dropped from state — the outcome this whole fix exists to
	// prevent. Without this case the guard can be deleted and the suite stays green.
	t.Run("a per-host failure that is not a 404 is not an absence", func(t *testing.T) {
		for _, status := range []int{
			http.StatusInternalServerError,
			http.StatusUnauthorized,
			http.StatusForbidden,
			http.StatusServiceUnavailable,
		} {
			_, known, err := hostRecord(context.Background(),
				windowed(t, status, `{"message":"boom"}`, listed), "host-a")
			if err == nil {
				t.Errorf("status %d: got no error; a failed read would be reported as a deleted host", status)
			}
			if known {
				t.Errorf("status %d: reported the host as known off a read that never answered", status)
			}
		}
	})

	t.Run("the ordinary path is one request", func(t *testing.T) {
		var n int
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			n++
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"host-a","roles":["hypervisor"],"hostconfig_id":"hc-1"}`))
		}))
		defer srv.Close()
		client := &gophercloud.ServiceClient{ProviderClient: &gophercloud.ProviderClient{}, Endpoint: srv.URL + "/"}
		if _, known, err := hostRecord(context.Background(), client, "host-a"); err != nil || !known {
			t.Fatalf("known=%v err=%v", known, err)
		}
		if n != 1 {
			t.Fatalf("a host resmgr describes cost %d requests, want 1", n)
		}
	})
}

func TestHostHasRoleThroughTheWindow(t *testing.T) {
	const listed = `[{"id":"host-a","roles":["hypervisor","image-library"]}]`
	client := windowed(t, 404, `{"message":"HostNotFound"}`, listed)

	has, known, err := hostHasRole(context.Background(), client, "host-a", "hypervisor")
	if err != nil || !known || !has {
		t.Fatalf("has=%v known=%v err=%v; the role is still on the host", has, known, err)
	}
	if has, known, _ = hostHasRole(context.Background(), client, "host-a", "persistent-storage"); has || !known {
		t.Fatalf("has=%v known=%v; the host is known, the role is not on it", has, known)
	}
}
