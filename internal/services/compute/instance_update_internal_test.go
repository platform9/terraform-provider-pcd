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

	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
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

	actions []string // "os-stop", "resize", "addSecurityGroup web", ...
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
		Region:            types.StringValue("region-one"),
	}
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
