// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package compute

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/action"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-go/tftypes"

	"github.com/platform9/terraform-provider-pcd/internal/clients"
)

// instanceMigrateConfig builds the action's configuration for Invoke; an
// attribute missing from vals is null, and unknown is tftypes.UnknownValue.
func instanceMigrateConfig(t *testing.T, vals map[string]any) tfsdk.Config {
	t.Helper()
	ctx := context.Background()
	var sch action.SchemaResponse
	(&instanceMigrateAction{}).Schema(ctx, action.SchemaRequest{}, &sch)
	typ := sch.Schema.Type().TerraformType(ctx).(tftypes.Object)
	full := map[string]tftypes.Value{}
	for name, at := range typ.AttributeTypes {
		full[name] = tftypes.NewValue(at, vals[name])
	}
	return tfsdk.Config{Schema: sch.Schema, Raw: tftypes.NewValue(typ, full)}
}

func invokeMigrate(t *testing.T, url string, vals map[string]any) (action.InvokeResponse, []string) {
	t.Helper()
	a := &instanceMigrateAction{config: fakeConfig(url)}
	var events []string
	resp := action.InvokeResponse{SendProgress: func(e action.InvokeProgressEvent) { events = append(events, e.Message) }}
	a.Invoke(context.Background(), action.InvokeRequest{Config: instanceMigrateConfig(t, vals)}, &resp)
	return resp, events
}

func TestResolveMigrateMode(t *testing.T) {
	for _, tc := range []struct {
		mode, status, zone string
		live, wantErr      bool
	}{
		{"auto", "ACTIVE", "", true, false},
		{"auto", "PAUSED", "", true, false},
		{"auto", "SHUTOFF", "", false, false},
		{"auto", "SUSPENDED", "", false, false},
		{"auto", "VERIFY_RESIZE", "", false, true},
		{"live", "SHUTOFF", "", false, true},
		{"cold", "ACTIVE", "", false, false}, // allowed; the docs say it restarts the VM
		{"cold", "PAUSED", "", false, true},
		{"auto", "SHUTOFF", "cluster-2", false, false},
		{"auto", "ACTIVE", "cluster-2", false, true}, // the UI refuses a running VM across clusters
		{"live", "SHUTOFF", "cluster-2", false, true},
	} {
		live, err := resolveMigrateMode(tc.mode, tc.status, tc.zone)
		if (err != nil) != tc.wantErr || (err == nil && live != tc.live) {
			t.Errorf("resolveMigrateMode(%s, %s, %q) = %v, %v; want live=%v err=%v", tc.mode, tc.status, tc.zone, live, err, tc.live, tc.wantErr)
		}
	}
}

func TestInstanceMigrateValidateConfig(t *testing.T) {
	for _, tc := range []struct {
		name    string
		vals    map[string]any
		wantErr string
	}{
		{"zone without host", map[string]any{"instance_id": "srv-1", "availability_zone": "cluster-2"}, "host is required with availability_zone"},
		{"zone with live", map[string]any{"instance_id": "srv-1", "availability_zone": "cluster-2", "host": "h", "mode": "live"}, "availability_zone needs a cold migration"},
		{"zone with host", map[string]any{"instance_id": "srv-1", "availability_zone": "cluster-2", "host": "h"}, ""},
		{"unknown zone is left to apply", map[string]any{"instance_id": "srv-1", "availability_zone": tftypes.UnknownValue}, ""},
		{"host only", map[string]any{"instance_id": "srv-1", "host": "h"}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := action.ValidateConfigResponse{}
			(&instanceMigrateAction{}).ValidateConfig(context.Background(), action.ValidateConfigRequest{Config: instanceMigrateConfig(t, tc.vals)}, &resp)
			if tc.wantErr == "" {
				if resp.Diagnostics.HasError() {
					t.Fatalf("diags = %v", resp.Diagnostics)
				}
				return
			}
			if !resp.Diagnostics.HasError() || resp.Diagnostics[0].Summary() != tc.wantErr {
				t.Fatalf("diags = %v; want %q", resp.Diagnostics, tc.wantErr)
			}
		})
	}
}

