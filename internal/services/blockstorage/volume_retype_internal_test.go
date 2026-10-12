// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package blockstorage

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
	"time"

	"github.com/gophercloud/gophercloud/v2/openstack/blockstorage/v3/volumes"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"

	"github.com/platform9/terraform-provider-pcd/internal/clients"
)

// retypeFake is a Cinder that holds one volume, vol-1. Before an os-retype
// action every GET reports volumeType with status "available"; after it, GETs
// walk through afterRetype (status, type) and then repeat its last entry. A
// wait still polling retypeFakeStuckPolls GETs past that sequence is stuck on
// a status it should accept: the fake fails the test and answers "error",
// which ends the wait within milliseconds instead of at its timeout. A
// non-empty refuse makes os-retype answer 400 with that message. GETs and PUTs
// (the name, description and metadata update) are counted and change nothing.
type retypeFake struct {
	t           *testing.T
	mu          sync.Mutex
	volumeType  string
	afterRetype [][2]string
	refuse      string
	retyped     bool
	gets        int
	polls       int
	updates     int
	actions     []map[string]any
}

// retypeFakeStuckPolls is how many GETs past its scripted statuses retypeFake
// answers before it calls a retype wait stuck.
const retypeFakeStuckPolls = 50

func (f *retypeFake) handler(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	volume := func(status, volumeType string) {
		fmt.Fprintf(w, `{"volume": {"id": "vol-1", "name": "data-1", "size": 1, "status": %q,
			"description": "", "volume_type": %q, "availability_zone": "nova",
			"bootable": "false", "encrypted": false, "metadata": {}}}`, status, volumeType)
	}
	switch r.Method + " " + r.URL.Path {
	case "GET /volumes/vol-1":
		f.gets++
		if !f.retyped {
			volume("available", f.volumeType)
			return
		}
		step := f.afterRetype[min(f.polls, len(f.afterRetype)-1)]
		f.polls++
		if stuck := len(f.afterRetype) + retypeFakeStuckPolls; f.polls >= stuck {
			if f.polls == stuck {
				f.t.Errorf("the retype wait polled %d times past the scripted statuses; it does not stop at %q", retypeFakeStuckPolls, step[0])
			}
			step[0] = "error"
		}
		volume(step[0], step[1])
	case "POST /volumes/vol-1/action":
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		if err := json.Unmarshal(raw, &body); err != nil {
			f.t.Errorf("action body is not JSON: %s", raw)
		}
		f.actions = append(f.actions, body)
		if f.refuse != "" {
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprintf(w, `{"badRequest": {"code": 400, "message": %q}}`, f.refuse)
			return
		}
		f.retyped = true
		w.WriteHeader(http.StatusAccepted)
	case "PUT /volumes/vol-1":
		f.updates++
		volume("available", f.volumeType)
	default:
		f.t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusNotImplemented)
	}
}

// fastRetypePolls shortens the retype wait's poll interval for one test. Tests
// that call it must not run in parallel with each other.
func fastRetypePolls(t *testing.T) {
	t.Helper()
	saved := volumeRetypePollInterval
	volumeRetypePollInterval = time.Millisecond
	t.Cleanup(func() { volumeRetypePollInterval = saved })
}

// retypeVolumeState is vol-1 as state records it after a refresh.
func retypeVolumeState(volumeType, policy string) volumeModel {
	return volumeModel{
		ID:                 types.StringValue("vol-1"),
		Name:               types.StringValue("data-1"),
		Size:               types.Int64Value(1),
		Description:        types.StringValue(""),
		VolumeType:         types.StringValue(volumeType),
		AvailabilityZone:   types.StringValue("nova"),
		SnapshotID:         types.StringNull(),
		SourceVolID:        types.StringNull(),
		ImageID:            types.StringNull(),
		Metadata:           types.MapValueMust(types.StringType, map[string]attr.Value{}),
		Bootable:           types.BoolValue(false),
		Encrypted:          types.BoolValue(false),
		Status:             types.StringValue("available"),
		Region:             types.StringValue("region-one"),
		VolumeRetypePolicy: types.StringValue(policy),
	}
}

