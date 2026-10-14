// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package compute

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/servers"
	"github.com/hashicorp/terraform-plugin-framework/action"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

const drainReason = "0b5a6c3e-1f2d-4a5b-8c7d-9e0f1a2b3c4d:1759744800000"

// hostDrainConfig builds the action's configuration for Invoke: every
// attribute not in vals is null, as when the configuration omits it.
func hostDrainConfig(t *testing.T, vals map[string]tftypes.Value) tfsdk.Config {
	t.Helper()
	ctx := context.Background()
	var sch action.SchemaResponse
	(&hostDrainAction{}).Schema(ctx, action.SchemaRequest{}, &sch)
	typ := sch.Schema.Type().TerraformType(ctx).(tftypes.Object)
	full := map[string]tftypes.Value{}
	for name, at := range typ.AttributeTypes {
		if v, ok := vals[name]; ok {
			full[name] = v
		} else {
			full[name] = tftypes.NewValue(at, nil)
		}
	}
	return tfsdk.Config{Schema: sch.Schema, Raw: tftypes.NewValue(typ, full)}
}

// drainFake extends the migration fake with the host's service record, the
// host's instance list, and server tags.
type drainFake struct {
	*fakeMigrateNova
	service map[string]any
	onHost  []map[string]any
	tagged  []string
}

func newDrainFake(t *testing.T, serviceStatus, reason string, onHost []map[string]any, states map[string][]fakeServerState) (*drainFake, string) {
	f, srv := newFakeMigrateNova(t, states)
	d := &drainFake{fakeMigrateNova: f, service: serviceRecord(serviceStatus, reason), onHost: onHost}
	f.other = func(w http.ResponseWriter, r *http.Request) bool {
		switch {
		case r.Method == "GET" && r.URL.Path == "/os-services":
			out, _ := json.Marshal(map[string]any{"services": []any{d.service}})
			_, _ = w.Write(out)
		case r.Method == "GET" && r.URL.Path == "/servers/detail":
			if q := r.URL.Query(); q.Get("all_tenants") != "true" || q.Get("host") != testHostID {
				t.Errorf("instance list query %q; want all_tenants=true&host=%s", r.URL.RawQuery, testHostID)
			}
			out, _ := json.Marshal(map[string]any{"servers": d.onHost})
			_, _ = w.Write(out)
		case r.Method == "PUT" && strings.Contains(r.URL.Path, "/tags/"):
			if mv := r.Header.Get("OpenStack-API-Version"); mv != "compute 2.26" {
				t.Errorf("tag sent at %q, want compute 2.26", mv)
			}
			d.tagged = append(d.tagged, r.URL.Path)
			w.WriteHeader(http.StatusCreated)
		default:
			return false
		}
		return true
	}
	return d, srv.URL
}

func listed(id, status, task string) map[string]any {
	return map[string]any{"id": id, "name": "vm-" + id, "status": status, "OS-EXT-STS:task_state": nullable(task),
		"OS-EXT-SRV-ATTR:host": testHostID, "metadata": map[string]any{}, "addresses": map[string]any{}}
}

func invokeDrain(t *testing.T, url string, vals map[string]tftypes.Value) (action.InvokeResponse, []string) {
	t.Helper()
	a := &hostDrainAction{config: fakeConfig(url)}
	var events []string
	resp := action.InvokeResponse{SendProgress: func(e action.InvokeProgressEvent) { events = append(events, e.Message) }}
	a.Invoke(context.Background(), action.InvokeRequest{Config: hostDrainConfig(t, vals)}, &resp)
	return resp, events
}

func hostVal() map[string]tftypes.Value {
	return map[string]tftypes.Value{"host_id": tftypes.NewValue(tftypes.String, testHostID)}
}

