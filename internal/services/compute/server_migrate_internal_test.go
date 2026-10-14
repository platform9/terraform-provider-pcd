// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package compute

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/servers"
)

// fakeServerState is one GET /servers/{id} answer.
type fakeServerState struct {
	status, task, host string
}

// fakeMigrateNova serves one or more servers whose GETs walk through a
// scripted list of states (the last one repeats), records every server action
// with its microversion header, and answers instance-action lookups.
type fakeMigrateNova struct {
	t       *testing.T
	mu      sync.Mutex
	states  map[string][]fakeServerState
	gets    map[string]int
	actions []fakeAction
	// actionStatus, when set, overrides the 202 for an action key such as
	// "os-migrateLive"; actionBody is then the error body.
	actionStatus map[string]int
	actionBody   map[string]string
	// onAction runs after an action is accepted, to script what Nova does next.
	// It runs while f.mu is held, so it must never call actionLog() or another
	// locked accessor.
	onAction func(f *fakeMigrateNova, id, name string)
	// event is the failed event GET /os-instance-actions/req-<id> returns.
	event map[string]any
	// other answers requests the migration fake does not know; nil fails them.
	// It runs while f.mu is held, so it must never call actionLog() or another
	// locked accessor.
	other func(w http.ResponseWriter, r *http.Request) bool
}

type fakeAction struct {
	server, name, microversion string
	body                       map[string]any
}

func newFakeMigrateNova(t *testing.T, states map[string][]fakeServerState) (*fakeMigrateNova, *httptest.Server) {
	f := &fakeMigrateNova{t: t, states: states, gets: map[string]int{}, actionStatus: map[string]int{}, actionBody: map[string]string{}}
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)
	return f, srv
}

func (f *fakeMigrateNova) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	switch {
	case r.Method == "GET" && len(parts) == 2 && parts[0] == "servers" && f.states[parts[1]] != nil:
		id := parts[1]
		seq := f.states[id]
		i := f.gets[id]
		if i >= len(seq) {
			i = len(seq) - 1
		}
		f.gets[id]++
		st := seq[i]
		out, _ := json.Marshal(map[string]any{"server": map[string]any{
			"id": id, "name": "vm-" + id, "status": st.status, "OS-EXT-STS:task_state": nullable(st.task),
			"OS-EXT-SRV-ATTR:host": st.host, "metadata": map[string]any{}, "addresses": map[string]any{},
			"fault": map[string]any{"message": "boom"},
		}})
		_, _ = w.Write(out)
	case r.Method == "POST" && len(parts) == 3 && parts[0] == "servers" && parts[2] == "action":
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		if err := json.Unmarshal(raw, &body); err != nil {
			f.t.Errorf("action body %q: %v", raw, err)
		}
		var name string
		for k := range body {
			name = k
		}
		f.actions = append(f.actions, fakeAction{server: parts[1], name: name, microversion: r.Header.Get("OpenStack-API-Version"), body: body})
		if code := f.actionStatus[name]; code != 0 {
			w.WriteHeader(code)
			fmt.Fprint(w, f.actionBody[name])
			return
		}
		if f.onAction != nil {
			f.onAction(f, parts[1], name)
		}
		w.Header().Set("X-Openstack-Request-Id", "req-"+parts[1])
		w.WriteHeader(http.StatusAccepted)
	case r.Method == "GET" && len(parts) == 4 && parts[2] == "os-instance-actions":
		events := []any{}
		if f.event != nil {
			events = append(events, f.event)
		}
		out, _ := json.Marshal(map[string]any{"instanceAction": map[string]any{
			"action": "migrate", "request_id": parts[3], "message": "Error", "events": events,
		}})
		_, _ = w.Write(out)
	default:
		if f.other != nil && f.other(w, r) {
			return
		}
		f.t.Errorf("unexpected request %s %s", r.Method, r.URL.String())
		w.WriteHeader(http.StatusNotImplemented)
	}
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func (f *fakeMigrateNova) actionLog() []fakeAction {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]fakeAction(nil), f.actions...)
}

// fastServerPolls shortens the server waiter's poll interval for one test
// through shortPolls.
func fastServerPolls(t *testing.T) {
	t.Helper()
	shortPolls(t)
}

// computeClientFor is a compute client for the fake. Type makes gophercloud
// send microversion headers the way the real client does, and the transport
// is its own: closing a test server closes the default transport's idle
// connections.
func computeClientFor(url string) *gophercloud.ServiceClient {
	return &gophercloud.ServiceClient{
		ProviderClient: &gophercloud.ProviderClient{HTTPClient: http.Client{Transport: &http.Transport{}}},
		Endpoint:       url + "/",
		Type:           "compute",
	}
}