// runVolumeUpdate calls Update with prior state and plan the way Terraform
// does and returns the response together with the state it recorded.
func runVolumeUpdate(t *testing.T, f *retypeFake, prior, planned volumeModel) (resource.UpdateResponse, volumeModel) {
	t.Helper()
	ctx := context.Background()
	srv := httptest.NewServer(http.HandlerFunc(f.handler))
	t.Cleanup(srv.Close)
	r := &volumeResource{config: fakeConfig(srv.URL)}
	var sch resource.SchemaResponse
	r.Schema(ctx, resource.SchemaRequest{}, &sch)
	s := sch.Schema

	state := tfsdk.State{Schema: s, Raw: tftypes.NewValue(s.Type().TerraformType(ctx), nil)}
	plan := tfsdk.Plan{Schema: s, Raw: tftypes.NewValue(s.Type().TerraformType(ctx), nil)}
	if d := state.Set(ctx, &prior); d.HasError() {
		t.Fatalf("building the state: %v", d)
	}
	if d := plan.Set(ctx, &planned); d.HasError() {
		t.Fatalf("building the plan: %v", d)
	}
	resp := resource.UpdateResponse{State: state}
	r.Update(ctx, resource.UpdateRequest{Plan: plan, State: state}, &resp)
	var got volumeModel
	if d := resp.State.Get(ctx, &got); d.HasError() {
		t.Fatalf("reading the recorded state: %v", d)
	}
	return resp, got
}

// Changing volume_type retypes the volume in place: one os-retype action with
// the configured policy, a wait while Cinder reports "retyping", and the new
// type in state. Before the fix the attribute forced a replacement, which
// destroyed the volume and its data.
func TestVolumeRetypeInPlace(t *testing.T) {
	fastRetypePolls(t)
	for _, policy := range []string{"never", "on-demand"} {
		t.Run(policy, func(t *testing.T) {
			f := &retypeFake{t: t, volumeType: "type-a", afterRetype: [][2]string{{"retyping", "type-a"}, {"retyping", "type-a"}, {"available", "type-b"}}}
			resp, got := runVolumeUpdate(t, f, retypeVolumeState("type-a", "never"), retypeVolumeState("type-b", policy))
			if resp.Diagnostics.HasError() {
				t.Fatalf("update: %v", resp.Diagnostics)
			}
			if len(f.actions) != 1 {
				t.Fatalf("volume actions = %d, want exactly one os-retype", len(f.actions))
			}
			want := map[string]any{"os-retype": map[string]any{"new_type": "type-b", "migration_policy": policy}}
			if fmt.Sprint(f.actions[0]) != fmt.Sprint(want) {
				t.Fatalf("action body = %v, want %v", f.actions[0], want)
			}
			if f.polls < 3 {
				t.Fatalf("update stopped polling after %d reads; want it to wait out \"retyping\"", f.polls)
			}
			if got.VolumeType.ValueString() != "type-b" || got.ID.ValueString() != "vol-1" {
				t.Fatalf("state volume_type=%s id=%s, want type-b and vol-1", got.VolumeType, got.ID)
			}
			if got.VolumeRetypePolicy.ValueString() != policy {
				t.Fatalf("state volume_retype_policy = %s, want %s", got.VolumeRetypePolicy, policy)
			}
		})
	}
}

