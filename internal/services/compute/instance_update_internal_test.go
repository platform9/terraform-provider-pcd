// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package compute

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/servers"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"

	"github.com/platform9/terraform-provider-pcd/internal/clients"
)

// novaState is what GET /servers/srv-1 reports: a status and a task state
// ("" is null, no task in flight).
type novaState struct{ status, task string }

// instanceNova models one server, srv-1, closely enough for Update: server
// actions move it the way Nova does, first through the in-flight state Nova
// reports while the task runs, then to the end state. It records every server
// action in order and fails the test on any request it does not know.
type instanceNova struct {
	t  *testing.T
	mu sync.Mutex

	name      string
	current   novaState
	inFlight  []novaState // reported one per GET before current
	preResize string      // the status a resize started from
	secgroups []string

	// failAction answers the named server action with this HTTP status
	// instead of applying it.
	failAction map[string]int
	// revert names server actions Nova accepts and then fails: the task runs
	// and the instance settles back in its old status, as after a resize the
	// scheduler cannot place or a power action the hypervisor refuses.
	revert map[string]bool
	// autoConfirm makes a resize end as on a cloud that sets
	// resize_confirm_window and confirms it between two polls: back in the old
	// status, on the new flavor.
	autoConfirm bool
	flavor      string // the flavor ID GET reports

	actions    []string       // "os-stop", "resize", "addSecurityGroup web", ...
	createBody map[string]any // the decoded POST /servers body
}

// newInstanceNova starts an instanceNova whose srv-1 is in status and returns
// it with an instanceResource that reaches it through fakeConfig (from
// fake_nova_internal_test.go).
func newInstanceNova(t *testing.T, status string) (*instanceNova, *instanceResource) {
	t.Helper()
	f := &instanceNova{t: t, name: "vm-1", current: novaState{status: status}, secgroups: []string{"default"}, failAction: map[string]int{},
		revert: map[string]bool{}, flavor: "flv-1"}
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)
	return f, &instanceResource{config: fakeConfig(srv.URL)}
}

