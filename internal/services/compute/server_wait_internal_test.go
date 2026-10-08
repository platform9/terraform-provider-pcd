// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package compute

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
)

// shortPolls makes the server waits poll every millisecond for one test. Tests
// that call it must not run in parallel: the interval is a package variable.
func shortPolls(t *testing.T) {
	t.Helper()
	old := serverPollInterval
	serverPollInterval = time.Millisecond
	t.Cleanup(func() { serverPollInterval = old })
}

// serverReplies serves GET /servers/srv-1 from a fixed script, one reply per
// GET, repeating the last one, and counts the GETs.
func serverReplies(t *testing.T, replies ...string) (*gophercloud.ServiceClient, func() int) {
	t.Helper()
	var mu sync.Mutex
	gets := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.Method+" "+r.URL.Path != "GET /servers/srv-1" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotImplemented)
			return
		}
		reply := replies[min(gets, len(replies)-1)]
		gets++
		if reply == "404" {
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"itemNotFound": {"code": 404, "message": "Instance srv-1 could not be found."}}`)
			return
		}
		fmt.Fprint(w, reply)
	}))
	t.Cleanup(srv.Close)
	// A transport of its own, as fakeConfig gives its clients: closing a test
	// server closes the default transport's idle connections.
	client := &gophercloud.ServiceClient{
		ProviderClient: &gophercloud.ProviderClient{HTTPClient: http.Client{Transport: &http.Transport{}}},
		Endpoint:       srv.URL + "/",
	}
	return client, func() int { mu.Lock(); defer mu.Unlock(); return gets }
}

// server renders a GET /servers/{id} body with a status and a task state ("" is null).
func serverJSON(status, task string) string {
	taskJSON := "null"
	if task != "" {
		taskJSON = fmt.Sprintf("%q", task)
	}
	return fmt.Sprintf(`{"server": {"id": "srv-1", "name": "vm-1", "status": %q, "OS-EXT-STS:task_state": %s,
		"metadata": {}, "addresses": {}, "fault": {"code": 500, "message": "boom"}}}`, status, taskJSON)
}

// Nova reports the old status while a power task runs (SHUTOFF while
// powering-on) and ERROR while it hard-reboots an instance in ERROR. A wait
// that judged the status alone would return too early in the first case and
// fail the recovery in the second.
func TestWaitForServerSettled(t *testing.T) {
	cases := []struct {
		name       string
		replies    []string
		want       []string
		wantStatus string // the status returned on success
		wantErr    string // substring of the error; "" means success
		wantGets   int
	}{
		{name: "waits out the task before trusting the status",
			replies: []string{serverJSON("ACTIVE", "powering-off"), serverJSON("SHUTOFF", "")},
			want:    []string{"SHUTOFF"}, wantStatus: "SHUTOFF", wantGets: 2},
		{name: "ERROR while a task runs is not fatal",
			replies: []string{serverJSON("ERROR", "reboot_started_hard"), serverJSON("ACTIVE", "")},
			want:    []string{"ACTIVE"}, wantStatus: "ACTIVE", wantGets: 2},
		{name: "ERROR once settled is fatal and carries the fault",
			replies: []string{serverJSON("ERROR", "")},
			want:    []string{"ACTIVE"}, wantErr: "entered ERROR state: boom", wantGets: 1},
		{name: "a settled status outside want keeps the wait going",
			replies: []string{serverJSON("VERIFY_RESIZE", ""), serverJSON("VERIFY_RESIZE", ""), serverJSON("SHUTOFF", "")},
			want:    []string{"SHUTOFF"}, wantStatus: "SHUTOFF", wantGets: 3},
		{name: "any status in want ends the wait",
			replies: []string{serverJSON("RESIZE", "resize_prep"), serverJSON("SUSPENDED", "")},
			want:    []string{"VERIFY_RESIZE", "SUSPENDED"}, wantStatus: "SUSPENDED", wantGets: 2},
		{name: "a 404 is returned to the caller",
			replies: []string{"404"},
			want:    []string{"ACTIVE"}, wantErr: "404", wantGets: 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			shortPolls(t)
			client, gets := serverReplies(t, tc.replies...)
			got, err := waitForServerSettled(context.Background(), client, "srv-1", tc.want, time.Minute)
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("unexpected error: %v", err)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Fatalf("error = %v, want one containing %q", err, tc.wantErr)
			case tc.wantErr == "" && got.Status != tc.wantStatus:
				t.Fatalf("returned status %q, want %q", got.Status, tc.wantStatus)
			}
			if gets() != tc.wantGets {
				t.Fatalf("%d GETs, want %d", gets(), tc.wantGets)
			}
		})
	}
}

// A task that never finishes ends in a timeout naming the last status and
// task, not in an endless poll.
func TestWaitForServerSettledTimesOut(t *testing.T) {
	shortPolls(t)
	client, _ := serverReplies(t, serverJSON("ACTIVE", "powering-off"))
	_, err := waitForServerSettled(context.Background(), client, "srv-1", []string{"SHUTOFF"}, 20*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), `timed out`) || !strings.Contains(err.Error(), `task state "powering-off"`) {
		t.Fatalf("error = %v, want a timeout naming the task state", err)
	}
}

// withMicroversion pins one call's version on a copy; the shared client must
// keep speaking the provider's default (2.1), whose response shapes every read
// path was written against.
func TestWithMicroversion(t *testing.T) {
	base := &gophercloud.ServiceClient{Endpoint: "https://nova.example/v2.1/"}
	pinned := withMicroversion(base, "2.93")
	if pinned.Microversion != "2.93" || pinned.Endpoint != base.Endpoint {
		t.Fatalf("pinned = %+v", pinned)
	}
	if base.Microversion != "" {
		t.Fatalf("withMicroversion changed the shared client to %q", base.Microversion)
	}
}