// An attached volume returns to "in-use" when the retype ends; the wait must
// accept that as well as "available".
func TestVolumeRetypeEndsInUse(t *testing.T) {
	fastRetypePolls(t)
	f := &retypeFake{t: t, volumeType: "type-a", afterRetype: [][2]string{{"retyping", "type-a"}, {"in-use", "type-b"}}}
	resp, got := runVolumeUpdate(t, f, retypeVolumeState("type-a", "never"), retypeVolumeState("type-b", "never"))
	if resp.Diagnostics.HasError() {
		t.Fatalf("update: %v", resp.Diagnostics)
	}
	if got.VolumeType.ValueString() != "type-b" || got.Status.ValueString() != "in-use" {
		t.Fatalf("state volume_type=%s status=%s, want type-b and in-use", got.VolumeType, got.Status)
	}
}

// Cinder accepts a retype with 202 and can fail it later, for example when the
// new type lives on another backend and the policy is "never". It then keeps
// the old type. Update must say so and record the type the volume really has;
// writing the planned type would hide the failure until the next refresh.
func TestVolumeRetypeRevertedByCinderIsAnError(t *testing.T) {
	fastRetypePolls(t)
	f := &retypeFake{t: t, volumeType: "type-a", afterRetype: [][2]string{{"retyping", "type-a"}, {"available", "type-a"}}}
	resp, got := runVolumeUpdate(t, f, retypeVolumeState("type-a", "never"), retypeVolumeState("type-b", "never"))
	if !resp.Diagnostics.HasError() {
		t.Fatal("update succeeded although the volume kept type-a")
	}
	if summary := resp.Diagnostics.Errors()[0].Summary(); summary != "blockstorage: retype did not take effect" {
		t.Fatalf("error summary = %q", summary)
	}
	if detail := resp.Diagnostics.Errors()[0].Detail(); !strings.Contains(detail, "set it to the type's name") {
		t.Fatalf("error detail %q does not say to use the type's name", detail)
	}
	if got.VolumeType.ValueString() != "type-a" {
		t.Fatalf("state volume_type = %s, want type-a, the type Cinder kept", got.VolumeType)
	}
}

// After a referenced volume type is renamed, Terraform renames the type first
// and then plans the volume with the new name, which Cinder already reports.
// Cinder refuses a retype to the volume's own type, so Update must not send one.
func TestVolumeRetypeSkipsTheTypeTheVolumeAlreadyHas(t *testing.T) {
	fastRetypePolls(t)
	f := &retypeFake{t: t, volumeType: "type-renamed"}
	resp, got := runVolumeUpdate(t, f, retypeVolumeState("type-old-name", "never"), retypeVolumeState("type-renamed", "never"))
	if resp.Diagnostics.HasError() {
		t.Fatalf("update: %v", resp.Diagnostics)
	}
	if len(f.actions) != 0 {
		t.Fatalf("update sent %d volume actions, want none", len(f.actions))
	}
	if got.VolumeType.ValueString() != "type-renamed" {
		t.Fatalf("state volume_type = %s, want type-renamed", got.VolumeType)
	}
}

// A volume_type given as a type ID never matches the name Cinder reports.
// When the volume already has that type, Cinder refuses the retype as one to
// the same type, and the error must say to use the type's name.
func TestVolumeRetypeToATypeIDSaysToUseTheName(t *testing.T) {
	fastRetypePolls(t)
	f := &retypeFake{t: t, volumeType: "type-b", refuse: "Retype to the same volume type is not allowed."}
	resp, got := runVolumeUpdate(t, f, retypeVolumeState("type-b", "never"), retypeVolumeState("6f1d2c3b-type-b-id", "never"))
	if !resp.Diagnostics.HasError() {
		t.Fatal("update succeeded although Cinder refused the retype")
	}
	e := resp.Diagnostics.Errors()[0]
	if e.Summary() != "blockstorage: retyping volume" || !strings.Contains(e.Detail(), "set it to the type's name") {
		t.Fatalf("error %q: %q, want the retype summary and a hint to use the type's name", e.Summary(), e.Detail())
	}
	if got.VolumeType.ValueString() != "type-b" {
		t.Fatalf("state volume_type = %s, want type-b from the prior state", got.VolumeType)
	}
}