func (f *instanceNova) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch r.Method + " " + r.URL.Path {
	case "GET /servers/srv-1":
		s := f.current
		if len(f.inFlight) > 0 {
			s, f.inFlight = f.inFlight[0], f.inFlight[1:]
		}
		f.writeServer(w, s)
	case "POST /servers":
		_ = json.NewDecoder(r.Body).Decode(&f.createBody)
		f.current = novaState{status: "ACTIVE"}
		w.WriteHeader(http.StatusAccepted)
		fmt.Fprint(w, `{"server": {"id": "srv-1"}}`)
	case "PUT /servers/srv-1":
		var body struct {
			Server struct {
				Name string `json:"name"`
			} `json:"server"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.name = body.Server.Name
		f.writeServer(w, f.current)
	case "POST /servers/srv-1/action":
		var body map[string]json.RawMessage
		_ = json.NewDecoder(r.Body).Decode(&body)
		for name, arg := range body {
			w.WriteHeader(f.act(name, arg))
		}
	default:
		f.t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusNotImplemented)
	}
}

func (f *instanceNova) writeServer(w http.ResponseWriter, s novaState) {
	task := "null"
	if s.task != "" {
		task = fmt.Sprintf("%q", s.task)
	}
	sgs := make([]string, 0, len(f.secgroups))
	for _, g := range f.secgroups {
		sgs = append(sgs, fmt.Sprintf(`{"name": %q}`, g))
	}
	fmt.Fprintf(w, `{"server": {"id": "srv-1", "name": %q, "status": %q, "OS-EXT-STS:task_state": %s,
		"flavor": {"id": %q}, "metadata": {}, "addresses": {}, "OS-EXT-AZ:availability_zone": "nova",
		"security_groups": [%s]}}`, f.name, s.status, task, f.flavor, strings.Join(sgs, ", "))
}

// act applies one server action and returns the HTTP status Nova answers.
func (f *instanceNova) act(name string, arg json.RawMessage) int {
	label := name
	if name == "addSecurityGroup" || name == "removeSecurityGroup" {
		var sg struct {
			Name string `json:"name"`
		}
		_ = json.Unmarshal(arg, &sg)
		label = name + " " + sg.Name
	}
	f.actions = append(f.actions, label)
	if code, ok := f.failAction[name]; ok {
		return code
	}
	old := f.current.status
	move := func(during novaState, end string) int {
		if f.revert[name] {
			end = old
		}
		f.inFlight = append(f.inFlight, during)
		f.current = novaState{status: end}
		return http.StatusAccepted
	}
	switch name {
	case "os-start":
		return move(novaState{old, "powering-on"}, "ACTIVE")
	case "os-stop":
		return move(novaState{old, "powering-off"}, "SHUTOFF")
	case "pause":
		return move(novaState{old, "pausing"}, "PAUSED")
	case "unpause":
		return move(novaState{old, "unpausing"}, "ACTIVE")
	case "suspend":
		return move(novaState{old, "suspending"}, "SUSPENDED")
	case "resume":
		return move(novaState{old, "resuming"}, "ACTIVE")
	case "reboot":
		return move(novaState{old, "rebooting"}, "ACTIVE")
	case "resize":
		// PCD's Nova resizes only ACTIVE and SHUTOFF instances.
		if old == "PAUSED" || old == "SUSPENDED" {
			return http.StatusConflict
		}
		f.preResize = old
		if f.autoConfirm && !f.revert[name] {
			var opts struct {
				FlavorRef string `json:"flavorRef"`
			}
			_ = json.Unmarshal(arg, &opts)
			f.flavor = opts.FlavorRef
			return move(novaState{"RESIZE", "resize_prep"}, old)
		}
		return move(novaState{"RESIZE", "resize_prep"}, "VERIFY_RESIZE")
	case "confirmResize":
		// Nova answers before the source host finishes: VERIFY_RESIZE with
		// no task, then the status the instance had before the resize.
		f.inFlight = append(f.inFlight, novaState{status: "VERIFY_RESIZE"})
		f.current = novaState{status: f.preResize}
		return http.StatusNoContent
	case "revertResize":
		return move(novaState{"REVERT_RESIZE", "resize_reverting"}, f.preResize)
	case "addSecurityGroup":
		g := strings.TrimPrefix(label, name+" ")
		if !slices.Contains(f.secgroups, g) {
			f.secgroups = append(f.secgroups, g)
		}
		return http.StatusAccepted
	case "removeSecurityGroup":
		g := strings.TrimPrefix(label, name+" ")
		i := slices.Index(f.secgroups, g)
		if i < 0 {
			return http.StatusNotFound
		}
		f.secgroups = slices.Delete(f.secgroups, i, i+1)
		return http.StatusAccepted
	}
	f.t.Errorf("unexpected server action %s", name)
	return http.StatusNotImplemented
}

func (f *instanceNova) recorded() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.actions)
}

// instanceTestModel is an instance as Read leaves it in state: booted from
// img-1 on flavor flv-1 with one NIC on net-1 and the default security group.
func instanceTestModel(t *testing.T, r *instanceResource, status string) instanceModel {
	t.Helper()
	ctx := context.Background()
	s := instanceTestSchema(r)
	nested := func(block string) types.ObjectType {
		return s.Blocks[block].(schema.ListNestedBlock).NestedObject.Type().(types.ObjectType)
	}
	network, d := types.ListValueFrom(ctx, nested("network"), []instanceNetworkModel{
		{UUID: types.StringValue("net-1"), Name: types.StringNull(), Port: types.StringValue("")},
	})
	if d.HasError() {
		t.Fatalf("network: %v", d)
	}
	sgs, d := types.SetValueFrom(ctx, types.StringType, []string{"default"})
	if d.HasError() {
		t.Fatalf("security_groups: %v", d)
	}
	meta, d := types.MapValueFrom(ctx, types.StringType, map[string]string{})
	if d.HasError() {
		t.Fatalf("metadata: %v", d)
	}
	return instanceModel{
		ID:                types.StringValue("srv-1"),
		Name:              types.StringValue("vm-1"),
		ImageID:           types.StringValue("img-1"),
		ImageName:         types.StringNull(),
		FlavorID:          types.StringValue("flv-1"),
		FlavorName:        types.StringNull(),
		KeyPair:           types.StringNull(),
		SecurityGroups:    sgs,
		Network:           network,
		BlockDevice:       types.ListNull(nested("block_device")),
		SchedulerHints:    types.ListNull(nested("scheduler_hints")),
		Metadata:          meta,
		MigrationPriority: types.StringValue(""),
		UserData:          types.StringNull(),
		AvailabilityZone:  types.StringValue("nova"),
		ConfigDrive:       types.BoolNull(),
		AccessIPv4:        types.StringValue(""),
		Status:            types.StringValue(status),
		PowerState:        testPowerState(status),
		Region:            types.StringValue("region-one"),
	}
}

// testPowerState is the power_state Read leaves in state for a status.
func testPowerState(status string) types.String {
	if ps, ok := powerStateFromStatus(status); ok {
		return types.StringValue(ps)
	}
	return types.StringNull()
}

func instanceTestSchema(r *instanceResource) schema.Schema {
	var sch resource.SchemaResponse
	r.Schema(context.Background(), resource.SchemaRequest{}, &sch)
	return sch.Schema
}

// runInstanceUpdate calls Update the way the framework does: the response
// state starts as the prior state, so an early return keeps it. Unlike
// runUpdate in fake_nova_internal_test.go, it takes and returns instance
// models and gives Update a deadline.
func runInstanceUpdate(t *testing.T, r *instanceResource, state, plan instanceModel) (instanceModel, *resource.UpdateResponse) {
	t.Helper()
	// A wait that never ends fails the test in seconds instead of at the
	// package timeout.
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	s := instanceTestSchema(r)
	prior := newState(t, s, &state)
	resp := &resource.UpdateResponse{State: tfsdk.State{Schema: s, Raw: prior.Raw.Copy()}}
	r.Update(ctx, resource.UpdateRequest{Plan: newPlan(t, s, &plan), State: prior}, resp)
	var got instanceModel
	if d := resp.State.Get(ctx, &got); d.HasError() {
		t.Fatalf("reading the new state: %v", d)
	}
	return got, resp
}

// A resize brings an instance back in the status it had before: Nova keeps a
// stopped instance stopped after the confirm. Waiting for ACTIVE there polled
// for 30 minutes and then failed an apply that had worked.
func TestUpdateResizeWaitsForThePreResizeStatus(t *testing.T) {
	for _, status := range []string{"ACTIVE", "SHUTOFF"} {
		t.Run(status, func(t *testing.T) {
			shortPolls(t)
			nova, r := newInstanceNova(t, status)
			state := instanceTestModel(t, r, status)
			plan := state
			plan.FlavorID = types.StringValue("flv-2")
			plan.Status = types.StringUnknown()

			got, resp := runInstanceUpdate(t, r, state, plan)
			if resp.Diagnostics.HasError() {
				t.Fatalf("resize of a %s instance failed: %v", status, resp.Diagnostics)
			}
			if want := []string{"resize", "confirmResize"}; !slices.Equal(nova.recorded(), want) {
				t.Fatalf("server actions = %v, want %v", nova.recorded(), want)
			}
			if got.FlavorID.ValueString() != "flv-2" || got.Status.ValueString() != status {
				t.Fatalf("state after resize: flavor_id=%s status=%s, want flv-2 and %s", got.FlavorID, got.Status, status)
			}
		})
	}
}

// Nova accepts a resize and only then schedules it; when no host fits, the
// instance settles back in its old status with no task. The apply must say
// so at once instead of waiting 30 minutes for VERIFY_RESIZE, and must not
// confirm or revert a resize that never happened.
func TestUpdateResizeRejectedAfterAcceptanceFailsFast(t *testing.T) {
	shortPolls(t)
	nova, r := newInstanceNova(t, "ACTIVE")
	nova.revert["resize"] = true
	state := instanceTestModel(t, r, "ACTIVE")
	plan := state
	plan.FlavorID = types.StringValue("flv-2")
	plan.Status = types.StringUnknown()

	got, resp := runInstanceUpdate(t, r, state, plan)
	if !resp.Diagnostics.HasError() {
		t.Fatal("update succeeded; want the failed resize reported")
	}
	if d := resp.Diagnostics.Errors()[0]; d.Summary() != "compute: resizing instance" || !strings.Contains(d.Detail(), "without resizing it") {
		t.Fatalf("diagnostic = %q: %q", d.Summary(), d.Detail())
	}
	if want := []string{"resize"}; !slices.Equal(nova.recorded(), want) {
		t.Fatalf("server actions = %v, want only %v", nova.recorded(), want)
	}
	if got.FlavorID.ValueString() != "flv-1" {
		t.Fatalf("state flavor_id = %s; a failed resize must keep the prior state", got.FlavorID)
	}
}

// A cloud that sets resize_confirm_window confirms a resize itself and can
// do so between two polls: the instance is then back in its old status on
// the new flavor. That resize is done; it must not be reported as failed or
// confirmed a second time.
func TestUpdateResizeAutoConfirmedByNova(t *testing.T) {
	shortPolls(t)
	nova, r := newInstanceNova(t, "ACTIVE")
	nova.autoConfirm = true
	state := instanceTestModel(t, r, "ACTIVE")
	plan := state
	plan.FlavorID = types.StringValue("flv-2")
	plan.Status = types.StringUnknown()

	got, resp := runInstanceUpdate(t, r, state, plan)
	if resp.Diagnostics.HasError() {
		t.Fatalf("update: %v", resp.Diagnostics)
	}
	if want := []string{"resize"}; !slices.Equal(nova.recorded(), want) {
		t.Fatalf("server actions = %v, want only %v", nova.recorded(), want)
	}
	if got.FlavorID.ValueString() != "flv-2" || got.Status.ValueString() != "ACTIVE" {
		t.Fatalf("state after resize: flavor_id=%s status=%s, want flv-2 and ACTIVE", got.FlavorID, got.Status)
	}
}

// Nova reports only these four statuses as steady power states; anything
// else is a transition or another mode, which power_state does not model.
func TestPowerStateFromStatus(t *testing.T) {
	for status, want := range map[string]string{
		"ACTIVE": "active", "SHUTOFF": "shutoff", "PAUSED": "paused", "SUSPENDED": "suspended",
		"BUILD": "", "REBOOT": "", "HARD_REBOOT": "", "RESIZE": "", "VERIFY_RESIZE": "", "REVERT_RESIZE": "",
		"MIGRATING": "", "RESCUE": "", "ERROR": "", "SHELVED": "", "SHELVED_OFFLOADED": "", "REBUILD": "",
		"PASSWORD": "", "DELETED": "", "SOFT_DELETED": "", "UNKNOWN": "",
	} {
		got, ok := powerStateFromStatus(status)
		if got != want || ok != (want != "") {
			t.Errorf("powerStateFromStatus(%q) = %q, %v; want %q", status, got, ok, want)
		}
	}
}

// power_state is unmanaged when omitted (no default) and must never be
// planned from stale state (no UseStateForUnknown): either would let an
// apply undo a power change made in the UI.
func TestPowerStateSchemaIsUnmanagedWhenOmitted(t *testing.T) {
	ps, ok := instanceTestSchema(&instanceResource{}).Attributes["power_state"].(schema.StringAttribute)
	if !ok {
		t.Fatal("power_state is missing from the schema")
	}
	if !ps.Optional || !ps.Computed || ps.Default != nil || len(ps.PlanModifiers) != 0 {
		t.Fatalf("power_state: optional=%v computed=%v default=%v plan modifiers=%d; want Optional+Computed, no default, no plan modifier",
			ps.Optional, ps.Computed, ps.Default, len(ps.PlanModifiers))
	}
}

// Every transition goes through ACTIVE, because Nova starts only a stopped
// instance, unpauses only a paused one, resumes only a suspended one, and
// stops, pauses or suspends only an active one.
func TestUpdateMovesThroughActive(t *testing.T) {
	cases := []struct {
		live, target string
		want         []string
		end          string
	}{
		{"SHUTOFF", "active", []string{"os-start"}, "ACTIVE"},
		{"ACTIVE", "shutoff", []string{"os-stop"}, "SHUTOFF"},
		{"ACTIVE", "paused", []string{"pause"}, "PAUSED"},
		{"ACTIVE", "suspended", []string{"suspend"}, "SUSPENDED"},
		{"PAUSED", "active", []string{"unpause"}, "ACTIVE"},
		{"SUSPENDED", "active", []string{"resume"}, "ACTIVE"},
		{"PAUSED", "shutoff", []string{"unpause", "os-stop"}, "SHUTOFF"},
		{"SUSPENDED", "paused", []string{"resume", "pause"}, "PAUSED"},
		{"SHUTOFF", "suspended", []string{"os-start", "suspend"}, "SUSPENDED"},
		{"ACTIVE", "active", nil, "ACTIVE"},
	}
	for _, tc := range cases {
		t.Run(tc.live+"->"+tc.target, func(t *testing.T) {
			shortPolls(t)
			nova, r := newInstanceNova(t, tc.live)
			state := instanceTestModel(t, r, tc.live)
			plan := state
			plan.PowerState = types.StringValue(tc.target)
			plan.Status = types.StringUnknown()

			got, resp := runInstanceUpdate(t, r, state, plan)
			if resp.Diagnostics.HasError() {
				t.Fatalf("update: %v", resp.Diagnostics)
			}
			if !slices.Equal(nova.recorded(), tc.want) {
				t.Fatalf("server actions = %v, want %v", nova.recorded(), tc.want)
			}
			if got.Status.ValueString() != tc.end || got.PowerState.ValueString() != tc.target {
				t.Fatalf("state status=%s power_state=%s, want %s and %s", got.Status, got.PowerState, tc.end, tc.target)
			}
		})
	}
}

// Update compares the plan with the live status, not with state: under
// -refresh=false, state can still say shutoff after the instance was started
// in the UI, and a configured power_state = "shutoff" must stop it again.
func TestUpdateUsesTheLiveStatus(t *testing.T) {
	shortPolls(t)
	nova, r := newInstanceNova(t, "ACTIVE")
	state := instanceTestModel(t, r, "SHUTOFF") // stale
	plan := state
	plan.Name = types.StringValue("vm-renamed")
	plan.Status = types.StringUnknown()

	got, resp := runInstanceUpdate(t, r, state, plan)
	if resp.Diagnostics.HasError() {
		t.Fatalf("update: %v", resp.Diagnostics)
	}
	if want := []string{"os-stop"}; !slices.Equal(nova.recorded(), want) {
		t.Fatalf("server actions = %v, want %v", nova.recorded(), want)
	}
	if got.PowerState.ValueString() != "shutoff" || got.Name.ValueString() != "vm-renamed" {
		t.Fatalf("state power_state=%s name=%s", got.PowerState, got.Name)
	}
}

// With power_state not configured, Terraform plans it unknown whenever the
// instance changes. Update must then leave the power state alone and report
// whatever the instance is in, so an instance stopped in the UI stays stopped
// through a rename.
func TestUpdateLeavesAnUnmanagedPowerStateAlone(t *testing.T) {
	shortPolls(t)
	nova, r := newInstanceNova(t, "SHUTOFF")
	state := instanceTestModel(t, r, "SHUTOFF")
	plan := state
	plan.Name = types.StringValue("vm-renamed")
	plan.PowerState = types.StringUnknown()
	plan.Status = types.StringUnknown()

	got, resp := runInstanceUpdate(t, r, state, plan)
	if resp.Diagnostics.HasError() {
		t.Fatalf("update: %v", resp.Diagnostics)
	}
	if len(nova.recorded()) != 0 {
		t.Fatalf("server actions = %v; an unconfigured power_state must not touch the power state", nova.recorded())
	}
	if got.PowerState.ValueString() != "shutoff" || got.Status.ValueString() != "SHUTOFF" {
		t.Fatalf("state power_state=%s status=%s, want shutoff and SHUTOFF", got.PowerState, got.Status)
	}
}

// An instance in ERROR, RESCUE or a transition has no power state to move
// from; Update must say so instead of sending Nova an action it rejects.
func TestUpdateRefusesToMovePowerFromAnUnsteadyStatus(t *testing.T) {
	shortPolls(t)
	nova, r := newInstanceNova(t, "ERROR")
	state := instanceTestModel(t, r, "ERROR")
	state.PowerState = types.StringValue("shutoff")
	plan := state
	plan.PowerState = types.StringValue("active")

	_, resp := runInstanceUpdate(t, r, state, plan)
	if !resp.Diagnostics.HasError() || resp.Diagnostics.Errors()[0].Summary() != "compute: setting power_state" {
		t.Fatalf("diagnostics = %v, want compute: setting power_state", resp.Diagnostics)
	}
	if len(nova.recorded()) != 0 {
		t.Fatalf("server actions = %v, want none", nova.recorded())
	}
}

// Power on runs before the other changes and power off, pause or suspend
// after them; a paused or suspended instance is brought up for a resize,
// which PCD's Nova refuses on either, and put back when power_state is not
// configured or asks for the state it was in.
func TestUpdateOrdersPowerStepsAroundAResize(t *testing.T) {
	cases := []struct {
		name, live string
		target     types.String
		want       []string
		end        string
	}{
		{"start before resize", "SHUTOFF", types.StringValue("active"), []string{"os-start", "resize", "confirmResize"}, "ACTIVE"},
		{"stop after resize", "ACTIVE", types.StringValue("shutoff"), []string{"resize", "confirmResize", "os-stop"}, "SHUTOFF"},
		{"resize a stopped instance in place", "SHUTOFF", types.StringValue("shutoff"), []string{"resize", "confirmResize"}, "SHUTOFF"},
		{"unmanaged paused instance is unpaused and paused again", "PAUSED", types.StringUnknown(), []string{"unpause", "resize", "confirmResize", "pause"}, "PAUSED"},
		{"paused instance unpaused, resized, then stopped", "PAUSED", types.StringValue("shutoff"), []string{"unpause", "resize", "confirmResize", "os-stop"}, "SHUTOFF"},
		{"unmanaged suspended instance is resumed and suspended again", "SUSPENDED", types.StringUnknown(), []string{"resume", "resize", "confirmResize", "suspend"}, "SUSPENDED"},
		{"configured suspended instance stays suspended", "SUSPENDED", types.StringValue("suspended"), []string{"resume", "resize", "confirmResize", "suspend"}, "SUSPENDED"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			shortPolls(t)
			nova, r := newInstanceNova(t, tc.live)
			state := instanceTestModel(t, r, tc.live)
			plan := state
			plan.FlavorID = types.StringValue("flv-2")
			plan.PowerState = tc.target
			plan.Status = types.StringUnknown()

			got, resp := runInstanceUpdate(t, r, state, plan)
			if resp.Diagnostics.HasError() {
				t.Fatalf("update: %v", resp.Diagnostics)
			}
			if !slices.Equal(nova.recorded(), tc.want) {
				t.Fatalf("server actions = %v, want %v", nova.recorded(), tc.want)
			}
			if got.Status.ValueString() != tc.end || got.FlavorID.ValueString() != "flv-2" {
				t.Fatalf("state status=%s flavor_id=%s, want %s and flv-2", got.Status, got.FlavorID, tc.end)
			}
		})
	}
}

// A resize that fails after Update brought a paused or suspended instance up
// for it puts the instance back the way it was found before the apply fails.
func TestUpdateRestoresThePowerStateWhenAResizeFails(t *testing.T) {
	for _, tc := range []struct {
		live string
		want []string
	}{
		{"PAUSED", []string{"unpause", "resize", "pause"}},
		{"SUSPENDED", []string{"resume", "resize", "suspend"}},
	} {
		t.Run(tc.live, func(t *testing.T) {
			shortPolls(t)
			nova, r := newInstanceNova(t, tc.live)
			nova.revert["resize"] = true
			state := instanceTestModel(t, r, tc.live)
			plan := state
			plan.FlavorID = types.StringValue("flv-2")
			plan.PowerState = types.StringUnknown()
			plan.Status = types.StringUnknown()

			_, resp := runInstanceUpdate(t, r, state, plan)
			if !resp.Diagnostics.HasError() || resp.Diagnostics.Errors()[0].Summary() != "compute: resizing instance" {
				t.Fatalf("diagnostics = %v, want the failed resize reported", resp.Diagnostics)
			}
			if !slices.Equal(nova.recorded(), tc.want) {
				t.Fatalf("server actions = %v, want %v", nova.recorded(), tc.want)
			}
		})
	}
}

// A status that is no power state (here a migration that PCD started on its
// own) blocks only a power_state change. With power_state unchanged, a rename
// goes through and the power state is left for a later apply.
func TestUpdateLeavesAnUnchangedPowerStateAloneWhileUnsteady(t *testing.T) {
	shortPolls(t)
	nova, r := newInstanceNova(t, "MIGRATING")
	state := instanceTestModel(t, r, "ACTIVE")
	plan := state
	plan.Name = types.StringValue("vm-renamed")
	plan.Status = types.StringUnknown()

	got, resp := runInstanceUpdate(t, r, state, plan)
	if resp.Diagnostics.HasError() {
		t.Fatalf("update: %v", resp.Diagnostics)
	}
	if len(nova.recorded()) != 0 {
		t.Fatalf("server actions = %v, want none", nova.recorded())
	}
	if got.Name.ValueString() != "vm-renamed" || got.PowerState.ValueString() != "active" || got.Status.ValueString() != "MIGRATING" {
		t.Fatalf("state name=%s power_state=%s status=%s, want vm-renamed, active and MIGRATING", got.Name, got.PowerState, got.Status)
	}
}

// Nova clears the task and keeps the old state when a power action it
// accepted fails in the hypervisor. setPowerState must report that at once
// instead of polling for the target status until its 30-minute timeout.
func TestSetPowerStateReportsAnActionNovaReverted(t *testing.T) {
	for _, tc := range []struct {
		action, live, target string
	}{
		{"os-stop", "ACTIVE", "shutoff"},
		{"os-start", "SHUTOFF", "active"},
		{"pause", "ACTIVE", "paused"},
		{"unpause", "PAUSED", "active"},
		{"suspend", "ACTIVE", "suspended"},
		{"resume", "SUSPENDED", "active"},
	} {
		t.Run(tc.action, func(t *testing.T) {
			shortPolls(t)
			nova, r := newInstanceNova(t, tc.live)
			nova.revert[tc.action] = true
			client, err := r.config.ComputeV2Client()
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			if _, err := setPowerState(ctx, client, "srv-1", tc.live, tc.target); err == nil || !strings.Contains(err.Error(), "event list") {
				t.Fatalf("error = %v, want the failed %s reported", err, tc.action)
			}
			if want := []string{tc.action}; !slices.Equal(nova.recorded(), want) {
				t.Fatalf("server actions = %v, want %v", nova.recorded(), want)
			}
		})
	}
}

// runInstanceCreate calls Create the way the framework does, with an empty
// state, and gives it a deadline.
func runInstanceCreate(t *testing.T, r *instanceResource, plan instanceModel) (instanceModel, *resource.CreateResponse) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	s := instanceTestSchema(r)
	resp := &resource.CreateResponse{State: tfsdk.State{Schema: s, Raw: tftypes.NewValue(s.Type().TerraformType(ctx), nil)}}
	r.Create(ctx, resource.CreateRequest{Plan: newPlan(t, s, &plan)}, resp)
	var got instanceModel
	if !resp.State.Raw.IsNull() {
		if d := resp.State.Get(ctx, &got); d.HasError() {
			t.Fatalf("reading the new state: %v", d)
		}
	}
	return got, resp
}

// instanceCreatePlan is the plan for a new instance: what the provider
// computes is unknown.
func instanceCreatePlan(t *testing.T, r *instanceResource, powerState types.String) instanceModel {
	t.Helper()
	plan := instanceTestModel(t, r, "ACTIVE")
	plan.ID = types.StringUnknown()
	plan.SecurityGroups = types.SetUnknown(types.StringType)
	plan.Metadata = types.MapUnknown(types.StringType)
	plan.MigrationPriority = types.StringUnknown()
	plan.AvailabilityZone = types.StringUnknown()
	plan.AccessIPv4 = types.StringUnknown()
	plan.Status = types.StringUnknown()
	plan.Region = types.StringUnknown()
	plan.PowerState = powerState
	return plan
}

// Nova boots every instance running: a configured shutoff, paused or
// suspended state is applied once the instance is ACTIVE; an unconfigured one
// reads back as active.
func TestCreateAppliesPowerState(t *testing.T) {
	cases := []struct {
		name  string
		plan  types.String
		want  []string
		state string
	}{
		{"unconfigured", types.StringUnknown(), nil, "active"},
		{"active", types.StringValue("active"), nil, "active"},
		{"shutoff", types.StringValue("shutoff"), []string{"os-stop"}, "shutoff"},
		{"paused", types.StringValue("paused"), []string{"pause"}, "paused"},
		{"suspended", types.StringValue("suspended"), []string{"suspend"}, "suspended"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			shortPolls(t)
			nova, r := newInstanceNova(t, "ACTIVE")
			got, resp := runInstanceCreate(t, r, instanceCreatePlan(t, r, tc.plan))
			if resp.Diagnostics.HasError() {
				t.Fatalf("create: %v", resp.Diagnostics)
			}
			if !slices.Equal(nova.recorded(), tc.want) {
				t.Fatalf("server actions = %v, want %v", nova.recorded(), tc.want)
			}
			if got.PowerState.ValueString() != tc.state {
				t.Fatalf("state power_state = %s, want %s", got.PowerState, tc.state)
			}
		})
	}
}

// A status that is not a power state keeps the last known power_state, so a
// reboot or a rescue in flight shows no drift; with nothing known (import,
// or a create that ended in such a status) it is null, never unknown.
func TestFlattenPowerState(t *testing.T) {
	r := &instanceResource{config: &clients.Config{Region: "region-one"}}
	for _, tc := range []struct {
		status string
		prior  types.String
		want   types.String
	}{
		{"SUSPENDED", types.StringValue("active"), types.StringValue("suspended")},
		{"REBOOT", types.StringValue("shutoff"), types.StringValue("shutoff")},
		{"RESCUE", types.StringValue("active"), types.StringValue("active")},
		{"REBOOT", types.StringUnknown(), types.StringNull()},
		{"ERROR", types.StringNull(), types.StringNull()},
	} {
		m := instanceTestModel(t, r, "ACTIVE")
		m.PowerState = tc.prior
		srv := &servers.Server{ID: "srv-1", Name: "vm-1", Status: tc.status}
		if d := r.flatten(context.Background(), srv, &m); d.HasError() {
			t.Fatalf("flatten: %v", d)
		}
		if !m.PowerState.Equal(tc.want) {
			t.Errorf("status %s with prior %s: power_state = %s, want %s", tc.status, tc.prior, m.PowerState, tc.want)
		}
	}
}

// A step that fails after a resize went through (a suspend refused after an
// upsize, say) leaves state on the old flavor. The next apply finds the
// instance already on the new one and must not ask Nova for a resize to the
// flavor it has, which Nova refuses every time.
func TestUpdateSkipsAResizeTheInstanceAlreadyHas(t *testing.T) {
	shortPolls(t)
	nova, r := newInstanceNova(t, "ACTIVE")
	nova.flavor = "flv-2"
	state := instanceTestModel(t, r, "ACTIVE")
	plan := state
	plan.FlavorID = types.StringValue("flv-2")
	plan.Status = types.StringUnknown()

	got, resp := runInstanceUpdate(t, r, state, plan)
	if resp.Diagnostics.HasError() {
		t.Fatalf("update: %v", resp.Diagnostics)
	}
	if len(nova.recorded()) != 0 {
		t.Fatalf("server actions = %v, want none", nova.recorded())
	}
	if got.FlavorID.ValueString() != "flv-2" {
		t.Fatalf("state flavor_id = %s, want flv-2", got.FlavorID)
	}
}

func TestSecurityGroupDelta(t *testing.T) {
	for _, tc := range []struct {
		name                string
		have, want          []string
		wantAdd, wantRemove []string
	}{
		{"no change", []string{"default", "web"}, []string{"web", "default"}, nil, nil},
		{"add only", []string{"default"}, []string{"default", "web", "db"}, []string{"db", "web"}, nil},
		{"remove only", []string{"default", "web", "db"}, []string{"default"}, nil, []string{"db", "web"}},
		{"swap", []string{"default", "web1"}, []string{"default", "web2"}, []string{"web2"}, []string{"web1"}},
		{"duplicates collapse", []string{"a", "a"}, []string{"b", "b"}, []string{"b"}, []string{"a"}},
		{"from nothing", nil, []string{"default"}, []string{"default"}, nil},
	} {
		add, remove := securityGroupDelta(tc.have, tc.want)
		if !slices.Equal(add, tc.wantAdd) || !slices.Equal(remove, tc.wantRemove) {
			t.Errorf("%s: add=%v remove=%v, want add=%v remove=%v", tc.name, add, remove, tc.wantAdd, tc.wantRemove)
		}
	}
}

// security_groups used to force a new instance, so swapping a group, or a
// group added in the UI, destroyed the VM. It now updates in place, adding
// the new group before removing the old one.
func TestUpdateChangesSecurityGroupsInPlace(t *testing.T) {
	shortPolls(t)
	nova, r := newInstanceNova(t, "ACTIVE")
	nova.secgroups = []string{"default", "web1"}
	state := instanceTestModel(t, r, "ACTIVE")
	state.SecurityGroups = types.SetValueMust(types.StringType, []attr.Value{types.StringValue("default"), types.StringValue("web1")})
	plan := state
	plan.SecurityGroups = types.SetValueMust(types.StringType, []attr.Value{types.StringValue("default"), types.StringValue("web2")})

	got, resp := runInstanceUpdate(t, r, state, plan)
	if resp.Diagnostics.HasError() {
		t.Fatalf("update: %v", resp.Diagnostics)
	}
	if want := []string{"addSecurityGroup web2", "removeSecurityGroup web1"}; !slices.Equal(nova.recorded(), want) {
		t.Fatalf("server actions = %v, want %v", nova.recorded(), want)
	}
	var sgs []string
	got.SecurityGroups.ElementsAs(context.Background(), &sgs, false)
	slices.Sort(sgs)
	if !slices.Equal(sgs, []string{"default", "web2"}) {
		t.Fatalf("state security_groups = %v, want [default web2]", sgs)
	}
}

// Nova answers 404 when no port has the group any more, for example when it
// was removed in the UI after Update read the instance; that is the outcome
// the apply wanted.
func TestUpdateSecurityGroupRemovalToleratesNotFound(t *testing.T) {
	shortPolls(t)
	nova, r := newInstanceNova(t, "ACTIVE")
	nova.secgroups = []string{"default", "gone"}
	nova.failAction["removeSecurityGroup"] = http.StatusNotFound
	state := instanceTestModel(t, r, "ACTIVE")
	state.SecurityGroups = types.SetValueMust(types.StringType, []attr.Value{types.StringValue("default"), types.StringValue("gone")})
	plan := state
	plan.SecurityGroups = types.SetValueMust(types.StringType, []attr.Value{types.StringValue("default")})

	_, resp := runInstanceUpdate(t, r, state, plan)
	if resp.Diagnostics.HasError() {
		t.Fatalf("update: %v", resp.Diagnostics)
	}
	if want := []string{"removeSecurityGroup gone"}; !slices.Equal(nova.recorded(), want) {
		t.Fatalf("server actions = %v, want %v", nova.recorded(), want)
	}
}

// Nova refuses to add a group to an instance with a NIC that has port
// security disabled or no IP (400). The apply fails with Nova's reason,
// removes nothing, and keeps the prior state for the next refresh to correct.
func TestUpdateSecurityGroupAddFailureKeepsPriorState(t *testing.T) {
	shortPolls(t)
	nova, r := newInstanceNova(t, "ACTIVE")
	nova.secgroups = []string{"default", "web1"}
	nova.failAction["addSecurityGroup"] = http.StatusBadRequest
	state := instanceTestModel(t, r, "ACTIVE")
	state.SecurityGroups = types.SetValueMust(types.StringType, []attr.Value{types.StringValue("default"), types.StringValue("web1")})
	plan := state
	plan.SecurityGroups = types.SetValueMust(types.StringType, []attr.Value{types.StringValue("default"), types.StringValue("web2")})

	got, resp := runInstanceUpdate(t, r, state, plan)
	if !resp.Diagnostics.HasError() || resp.Diagnostics.Errors()[0].Summary() != "compute: adding security group web2" {
		t.Fatalf("diagnostics = %v, want the refused add reported", resp.Diagnostics)
	}
	if want := []string{"addSecurityGroup web2"}; !slices.Equal(nova.recorded(), want) {
		t.Fatalf("server actions = %v, want only %v", nova.recorded(), want)
	}
	if !got.SecurityGroups.Equal(state.SecurityGroups) {
		t.Fatalf("state security_groups = %v, want the prior %v", got.SecurityGroups, state.SecurityGroups)
	}
}

// An unconfigured security_groups (it has no plan modifier) is unknown in the
// plan of any change; it must not touch the groups, and reads back whatever
// the ports carry.
func TestUpdateLeavesUnmanagedSecurityGroupsAlone(t *testing.T) {
	shortPolls(t)
	nova, r := newInstanceNova(t, "ACTIVE")
	nova.secgroups = []string{"default", "from-ui"}
	state := instanceTestModel(t, r, "ACTIVE")
	plan := state
	plan.Name = types.StringValue("vm-renamed")
	plan.SecurityGroups = types.SetUnknown(types.StringType)

	got, resp := runInstanceUpdate(t, r, state, plan)
	if resp.Diagnostics.HasError() {
		t.Fatalf("update: %v", resp.Diagnostics)
	}
	if len(nova.recorded()) != 0 {
		t.Fatalf("server actions = %v, want none", nova.recorded())
	}
	if len(got.SecurityGroups.Elements()) != 2 {
		t.Fatalf("state security_groups = %v, want the two on the ports", got.SecurityGroups)
	}
}

// The schema guard: a RequiresReplace here would bring back the destroy and
// recreate this attribute used to cause, and a UseStateForUnknown would plan
// an unset list from state, so the next apply would remove a group added
// outside Terraform or fail with an inconsistent result.
func TestSecurityGroupsSchemaUpdatesInPlace(t *testing.T) {
	sg := instanceTestSchema(&instanceResource{}).Attributes["security_groups"].(schema.SetAttribute)
	if !sg.Optional || !sg.Computed || len(sg.PlanModifiers) != 0 {
		t.Fatalf("security_groups: optional=%v computed=%v plan modifiers=%d; want Optional+Computed, no plan modifier",
			sg.Optional, sg.Computed, len(sg.PlanModifiers))
	}
}

// Update compares the configured groups with the live instance, not with
// state: after -refresh=false or with a saved plan, state can miss a group
// added in the UI, and the apply must still end with exactly the configured
// groups (anything else fails as an inconsistent result).
func TestUpdateSecurityGroupsFollowTheLiveInstance(t *testing.T) {
	setOf := func(t *testing.T, names ...string) types.Set {
		s, d := types.SetValueFrom(context.Background(), types.StringType, names)
		if d.HasError() {
			t.Fatalf("set: %v", d)
		}
		return s
	}
	for _, tc := range []struct {
		name        string
		state, plan []string
		want        []string // server actions
	}{
		{"swap after a group was added in the UI", []string{"default", "web1"}, []string{"default", "web2"},
			[]string{"addSecurityGroup web2", "removeSecurityGroup from-ui", "removeSecurityGroup web1"}},
		{"unchanged list in an update of something else", []string{"default", "web1"}, []string{"default", "web1"},
			[]string{"removeSecurityGroup from-ui"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			shortPolls(t)
			nova, r := newInstanceNova(t, "ACTIVE")
			nova.secgroups = []string{"default", "web1", "from-ui"}
			state := instanceTestModel(t, r, "ACTIVE")
			state.SecurityGroups = setOf(t, tc.state...)
			plan := state
			plan.Name = types.StringValue("vm-renamed")
			plan.SecurityGroups = setOf(t, tc.plan...)

			got, resp := runInstanceUpdate(t, r, state, plan)
			if resp.Diagnostics.HasError() {
				t.Fatalf("update: %v", resp.Diagnostics)
			}
			if !slices.Equal(nova.recorded(), tc.want) {
				t.Fatalf("server actions = %v, want %v", nova.recorded(), tc.want)
			}
			if !got.SecurityGroups.Equal(plan.SecurityGroups) {
				t.Fatalf("state security_groups = %v, want the planned %v", got.SecurityGroups, plan.SecurityGroups)
			}
		})
	}
}

// validateInstanceConfig runs ValidateConfig on a configuration built from m.
func validateInstanceConfig(t *testing.T, r *instanceResource, m instanceModel) diag.Diagnostics {
	t.Helper()
	ctx := context.Background()
	s := instanceTestSchema(r)
	var resp resource.ValidateConfigResponse
	r.ValidateConfig(ctx, resource.ValidateConfigRequest{Config: tfsdk.Config{Schema: s, Raw: newPlan(t, s, &m).Raw}}, &resp)
	return resp.Diagnostics
}

// instanceTestConfig is a configuration with one network block.
func instanceTestConfig(t *testing.T, r *instanceResource, sgs types.Set, nic instanceNetworkModel) instanceModel {
	t.Helper()
	m := instanceTestModel(t, r, "ACTIVE")
	s := instanceTestSchema(r)
	network, d := types.ListValueFrom(context.Background(),
		s.Blocks["network"].(schema.ListNestedBlock).NestedObject.Type(), []instanceNetworkModel{nic})
	if d.HasError() {
		t.Fatalf("network: %v", d)
	}
	m.ID, m.Status, m.PowerState, m.AccessIPv4, m.Region = types.StringNull(), types.StringNull(), types.StringNull(), types.StringNull(), types.StringNull()
	m.SecurityGroups, m.Network = sgs, network
	return m
}

// Nova applies security_groups only to ports it creates; a pre-created port
// keeps its own groups. Warn, and count an unknown port (one created in the
// same apply) as set, since that is the usual configuration.
func TestValidateConfigWarnsAboutSecurityGroupsWithPorts(t *testing.T) {
	r := &instanceResource{}
	web := types.SetValueMust(types.StringType, []attr.Value{types.StringValue("web")})
	for _, tc := range []struct {
		name  string
		sgs   types.Set
		nic   instanceNetworkModel
		warns int
	}{
		{"known port", web, instanceNetworkModel{UUID: types.StringNull(), Name: types.StringNull(), Port: types.StringValue("p-1")}, 1},
		{"port created in the same apply", web, instanceNetworkModel{UUID: types.StringNull(), Name: types.StringNull(), Port: types.StringUnknown()}, 1},
		{"network uuid only", web, instanceNetworkModel{UUID: types.StringValue("net-1"), Name: types.StringNull(), Port: types.StringNull()}, 0},
		{"port without security_groups", types.SetNull(types.StringType), instanceNetworkModel{UUID: types.StringNull(), Name: types.StringNull(), Port: types.StringValue("p-1")}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := validateInstanceConfig(t, r, instanceTestConfig(t, r, tc.sgs, tc.nic))
			if d.HasError() || d.WarningsCount() != tc.warns {
				t.Fatalf("diagnostics = %v, want %d warning(s) and no error", d, tc.warns)
			}
		})
	}
}