func bodyJSON(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func noProgress(string) {}

func TestLiveMigrateOptsBody(t *testing.T) {
	b, _ := liveMigrateOpts{}.ToLiveMigrateMap()
	if got := bodyJSON(t, b); got != `{"os-migrateLive":{"block_migration":"auto","host":null}}` {
		t.Fatalf("scheduler-chosen body = %s", got)
	}
	b, _ = liveMigrateOpts{Host: "host-b"}.ToLiveMigrateMap()
	if got := bodyJSON(t, b); got != `{"os-migrateLive":{"block_migration":"auto","host":"host-b"}}` {
		t.Fatalf("targeted body = %s", got)
	}
}

func TestColdMigrateBody(t *testing.T) {
	if got := bodyJSON(t, coldMigrateBody("host-b", "")); got != `{"migrate":{"host":"host-b"}}` {
		t.Fatalf("targeted body = %s", got)
	}
	if got := bodyJSON(t, coldMigrateBody("host-b", "cluster-2")); got != `{"migrate":{"availability_zone":"cluster-2","host":"host-b"}}` {
		t.Fatalf("cross-cluster body = %s", got)
	}
}

func TestMigrateServer(t *testing.T) {
	fastServerPolls(t)
	ctx := context.Background()
	srv := func(status, host string) *servers.Server {
		return &servers.Server{ID: "srv-1", Status: status, Host: host}
	}

	t.Run("live to a scheduler-chosen host", func(t *testing.T) {
		f, nova := newFakeMigrateNova(t, map[string][]fakeServerState{"srv-1": {
			{"MIGRATING", "migrating", "host-a"}, {"ACTIVE", "", "host-b"},
		}})
		res, err := migrateServer(ctx, computeClientFor(nova.URL), srv("ACTIVE", "host-a"), migrateRequest{Live: true, Confirm: true}, noProgress)
		if err != nil {
			t.Fatalf("migrateServer: %v", err)
		}
		if res.FromHost != "host-a" || res.Server.Host != "host-b" || res.Unconfirmed {
			t.Fatalf("result = %+v", res)
		}
		acts := f.actionLog()
		if len(acts) != 1 || acts[0].microversion != "compute 2.68" ||
			bodyJSON(t, acts[0].body) != `{"os-migrateLive":{"block_migration":"auto","host":null}}` {
			t.Fatalf("actions = %+v", acts)
		}
	})
	t.Run("cold to a scheduler-chosen host is confirmed", func(t *testing.T) {
		f, nova := newFakeMigrateNova(t, map[string][]fakeServerState{"srv-1": {
			{"SHUTOFF", "resize_migrating", "host-a"}, {"VERIFY_RESIZE", "", "host-b"},
			{"VERIFY_RESIZE", "", "host-b"}, {"SHUTOFF", "", "host-b"},
		}})
		res, err := migrateServer(ctx, computeClientFor(nova.URL), srv("SHUTOFF", "host-a"), migrateRequest{Confirm: true}, noProgress)
		if err != nil {
			t.Fatalf("migrateServer: %v", err)
		}
		if res.Server.Host != "host-b" || res.Server.Status != "SHUTOFF" {
			t.Fatalf("result server = %s on %s", res.Server.Status, res.Server.Host)
		}
		acts := f.actionLog()
		if len(acts) != 2 || bodyJSON(t, acts[0].body) != `{"migrate":null}` || acts[0].microversion != "" ||
			acts[1].name != "confirmResize" {
			t.Fatalf("actions = %+v; want {\"migrate\":null} at the default microversion, then confirmResize", acts)
		}
	})
	t.Run("an unconfirmed cold migration is left in VERIFY_RESIZE", func(t *testing.T) {
		f, nova := newFakeMigrateNova(t, map[string][]fakeServerState{"srv-1": {{"VERIFY_RESIZE", "", "host-b"}}})
		res, err := migrateServer(ctx, computeClientFor(nova.URL), srv("SHUTOFF", "host-a"), migrateRequest{Confirm: false}, noProgress)
		if err != nil || !res.Unconfirmed {
			t.Fatalf("res=%+v err=%v; want Unconfirmed", res, err)
		}
		if acts := f.actionLog(); len(acts) != 1 {
			t.Fatalf("actions = %+v; want no confirmResize", acts)
		}
	})
	t.Run("cold to a chosen host uses 2.56", func(t *testing.T) {
		f, nova := newFakeMigrateNova(t, map[string][]fakeServerState{"srv-1": {{"VERIFY_RESIZE", "", "host-b"}, {"ACTIVE", "", "host-b"}}})
		if _, err := migrateServer(ctx, computeClientFor(nova.URL), srv("ACTIVE", "host-a"), migrateRequest{Host: "host-b", Confirm: true}, noProgress); err != nil {
			t.Fatalf("migrateServer: %v", err)
		}
		acts := f.actionLog()
		if acts[0].microversion != "compute 2.56" || bodyJSON(t, acts[0].body) != `{"migrate":{"host":"host-b"}}` {
			t.Fatalf("first action = %+v", acts[0])
		}
	})
	t.Run("cross-cluster uses 2.101 and explains a 406", func(t *testing.T) {
		f, nova := newFakeMigrateNova(t, map[string][]fakeServerState{"srv-1": {{"SHUTOFF", "", "host-a"}}})
		f.actionStatus["migrate"] = http.StatusNotAcceptable
		f.actionBody["migrate"] = `{"computeFault": {"code": 406, "message": "Version 2.101 is not supported by the API."}}`
		_, err := migrateServer(ctx, computeClientFor(nova.URL), srv("SHUTOFF", "host-a"),
			migrateRequest{Host: "host-b", AvailabilityZone: "cluster-2", Confirm: true}, noProgress)
		if err == nil || !strings.Contains(err.Error(), "does not support migration to another cluster") {
			t.Fatalf("err = %v", err)
		}
		if acts := f.actionLog(); acts[0].microversion != "compute 2.101" {
			t.Fatalf("microversion = %q, want compute 2.101", acts[0].microversion)
		}
	})
	t.Run("an async failure is found in the instance action", func(t *testing.T) {
		f, nova := newFakeMigrateNova(t, map[string][]fakeServerState{"srv-1": {
			{"ACTIVE", "migrating", "host-a"}, {"ACTIVE", "", "host-a"},
		}})
		f.event = map[string]any{"event": "conductor_live_migrate_instance", "result": "Error",
			"traceback": "Traceback (most recent call last):\n  File \"x.py\", line 1\nnova.exception.NoValidHost: No valid host was found. There are not enough hosts available.\n"}
		_, err := migrateServer(ctx, computeClientFor(nova.URL), srv("ACTIVE", "host-a"), migrateRequest{Live: true, Confirm: true}, noProgress)
		if err == nil || !strings.Contains(err.Error(), "NoValidHost") || !strings.Contains(err.Error(), "still on host-a") {
			t.Fatalf("err = %v; want the NoValidHost from the instance action", err)
		}
	})
	t.Run("a synchronous refusal is returned as is", func(t *testing.T) {
		f, nova := newFakeMigrateNova(t, map[string][]fakeServerState{"srv-1": {{"ACTIVE", "", "host-a"}}})
		f.actionStatus["migrate"] = http.StatusBadRequest
		f.actionBody["migrate"] = `{"badRequest": {"code": 400, "message": "The target host can't be the same one."}}`
		_, err := migrateServer(ctx, computeClientFor(nova.URL), srv("ACTIVE", "host-a"), migrateRequest{Host: "host-a", Confirm: true}, noProgress)
		if err == nil || !strings.Contains(err.Error(), "can't be the same one") {
			t.Fatalf("err = %v; want Nova's message", err)
		}
	})
	t.Run("an instance that ends in ERROR fails", func(t *testing.T) {
		_, nova := newFakeMigrateNova(t, map[string][]fakeServerState{"srv-1": {{"ERROR", "", "host-a"}}})
		_, err := migrateServer(ctx, computeClientFor(nova.URL), srv("ACTIVE", "host-a"), migrateRequest{Live: true, Confirm: true}, noProgress)
		if err == nil || !strings.Contains(err.Error(), "ERROR") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("a token that cannot see the host is refused before any request", func(t *testing.T) {
		f, nova := newFakeMigrateNova(t, map[string][]fakeServerState{"srv-1": {{"ACTIVE", "", ""}}})
		_, err := migrateServer(ctx, computeClientFor(nova.URL), srv("ACTIVE", ""), migrateRequest{Live: true}, noProgress)
		if !errors.Is(err, errNotAdmin) || len(f.actionLog()) != 0 {
			t.Fatalf("err = %v, actions = %v", err, f.actionLog())
		}
	})
}

func TestLastLine(t *testing.T) {
	if got := lastLine("a\nb\nnova.exception.NoValidHost: none\n\n"); got != "nova.exception.NoValidHost: none" {
		t.Fatalf("got %q", got)
	}
}

// Nova live-migrates only ACTIVE and PAUSED instances; PCD's Nova
// cold-migrates ACTIVE, SHUTOFF and also SUSPENDED ones (a PF9 change).
func TestMigratableStatuses(t *testing.T) {
	for status, want := range map[string][2]bool{
		"ACTIVE": {true, true}, "PAUSED": {true, false}, "SHUTOFF": {false, true}, "SUSPENDED": {false, true},
		"VERIFY_RESIZE": {false, false}, "RESCUE": {false, false}, "ERROR": {false, false}, "SHELVED_OFFLOADED": {false, false},
	} {
		if liveMigratable(status) != want[0] || coldMigratable(status) != want[1] {
			t.Errorf("%s: live=%v cold=%v, want %v", status, liveMigratable(status), coldMigratable(status), want)
		}
	}
}