func TestDrainPlan(t *testing.T) {
	onHost := []servers.Server{
		{ID: "a", Name: "a", Status: "ACTIVE"}, {ID: "p", Name: "p", Status: "PAUSED"},
		{ID: "o", Name: "o", Status: "SHUTOFF"}, {ID: "s", Name: "s", Status: "SUSPENDED"},
		{ID: "v", Name: "v", Status: "VERIFY_RESIZE"}, {ID: "r", Name: "r", Status: "RESCUE"},
		{ID: "e", Name: "e", Status: "ERROR"}, {ID: "b", Name: "b", Status: "ACTIVE", TaskState: "rebooting"},
	}
	kinds := func(moves []drainMove) string {
		var out []string
		for _, m := range moves {
			k := "cold"
			if m.live {
				k = "live"
			}
			out = append(out, m.server.ID+"="+k)
		}
		return strings.Join(out, " ")
	}
	moves, left := drainPlan(onHost, false)
	if got := kinds(moves); got != "a=live p=live o=cold s=cold" {
		t.Fatalf("moves = %s", got)
	}
	if len(left) != 4 {
		t.Fatalf("left = %v; want v, r, e and the busy b", left)
	}
	moves, left = drainPlan(onHost, true)
	if got := kinds(moves); got != "a=live p=live s=cold" {
		t.Fatalf("skip_powered_off moves = %s; suspended VMs still move, as in the UI", got)
	}
	if len(left) != 5 {
		t.Fatalf("skip_powered_off left = %v", left)
	}
}

// The drain tags each instance with the maintenance ID, moves it, and checks
// it left: here one live and one cold migration both land on host-b.
func TestHostDrainMovesEveryInstance(t *testing.T) {
	fastServerPolls(t)
	d, url := newDrainFake(t, "disabled", drainReason,
		[]map[string]any{listed("srv-1", "ACTIVE", ""), listed("srv-2", "SHUTOFF", "")},
		map[string][]fakeServerState{
			"srv-1": {{"ACTIVE", "migrating", testHostID}, {"ACTIVE", "", "host-b"}},
			"srv-2": {{"VERIFY_RESIZE", "", "host-b"}, {"SHUTOFF", "", "host-b"}},
		})
	resp, events := invokeDrain(t, url, hostVal())
	if resp.Diagnostics.HasError() {
		t.Fatalf("drain: %v", resp.Diagnostics)
	}
	if len(d.tagged) != 2 || !strings.HasSuffix(d.tagged[0], "/servers/srv-1/tags/0b5a6c3e-1f2d-4a5b-8c7d-9e0f1a2b3c4d") {
		t.Fatalf("tags = %v; want each instance tagged with the maintenance uuid", d.tagged)
	}
	var names []string
	for _, a := range d.actionLog() {
		names = append(names, a.server+":"+a.name)
	}
	if got := strings.Join(names, " "); got != "srv-1:os-migrateLive srv-2:migrate srv-2:confirmResize" {
		t.Fatalf("actions = %s", got)
	}
	if len(events) == 0 || !strings.Contains(events[len(events)-1], "2 instances moved") {
		t.Fatalf("progress = %v", events)
	}
}

// One instance that cannot move does not stop the others, and the drain fails
// naming it, so the apply shows what is left.
func TestHostDrainReportsInstancesThatStayed(t *testing.T) {
	fastServerPolls(t)
	d, url := newDrainFake(t, "disabled", drainReason,
		[]map[string]any{listed("srv-1", "ACTIVE", ""), listed("srv-2", "ACTIVE", "")},
		map[string][]fakeServerState{
			"srv-1": {{"ACTIVE", "", testHostID}},
			"srv-2": {{"ACTIVE", "", "host-b"}},
		})
	d.onAction = func(f *fakeMigrateNova, id, _ string) {
		if id == "srv-1" {
			f.event = map[string]any{"event": "conductor_live_migrate_instance", "result": "Error",
				"traceback": "nova.exception.NoValidHost: No valid host was found."}
		}
	}
	resp, _ := invokeDrain(t, url, hostVal())
	if !resp.Diagnostics.HasError() {
		t.Fatal("drain succeeded; want srv-1 reported")
	}
	detail := resp.Diagnostics[len(resp.Diagnostics)-1].Detail()
	if !strings.Contains(detail, "srv-1") || !strings.Contains(detail, "NoValidHost") || strings.Contains(detail, "srv-2") {
		t.Fatalf("detail = %q; want only srv-1 and its NoValidHost", detail)
	}
	if n := len(d.actionLog()); n != 2 {
		t.Fatalf("%d migrations requested; srv-2 must still be moved after srv-1 failed", n)
	}
}