// The default migrates a running instance live to a scheduler-chosen host
// and reports each phase.
func TestInstanceMigrateLive(t *testing.T) {
	fastServerPolls(t)
	f, nova := newFakeMigrateNova(t, map[string][]fakeServerState{"srv-1": {
		{"ACTIVE", "", "host-a"}, {"MIGRATING", "migrating", "host-a"}, {"ACTIVE", "", "host-b"},
	}})
	resp, events := invokeMigrate(t, nova.URL, map[string]any{"instance_id": "srv-1"})
	if resp.Diagnostics.HasError() {
		t.Fatalf("migrate: %v", resp.Diagnostics)
	}
	if acts := f.actionLog(); len(acts) != 1 || acts[0].name != "os-migrateLive" {
		t.Fatalf("actions = %+v", acts)
	}
	if len(events) < 2 || !strings.Contains(events[len(events)-1], "moved from host-a to host-b") {
		t.Fatalf("progress = %v", events)
	}
}

// confirm = false leaves a cold migration for the operator, with a warning.
func TestInstanceMigrateColdUnconfirmed(t *testing.T) {
	fastServerPolls(t)
	f, nova := newFakeMigrateNova(t, map[string][]fakeServerState{"srv-1": {
		{"SHUTOFF", "", "host-a"}, {"VERIFY_RESIZE", "", "host-b"},
	}})
	resp, _ := invokeMigrate(t, nova.URL, map[string]any{"instance_id": "srv-1", "host": "host-b", "confirm": false})
	if resp.Diagnostics.HasError() || len(resp.Diagnostics) != 1 || resp.Diagnostics[0].Summary() != "Cold migration not confirmed" {
		t.Fatalf("diags = %v", resp.Diagnostics)
	}
	if acts := f.actionLog(); len(acts) != 1 || acts[0].microversion != "compute 2.56" {
		t.Fatalf("actions = %+v; want one targeted migrate at 2.56 and no confirmResize", acts)
	}
}

// With confirm omitted, a cold migration is confirmed: the action sends
// confirmResize once the instance waits in VERIFY_RESIZE, and does not warn.
func TestInstanceMigrateColdConfirmsByDefault(t *testing.T) {
	fastServerPolls(t)
	f, nova := newFakeMigrateNova(t, map[string][]fakeServerState{"srv-1": {
		{"SHUTOFF", "", "host-a"}, {"VERIFY_RESIZE", "", "host-b"}, {"SHUTOFF", "", "host-b"},
	}})
	resp, _ := invokeMigrate(t, nova.URL, map[string]any{"instance_id": "srv-1", "host": "host-b"})
	if resp.Diagnostics.HasError() {
		t.Fatalf("migrate: %v", resp.Diagnostics)
	}
	for _, d := range resp.Diagnostics {
		if d.Summary() == "Cold migration not confirmed" {
			t.Fatalf("diags = %v; an omitted confirm must confirm the migration", resp.Diagnostics)
		}
	}
	acts := f.actionLog()
	if len(acts) != 2 || acts[0].name != "migrate" || acts[1].name != "confirmResize" {
		t.Fatalf("actions = %+v; want the migrate, then confirmResize", acts)
	}
}

// A move to another cluster sends the zone at microversion 2.101, confirms
// the cold migration and succeeds.
func TestInstanceMigrateCrossClusterMoves(t *testing.T) {
	fastServerPolls(t)
	f, nova := newFakeMigrateNova(t, map[string][]fakeServerState{"srv-1": {
		{"SHUTOFF", "", "host-a"}, {"VERIFY_RESIZE", "", "host-b"}, {"SHUTOFF", "", "host-b"},
	}})
	resp, _ := invokeMigrate(t, nova.URL, map[string]any{"instance_id": "srv-1", "host": "host-b", "availability_zone": "cluster-2"})
	if resp.Diagnostics.HasError() || len(resp.Diagnostics) != 0 {
		t.Fatalf("diags = %v; want a clean move", resp.Diagnostics)
	}
	acts := f.actionLog()
	if len(acts) != 2 || acts[0].microversion != "compute 2.101" ||
		bodyJSON(t, acts[0].body) != `{"migrate":{"availability_zone":"cluster-2","host":"host-b"}}` ||
		acts[1].name != "confirmResize" {
		t.Fatalf("actions = %+v; want the migrate with the zone at 2.101, then confirmResize", acts)
	}
}