// volume_retype_policy only qualifies a retype. Changing it alone plans an
// update, and that update must not retype the volume. When the configuration
// leaves out metadata, Terraform plans it unknown, so Update also rewrites the
// name and description; the second case is that plan.
func TestVolumeRetypePolicyChangeAloneMakesNoRetype(t *testing.T) {
	fastRetypePolls(t)
	for _, tc := range []struct {
		name     string
		metadata types.Map
		wantPUTs int
	}{
		{name: "metadata configured", metadata: types.MapValueMust(types.StringType, map[string]attr.Value{})},
		{name: "metadata left out", metadata: types.MapUnknown(types.StringType), wantPUTs: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &retypeFake{t: t, volumeType: "type-a"}
			planned := retypeVolumeState("type-a", "on-demand")
			planned.Metadata = tc.metadata
			resp, got := runVolumeUpdate(t, f, retypeVolumeState("type-a", "never"), planned)
			if resp.Diagnostics.HasError() {
				t.Fatalf("update: %v", resp.Diagnostics)
			}
			if len(f.actions) != 0 {
				t.Fatalf("update sent %d volume actions, want none", len(f.actions))
			}
			if f.updates != tc.wantPUTs {
				t.Fatalf("update sent %d volume PUTs, want %d", f.updates, tc.wantPUTs)
			}
			// Only the final read: a retype would read the volume first.
			if f.gets != 1 {
				t.Fatalf("update read the volume %d times, want 1", f.gets)
			}
			if got.VolumeRetypePolicy.ValueString() != "on-demand" {
				t.Fatalf("state volume_retype_policy = %s, want on-demand", got.VolumeRetypePolicy)
			}
		})
	}
}

// Cinder has nothing to read the policy back from. A state written by an
// older provider, or by an import, has none; Read fills in the default so the
// next plan is empty.
func TestVolumeReadDefaultsTheRetypePolicy(t *testing.T) {
	ctx := context.Background()
	f := &retypeFake{t: t, volumeType: "type-a"}
	srv := httptest.NewServer(http.HandlerFunc(f.handler))
	t.Cleanup(srv.Close)
	r := &volumeResource{config: fakeConfig(srv.URL)}
	var sch resource.SchemaResponse
	r.Schema(ctx, resource.SchemaRequest{}, &sch)
	s := sch.Schema

	prior := retypeVolumeState("type-a", "never")
	prior.VolumeRetypePolicy = types.StringNull()
	state := tfsdk.State{Schema: s, Raw: tftypes.NewValue(s.Type().TerraformType(ctx), nil)}
	if d := state.Set(ctx, &prior); d.HasError() {
		t.Fatalf("building the state: %v", d)
	}
	resp := resource.ReadResponse{State: state}
	r.Read(ctx, resource.ReadRequest{State: state}, &resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("read: %v", resp.Diagnostics)
	}
	var got volumeModel
	if d := resp.State.Get(ctx, &got); d.HasError() {
		t.Fatalf("reading the state: %v", d)
	}
	if got.VolumeRetypePolicy.ValueString() != "never" {
		t.Fatalf("volume_retype_policy = %s, want never", got.VolumeRetypePolicy)
	}
}

// volume_type no longer forces a replacement, and volume_retype_policy
// accepts only the two policies Cinder knows.
func TestVolumeRetypeSchema(t *testing.T) {
	ctx := context.Background()
	var sch resource.SchemaResponse
	(&volumeResource{}).Schema(ctx, resource.SchemaRequest{}, &sch)

	vt := sch.Schema.Attributes["volume_type"].(schema.StringAttribute)
	for _, m := range vt.PlanModifiers {
		if m.Description(ctx) == "If the value of this attribute changes, Terraform will destroy and recreate the resource." {
			t.Fatal("volume_type still forces replacement")
		}
	}

	policy := sch.Schema.Attributes["volume_retype_policy"].(schema.StringAttribute)
	if !policy.Optional || !policy.Computed || policy.Default == nil {
		t.Fatalf("volume_retype_policy optional=%v computed=%v default=%v, want optional, computed, with a default", policy.Optional, policy.Computed, policy.Default)
	}
	for value, wantErr := range map[string]bool{"never": false, "on-demand": false, "always": true} {
		resp := &validator.StringResponse{}
		for _, v := range policy.Validators {
			v.ValidateString(ctx, validator.StringRequest{ConfigValue: types.StringValue(value)}, resp)
		}
		if resp.Diagnostics.HasError() != wantErr {
			t.Errorf("volume_retype_policy = %q: error = %v, want %v", value, resp.Diagnostics.HasError(), wantErr)
		}
	}
}