// A host that still takes new instances is left alone, with a warning, so a
// trigger that fires when the host goes back to enabled does no harm.
func TestHostDrainSkipsAnEnabledHost(t *testing.T) {
	d, url := newDrainFake(t, "enabled", "", []map[string]any{listed("srv-1", "ACTIVE", "")}, nil)
	resp, _ := invokeDrain(t, url, hostVal())
	if resp.Diagnostics.HasError() || len(resp.Diagnostics) != 1 || resp.Diagnostics[0].Summary() != "Host not in maintenance" {
		t.Fatalf("diags = %v; want only the warning", resp.Diagnostics)
	}
	if len(d.actionLog()) != 0 {
		t.Fatalf("actions = %v; nothing may move off a host that still takes new instances", d.actionLog())
	}
}

// A host disabled without the UI's reason still drains, untagged, with a
// warning; shut-off instances stay when skip_powered_off is set.
func TestHostDrainWithoutMaintenanceID(t *testing.T) {
	fastServerPolls(t)
	d, url := newDrainFake(t, "disabled", "", []map[string]any{listed("srv-1", "PAUSED", ""), listed("srv-2", "SHUTOFF", "")},
		map[string][]fakeServerState{"srv-1": {{"PAUSED", "", "host-b"}}})
	vals := hostVal()
	vals["skip_powered_off"] = tftypes.NewValue(tftypes.Bool, true)
	resp, events := invokeDrain(t, url, vals)
	if resp.Diagnostics.HasError() {
		t.Fatalf("drain: %v", resp.Diagnostics)
	}
	if len(d.tagged) != 0 {
		t.Fatalf("tagged %v without a maintenance ID", d.tagged)
	}
	if len(resp.Diagnostics) != 2 {
		t.Fatalf("diags = %v; want the missing-ID warning and the left-on-host warning", resp.Diagnostics)
	}
	if acts := d.actionLog(); len(acts) != 1 || acts[0].server != "srv-1" {
		t.Fatalf("actions = %+v; want only the paused srv-1 moved", acts)
	}
	if len(events) == 0 || !strings.Contains(events[len(events)-1], "1 instance moved off") {
		t.Fatalf("progress = %v; want the singular for one instance", events)
	}
}

// A cold migration that lands on another host but whose confirm fails has left
// the drained host: the error says where the instance is now, and that it must
// be confirmed or reverted, since running the drain again would not see it.
func TestHostDrainReportsAnUnconfirmedMove(t *testing.T) {
	fastServerPolls(t)
	d, url := newDrainFake(t, "disabled", drainReason, []map[string]any{listed("srv-1", "SHUTOFF", "")},
		map[string][]fakeServerState{"srv-1": {{"VERIFY_RESIZE", "", "host-b"}}})
	d.actionStatus["confirmResize"] = http.StatusConflict
	d.actionBody["confirmResize"] = `{"conflictingRequest": {"code": 409, "message": "Instance srv-1 is in an invalid state for 'confirmResize'"}}`
	resp, _ := invokeDrain(t, url, hostVal())
	if !resp.Diagnostics.HasError() {
		t.Fatal("drain succeeded; want the unconfirmed srv-1 reported")
	}
	detail := resp.Diagnostics[len(resp.Diagnostics)-1].Detail()
	for _, want := range []string{"1 of 1 migration off", "vm-srv-1 (srv-1)", "now VERIFY_RESIZE on host-b, unconfirmed", "confirmed or reverted", "stays disabled"} {
		if !strings.Contains(detail, want) {
			t.Errorf("detail = %q; want it to contain %q", detail, want)
		}
	}
	if strings.Contains(detail, "did not leave") {
		t.Errorf("detail = %q; the instance left the host", detail)
	}
	var names []string
	for _, a := range d.actionLog() {
		names = append(names, a.server+":"+a.name)
	}
	if got := strings.Join(names, " "); got != "srv-1:migrate srv-1:confirmResize" {
		t.Fatalf("actions = %s", got)
	}
}

