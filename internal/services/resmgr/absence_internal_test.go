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