// gophercloud decodes a 200 answer without the volume to an empty volume, not
// to nil. Its empty type must not send a retype, and its empty status must not
// keep the wait polling until it times out: both reads report ErrNoObject.
func TestVolumeRetypeAnswerWithoutTheVolumeIsAnError(t *testing.T) {
	fastRetypePolls(t)
	var mu sync.Mutex
	actions := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch r.Method + " " + r.URL.Path {
		case "GET /volumes/vol-1":
			fmt.Fprint(w, `{}`)
		case "POST /volumes/vol-1/action":
			actions++
			w.WriteHeader(http.StatusBadRequest)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotImplemented)
		}
	}))
	t.Cleanup(srv.Close)
	client, err := fakeConfig(srv.URL).BlockStorageV3Client()
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	ctx := context.Background()
	if _, err := retypeVolume(ctx, client, "vol-1", "type-b", volumes.MigrationPolicyNever); !errors.Is(err, clients.ErrNoObject) {
		t.Errorf("retypeVolume error = %v, want ErrNoObject", err)
	}
	if _, err := waitForVolumeRetype(ctx, client, "vol-1", 2*time.Second); !errors.Is(err, clients.ErrNoObject) {
		t.Errorf("waitForVolumeRetype error = %v, want ErrNoObject", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if actions != 0 {
		t.Errorf("retypeVolume sent %d volume actions for a volume it could not read, want none", actions)
	}
}

// The HTTP client has no timeout, so the wait's own timeout must end it,
// whether Cinder never answers a GET or keeps reporting "retyping". The error
// says that the wait timed out, for which volume and in what status.
func TestVolumeRetypeWaitStopsAtItsTimeout(t *testing.T) {
	for _, tc := range []struct {
		name       string
		hang       bool
		wantStatus string
	}{
		{name: "GET never answered", hang: true, wantStatus: `(last status "")`},
		{name: "still retyping", wantStatus: `(last status "retyping")`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			release := make(chan struct{})
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if !tc.hang {
					fmt.Fprint(w, `{"volume": {"id": "vol-1", "status": "retyping", "volume_type": "type-a"}}`)
					return
				}
				select {
				case <-r.Context().Done():
				case <-release:
				}
			}))
			t.Cleanup(srv.Close)
			// Cleanups run last-in first-out: the handler is released before
			// Close waits for it.
			t.Cleanup(func() { close(release) })
			client, err := fakeConfig(srv.URL).BlockStorageV3Client()
			if err != nil {
				t.Fatalf("client: %v", err)
			}
			done := make(chan error, 1)
			go func() {
				_, err := waitForVolumeRetype(context.Background(), client, "vol-1", 500*time.Millisecond)
				done <- err
			}()
			select {
			case err := <-done:
				if !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("wait error = %v, want the timeout's context.DeadlineExceeded", err)
				}
				for _, want := range []string{"timed out", "vol-1", tc.wantStatus} {
					if !strings.Contains(err.Error(), want) {
						t.Fatalf("wait error %q does not contain %q", err, want)
					}
				}
			case <-time.After(5 * time.Second):
				t.Fatal("the retype wait was still running 5s after its 500ms timeout")
			}
		})
	}
}