// A drain canceled while the first instance moves stops there: the instances
// not yet started are listed as not attempted, not as failed migrations, and
// are neither tagged nor moved.
func TestHostDrainStopsWhenCanceled(t *testing.T) {
	fastServerPolls(t)
	d, url := newDrainFake(t, "disabled", drainReason,
		[]map[string]any{listed("srv-1", "ACTIVE", ""), listed("srv-2", "ACTIVE", ""), listed("srv-3", "SHUTOFF", "")},
		map[string][]fakeServerState{
			"srv-1": {{"ACTIVE", "migrating", testHostID}},
			"srv-2": {{"ACTIVE", "", testHostID}},
			"srv-3": {{"SHUTOFF", "", testHostID}},
		})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// The hook runs under the fake's mutex: it only cancels.
	d.onAction = func(_ *fakeMigrateNova, id, _ string) {
		if id == "srv-1" {
			cancel()
		}
	}
	a := &hostDrainAction{config: fakeConfig(url)}
	resp := action.InvokeResponse{SendProgress: func(action.InvokeProgressEvent) {}}
	a.Invoke(ctx, action.InvokeRequest{Config: hostDrainConfig(t, hostVal())}, &resp)

	if !resp.Diagnostics.HasError() {
		t.Fatal("drain succeeded; want the cancellation reported")
	}
	last := resp.Diagnostics[len(resp.Diagnostics)-1]
	detail := last.Detail()
	if last.Summary() != "Host drain canceled" || !strings.Contains(detail, "was canceled (context canceled) after 1 of 3 instances were attempted") {
		t.Fatalf("diag = %q: %q; want the cancellation after 1 of 3 instances", last.Summary(), detail)
	}
	if !strings.Contains(detail, "Not attempted:\nvm-srv-2 (srv-2)\nvm-srv-3 (srv-3)") {
		t.Errorf("detail = %q; want srv-2 and srv-3 listed as not attempted", detail)
	}
	for _, bad := range []string{"stays disabled", "(srv-2):", "(srv-3):"} {
		if strings.Contains(detail, bad) {
			t.Errorf("detail = %q; it must not contain %q: srv-2 and srv-3 were never tried", detail, bad)
		}
	}
	for _, diag := range resp.Diagnostics {
		if diag.Summary() == "Tagging an instance failed" {
			t.Errorf("tag warning %q; no instance after the cancellation may be tagged", diag.Detail())
		}
	}
	if acts := d.actionLog(); len(acts) != 1 || acts[0].server != "srv-1" {
		t.Fatalf("actions = %+v; want only srv-1's migration requested", acts)
	}
	d.mu.Lock()
	tagged := append([]string(nil), d.tagged...)
	d.mu.Unlock()
	if len(tagged) != 1 || !strings.Contains(tagged[0], "/servers/srv-1/") {
		t.Fatalf("tags = %v; want only srv-1 tagged", tagged)
	}
}

func TestHostDrainEmptyHost(t *testing.T) {
	d, url := newDrainFake(t, "disabled", drainReason, []map[string]any{}, nil)
	resp, events := invokeDrain(t, url, hostVal())
	if resp.Diagnostics.HasError() || len(d.actionLog()) != 0 {
		t.Fatalf("diags=%v actions=%v", resp.Diagnostics, d.actionLog())
	}
	if len(events) != 1 || !strings.Contains(events[0], "0 instances moved") {
		t.Fatalf("progress = %v", events)
	}
}