// A 200 that holds no server is reported as an empty answer, not as a token
// that cannot see the instance's host, and nothing is sent.
func TestInstanceMigrateRefusesEmptyServer(t *testing.T) {
	f, nova := newFakeMigrateNova(t, nil)
	f.other = func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method != "GET" || r.URL.Path != "/servers/srv-1" {
			return false
		}
		_, _ = w.Write([]byte(`{"server": {}}`))
		return true
	}
	resp, _ := invokeMigrate(t, nova.URL, map[string]any{"instance_id": "srv-1"})
	if !resp.Diagnostics.HasError() {
		t.Fatal("migrate succeeded; want the empty answer reported")
	}
	detail := resp.Diagnostics[0].Detail()
	if !strings.Contains(detail, clients.ErrNoObject.Error()) || strings.Contains(detail, "admin token") {
		t.Fatalf("diags = %v; want %q, not the admin-token error", resp.Diagnostics, clients.ErrNoObject)
	}
	if acts := f.actionLog(); len(acts) != 0 {
		t.Fatalf("actions = %+v; nothing may be sent", acts)
	}
}

// An availability_zone unknown at validation (a cluster made in the same
// apply) skips ValidateConfig's host check, so Invoke refuses a zone without
// a host before sending anything: a plain cold migration would stay in the
// instance's cluster and report success.
func TestInstanceMigrateZoneNeedsHostAtInvoke(t *testing.T) {
	f, nova := newFakeMigrateNova(t, map[string][]fakeServerState{"srv-1": {{"SHUTOFF", "", "host-a"}}})
	resp, _ := invokeMigrate(t, nova.URL, map[string]any{"instance_id": "srv-1", "availability_zone": "cluster-2"})
	if !resp.Diagnostics.HasError() || resp.Diagnostics[0].Summary() != "host is required with availability_zone" {
		t.Fatalf("diags = %v; want the missing host reported", resp.Diagnostics)
	}
	if acts := f.actionLog(); len(acts) != 0 {
		t.Fatalf("actions = %+v; nothing may be sent without a host", acts)
	}
}

// A running instance cannot move to another cluster; nothing is sent.
func TestInstanceMigrateRefusesRunningCrossCluster(t *testing.T) {
	f, nova := newFakeMigrateNova(t, map[string][]fakeServerState{"srv-1": {{"ACTIVE", "", "host-a"}}})
	resp, _ := invokeMigrate(t, nova.URL, map[string]any{"instance_id": "srv-1", "host": "host-c", "availability_zone": "cluster-2"})
	if !resp.Diagnostics.HasError() || len(f.actionLog()) != 0 {
		t.Fatalf("diags=%v actions=%v", resp.Diagnostics, f.actionLog())
	}
}

// Nova's synchronous refusal of the current host reaches the user as is.
func TestInstanceMigrateSameHost(t *testing.T) {
	f, nova := newFakeMigrateNova(t, map[string][]fakeServerState{"srv-1": {{"SHUTOFF", "", "host-a"}}})
	f.actionStatus["migrate"] = 400
	f.actionBody["migrate"] = `{"badRequest": {"code": 400, "message": "The target host can't be the same one."}}`
	resp, _ := invokeMigrate(t, nova.URL, map[string]any{"instance_id": "srv-1", "host": "host-a"})
	if !resp.Diagnostics.HasError() || resp.Diagnostics[0].Summary() != "Migration failed" ||
		!strings.Contains(resp.Diagnostics[0].Detail(), "can't be the same one") {
		t.Fatalf("diags = %v", resp.Diagnostics)
	}
}
