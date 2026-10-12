// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package blockstorage

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

// transferCall is one request the fake Cinder saw.
type transferCall struct {
	method, path, microversion string
	body                       map[string]any
}

// transferRoute is the canned answer to one "METHOD /path".
type transferRoute struct {
	status int
	body   string
}

// transferFake is a Cinder that answers each "METHOD /path" from routes and
// records every request. A request it has no route for fails the test.
type transferFake struct {
	t      *testing.T
	mu     sync.Mutex
	routes map[string]transferRoute
	calls  []transferCall
}

func newTransferFake(t *testing.T) *transferFake {
	return &transferFake{t: t, routes: map[string]transferRoute{}}
}

func (f *transferFake) on(route string, status int, body string) *transferFake {
	f.routes[route] = transferRoute{status: status, body: body}
	return f
}

func (f *transferFake) handler(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	call := transferCall{method: r.Method, path: r.URL.Path, microversion: r.Header.Get("OpenStack-API-Version")}
	if raw, _ := io.ReadAll(r.Body); len(raw) > 0 {
		if err := json.Unmarshal(raw, &call.body); err != nil {
			f.t.Errorf("%s %s: body is not JSON: %s", r.Method, r.URL.Path, raw)
		}
	}
	f.calls = append(f.calls, call)
	route, ok := f.routes[r.Method+" "+r.URL.Path]
	if !ok {
		f.t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusNotImplemented)
		return
	}
	w.WriteHeader(route.status)
	_, _ = io.WriteString(w, route.body)
}

func (f *transferFake) recorded() []transferCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]transferCall(nil), f.calls...)
}

func (f *transferFake) offerResource(t *testing.T) (*volumeTransferResource, schema.Schema) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(f.handler))
	t.Cleanup(srv.Close)
	r := &volumeTransferResource{config: fakeConfig(srv.URL)}
	var sch resource.SchemaResponse
	r.Schema(context.Background(), resource.SchemaRequest{}, &sch)
	return r, sch.Schema
}

const transferCreated = `{"transfer": {"id": "t-1", "auth_key": "k-1", "volume_id": "vol-1", "name": null,
	"created_at": "2026-10-06T10:20:30.000000", "source_project_id": "proj-src",
	"destination_project_id": null, "accepted": false, "links": []}}`

const transferPending = `{"transfer": {"id": "t-1", "volume_id": "vol-1", "name": "move-data",
	"created_at": "2026-10-06T10:20:30.000000", "source_project_id": "proj-src",
	"destination_project_id": null, "accepted": false, "links": []}}`

// pendingOffer is the state of offer t-1 for vol-1 from project proj-src.
func pendingOffer() volumeTransferModel {
	return volumeTransferModel{
		ID:              types.StringValue("t-1"),
		VolumeID:        types.StringValue("vol-1"),
		Name:            types.StringValue("move-data"),
		AuthKey:         types.StringValue("k-1"),
		SourceProjectID: types.StringValue("proj-src"),
		Accepted:        types.BoolValue(false),
		Region:          types.StringValue("region-one"),
	}
}

func readOffer(t *testing.T, r *volumeTransferResource, s schema.Schema, prior volumeTransferModel) (resource.ReadResponse, *volumeTransferModel) {
	t.Helper()
	ctx := context.Background()
	state := tfsdk.State{Schema: s, Raw: tftypes.NewValue(s.Type().TerraformType(ctx), nil)}
	if d := state.Set(ctx, &prior); d.HasError() {
		t.Fatalf("building the state: %v", d)
	}
	resp := resource.ReadResponse{State: state}
	r.Read(ctx, resource.ReadRequest{State: state}, &resp)
	if resp.State.Raw.IsNull() {
		return resp, nil
	}
	var got volumeTransferModel
	if d := resp.State.Get(ctx, &got); d.HasError() {
		t.Fatalf("reading the state: %v", d)
	}
	return resp, &got
}

// Create offers the volume and records the one thing Cinder never shows
// again, the auth key, plus the project the volume came from. The request
// asks for volume API 3.57, the first version whose transfer carries
// source_project_id; Read needs it to tell an accepted offer from a canceled one.
func TestVolumeTransferCreateRecordsTheKeyAndTheSourceProject(t *testing.T) {
	ctx := context.Background()
	f := newTransferFake(t).on("POST /os-volume-transfer", http.StatusAccepted, transferCreated)
	r, s := f.offerResource(t)

	planned := volumeTransferModel{
		ID:              types.StringUnknown(),
		VolumeID:        types.StringValue("vol-1"),
		Name:            types.StringNull(),
		AuthKey:         types.StringUnknown(),
		SourceProjectID: types.StringUnknown(),
		Accepted:        types.BoolUnknown(),
		Region:          types.StringUnknown(),
	}
	plan := tfsdk.Plan{Schema: s, Raw: tftypes.NewValue(s.Type().TerraformType(ctx), nil)}
	if d := plan.Set(ctx, &planned); d.HasError() {
		t.Fatalf("building the plan: %v", d)
	}
	resp := resource.CreateResponse{State: tfsdk.State{Schema: s, Raw: tftypes.NewValue(s.Type().TerraformType(ctx), nil)}}
	r.Create(ctx, resource.CreateRequest{Plan: plan}, &resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("create: %v", resp.Diagnostics)
	}

	calls := f.recorded()
	if len(calls) != 1 || calls[0].microversion != "volume "+blockstorageMicroversionTransferResponse {
		t.Fatalf("requests %+v, want one POST at volume %s", calls, blockstorageMicroversionTransferResponse)
	}
	transfer, _ := calls[0].body["transfer"].(map[string]any)
	if transfer["volume_id"] != "vol-1" {
		t.Fatalf("request body %v, want transfer.volume_id = vol-1", calls[0].body)
	}
	if _, sent := transfer["name"]; sent {
		t.Fatalf("request body %v sends a name although none is configured", calls[0].body)
	}

	if !resp.State.Raw.IsFullyKnown() {
		t.Fatalf("create state holds unknown values: %v", resp.State.Raw)
	}
	var got volumeTransferModel
	if d := resp.State.Get(ctx, &got); d.HasError() {
		t.Fatalf("reading the state: %v", d)
	}
	if got.ID.ValueString() != "t-1" || got.AuthKey.ValueString() != "k-1" || got.SourceProjectID.ValueString() != "proj-src" {
		t.Fatalf("state id=%s auth_key=%s source_project_id=%s, want t-1, k-1, proj-src", got.ID, got.AuthKey, got.SourceProjectID)
	}
	if got.Accepted.ValueBool() || !got.Name.IsNull() {
		t.Fatalf("state accepted=%s name=%s, want false and null", got.Accepted, got.Name)
	}
	if !s.Attributes["auth_key"].IsSensitive() {
		t.Fatal("auth_key is not sensitive; anyone who reads the plan output could accept the transfer")
	}
}

// While the offer is pending, Read refreshes it from Cinder and keeps the
// auth key, which Cinder does not return after the create.
func TestVolumeTransferReadPendingKeepsTheKey(t *testing.T) {
	f := newTransferFake(t).on("GET /os-volume-transfer/t-1", http.StatusOK, transferPending)
	r, s := f.offerResource(t)
	resp, got := readOffer(t, r, s, pendingOffer())
	if resp.Diagnostics.HasError() || got == nil {
		t.Fatalf("read: %v (state removed: %v)", resp.Diagnostics, got == nil)
	}
	if got.AuthKey.ValueString() != "k-1" || got.Accepted.ValueBool() || got.Name.ValueString() != "move-data" {
		t.Fatalf("state auth_key=%s accepted=%s name=%s, want k-1, false, move-data", got.AuthKey, got.Accepted, got.Name)
	}
	if calls := f.recorded(); len(calls) != 1 || calls[0].microversion != "volume "+blockstorageMicroversionTransferResponse {
		t.Fatalf("requests %+v, want one GET at volume %s", calls, blockstorageMicroversionTransferResponse)
	}
}

// Cinder hides a transfer once it is accepted or canceled, so a 404 alone
// cannot say which. The volume can: an accepted transfer moved it to another
// project. Removing an accepted offer from state would make the next plan
// offer the volume again; keeping a canceled one would hide that it is gone.
func TestVolumeTransferReadAfterTheTransferIsGone(t *testing.T) {
	for _, tc := range []struct {
		name         string
		volumeStatus int
		volumeBody   string
		wantKept     bool
	}{
		{
			name:         "volume moved out of sight",
			volumeStatus: http.StatusNotFound, volumeBody: `{"itemNotFound": {"code": 404, "message": "Volume vol-1 could not be found."}}`,
			wantKept: true,
		},
		{
			name:         "admin sees the volume in another project",
			volumeStatus: http.StatusOK, volumeBody: `{"volume": {"id": "vol-1", "status": "available", "os-vol-tenant-attr:tenant_id": "proj-dst"}}`,
			wantKept: true,
		},
		{
			name:         "admin sees the volume still in the source project",
			volumeStatus: http.StatusOK, volumeBody: `{"volume": {"id": "vol-1", "status": "available", "os-vol-tenant-attr:tenant_id": "proj-src"}}`,
		},
		{
			name:         "member sees the volume, so it is still in the source project",
			volumeStatus: http.StatusOK, volumeBody: `{"volume": {"id": "vol-1", "status": "available"}}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newTransferFake(t).
				on("GET /os-volume-transfer/t-1", http.StatusNotFound, `{"itemNotFound": {"code": 404, "message": "Transfer t-1 could not be found."}}`).
				on("GET /volumes/vol-1", tc.volumeStatus, tc.volumeBody)
			r, s := f.offerResource(t)
			resp, got := readOffer(t, r, s, pendingOffer())
			if resp.Diagnostics.HasError() {
				t.Fatalf("read: %v", resp.Diagnostics)
			}
			if kept := got != nil; kept != tc.wantKept {
				t.Fatalf("offer kept in state = %v, want %v", kept, tc.wantKept)
			}
			if tc.wantKept {
				if !got.Accepted.ValueBool() || got.AuthKey.ValueString() != "k-1" {
					t.Fatalf("state accepted=%s auth_key=%s, want true and k-1", got.Accepted, got.AuthKey)
				}
				return
			}
			if len(resp.Diagnostics.Warnings()) != 1 {
				t.Fatalf("warnings = %v, want one saying the offer was canceled outside Terraform", resp.Diagnostics.Warnings())
			}
		})
	}
}

// Once accepted, an offer stays accepted without asking Cinder: the volume
// may later come back to this project through another transfer, which must
// not make this offer look canceled and plan a new one.
func TestVolumeTransferReadOfAnAcceptedOfferCallsNothing(t *testing.T) {
	f := newTransferFake(t)
	r, s := f.offerResource(t)
	prior := pendingOffer()
	prior.Accepted = types.BoolValue(true)
	resp, got := readOffer(t, r, s, prior)
	if resp.Diagnostics.HasError() || got == nil || !got.Accepted.ValueBool() {
		t.Fatalf("read: %v, state %+v; want the accepted offer kept", resp.Diagnostics, got)
	}
	if calls := f.recorded(); len(calls) != 0 {
		t.Fatalf("read made %d requests, want none", len(calls))
	}
}

// Destroying the offer cancels it. A transfer that is already gone, accepted
// or canceled, is as good as canceled.
func TestVolumeTransferDeleteCancels(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
	}{
		{name: "pending", status: http.StatusAccepted},
		{name: "already gone", status: http.StatusNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			f := newTransferFake(t).on("DELETE /os-volume-transfer/t-1", tc.status, "")
			r, s := f.offerResource(t)
			prior := pendingOffer()
			state := tfsdk.State{Schema: s, Raw: tftypes.NewValue(s.Type().TerraformType(ctx), nil)}
			if d := state.Set(ctx, &prior); d.HasError() {
				t.Fatalf("building the state: %v", d)
			}
			resp := resource.DeleteResponse{State: state}
			r.Delete(ctx, resource.DeleteRequest{State: state}, &resp)
			if resp.Diagnostics.HasError() {
				t.Fatalf("delete: %v", resp.Diagnostics)
			}
			if calls := f.recorded(); len(calls) != 1 {
				t.Fatalf("delete made %d requests, want one DELETE", len(calls))
			}
		})
	}
}

// An import knows only the transfer ID. Read fills in the rest from Cinder;
// the auth key stays null because Cinder returns it only on create.
func TestVolumeTransferImport(t *testing.T) {
	ctx := context.Background()
	f := newTransferFake(t).on("GET /os-volume-transfer/t-1", http.StatusOK, transferPending)
	r, s := f.offerResource(t)

	importResp := resource.ImportStateResponse{State: tfsdk.State{Schema: s, Raw: tftypes.NewValue(s.Type().TerraformType(ctx), nil)}}
	r.ImportState(ctx, resource.ImportStateRequest{ID: "t-1"}, &importResp)
	if importResp.Diagnostics.HasError() {
		t.Fatalf("import: %v", importResp.Diagnostics)
	}
	readResp := resource.ReadResponse{State: importResp.State}
	r.Read(ctx, resource.ReadRequest{State: importResp.State}, &readResp)
	if readResp.Diagnostics.HasError() {
		t.Fatalf("read after import: %v", readResp.Diagnostics)
	}
	var got volumeTransferModel
	if d := readResp.State.Get(ctx, &got); d.HasError() {
		t.Fatalf("reading the state: %v", d)
	}
	if got.VolumeID.ValueString() != "vol-1" || got.SourceProjectID.ValueString() != "proj-src" || !got.AuthKey.IsNull() {
		t.Fatalf("imported volume_id=%s source_project_id=%s auth_key=%s, want vol-1, proj-src, null", got.VolumeID, got.SourceProjectID, got.AuthKey)
	}
}

// Cinder strips a transfer's name, and gophercloud never sends an empty one.
// A configured name that is empty or has surrounding whitespace would never
// match what Read reports, and every plan would replace the offer and its key.
func TestVolumeTransferNameMustSurviveCinder(t *testing.T) {
	ctx := context.Background()
	var sch resource.SchemaResponse
	(&volumeTransferResource{}).Schema(ctx, resource.SchemaRequest{}, &sch)
	name := sch.Schema.Attributes["name"].(schema.StringAttribute)
	for value, wantErr := range map[string]bool{
		"move-data": false, "a": false, "app-data to team-b": false,
		"": true, " move-data": true, "move-data ": true, "\tmove-data": true,
	} {
		resp := &validator.StringResponse{}
		for _, v := range name.Validators {
			v.ValidateString(ctx, validator.StringRequest{ConfigValue: types.StringValue(value)}, resp)
		}
		if resp.Diagnostics.HasError() != wantErr {
			t.Errorf("name = %q: error = %v, want %v", value, resp.Diagnostics.HasError(), wantErr)
		}
	}
}

// Cinder holds the offer as soon as it answers the create, and the auth key
// exists only in that answer, so Create records both before it decodes the
// rest. An answer it cannot fully read then fails the apply with the offer in
// state, which Terraform taints and the next apply cancels and offers again,
// instead of leaving the volume awaiting a transfer nothing tracks.
func TestVolumeTransferCreateRecordsTheOfferBeforeReadingTheRest(t *testing.T) {
	ctx := context.Background()
	f := newTransferFake(t).on("POST /os-volume-transfer", http.StatusAccepted,
		`{"transfer": {"id": "t-1", "auth_key": "k-1", "volume_id": "vol-1", "name": null,
		"created_at": "2026-10-06T10:20:30.000000", "source_project_id": 7, "links": []}}`)
	r, s := f.offerResource(t)
	planned := volumeTransferModel{
		ID:              types.StringUnknown(),
		VolumeID:        types.StringValue("vol-1"),
		Name:            types.StringNull(),
		AuthKey:         types.StringUnknown(),
		SourceProjectID: types.StringUnknown(),
		Accepted:        types.BoolUnknown(),
		Region:          types.StringUnknown(),
	}
	plan := tfsdk.Plan{Schema: s, Raw: tftypes.NewValue(s.Type().TerraformType(ctx), nil)}
	if d := plan.Set(ctx, &planned); d.HasError() {
		t.Fatalf("building the plan: %v", d)
	}
	resp := resource.CreateResponse{State: tfsdk.State{Schema: s, Raw: tftypes.NewValue(s.Type().TerraformType(ctx), nil)}}
	r.Create(ctx, resource.CreateRequest{Plan: plan}, &resp)
	if !resp.Diagnostics.HasError() {
		t.Fatal("create succeeded although the answer's source_project_id is not a string")
	}
	if resp.State.Raw.IsNull() {
		t.Fatal("the offer Cinder created was left out of state")
	}
	var got volumeTransferModel
	if d := resp.State.Get(ctx, &got); d.HasError() {
		t.Fatalf("reading the state: %v", d)
	}
	if got.ID.ValueString() != "t-1" || got.AuthKey.ValueString() != "k-1" || !got.SourceProjectID.IsNull() {
		t.Fatalf("state id=%s auth_key=%s source_project_id=%s, want t-1, k-1 and null", got.ID, got.AuthKey, got.SourceProjectID)
	}
}

// A state row with no ID names no transfer: a GET or DELETE with an empty ID
// would reach Cinder's transfer list instead. Read drops the row with a
// warning and Delete skips it, and neither calls Cinder.
func TestVolumeTransferRowWithoutIDCallsNothing(t *testing.T) {
	ctx := context.Background()
	f := newTransferFake(t)
	r, s := f.offerResource(t)
	prior := pendingOffer()
	prior.ID = types.StringValue("")
	resp, got := readOffer(t, r, s, prior)
	if resp.Diagnostics.HasError() || got != nil || len(resp.Diagnostics.Warnings()) != 1 {
		t.Fatalf("read: %v, row kept: %v; want the row dropped with one warning", resp.Diagnostics, got != nil)
	}
	state := tfsdk.State{Schema: s, Raw: tftypes.NewValue(s.Type().TerraformType(ctx), nil)}
	if d := state.Set(ctx, &prior); d.HasError() {
		t.Fatalf("building the state: %v", d)
	}
	deleteResp := resource.DeleteResponse{State: state}
	r.Delete(ctx, resource.DeleteRequest{State: state}, &deleteResp)
	if deleteResp.Diagnostics.HasError() || len(deleteResp.Diagnostics.Warnings()) != 1 {
		t.Fatalf("delete: %v; want no error and one warning", deleteResp.Diagnostics)
	}
	if calls := f.recorded(); len(calls) != 0 {
		t.Fatalf("read and delete made %d requests, want none", len(calls))
	}
}

// A 200 without a transfer object is an error, not a transfer with an empty
// ID: saving that would lose the offer and its auth key from state.
func TestVolumeTransferReadWithoutATransferObjectFails(t *testing.T) {
	f := newTransferFake(t).on("GET /os-volume-transfer/t-1", http.StatusOK, `{}`)
	r, s := f.offerResource(t)
	resp, got := readOffer(t, r, s, pendingOffer())
	if !reportsNoObject(resp.Diagnostics) {
		t.Fatalf("read: %v; want an error for an answer without a transfer object", resp.Diagnostics)
	}
	if got == nil || got.ID.ValueString() != "t-1" || got.AuthKey.ValueString() != "k-1" {
		t.Fatalf("state %+v, want the prior offer kept", got)
	}
}

// After the transfer is gone, a 200 without a volume object says nothing
// about where the volume is. Reading it as "the volume is still ours" would
// drop a pending or accepted offer as canceled, so it is an error that keeps
// the offer in state.
func TestVolumeTransferReadWithoutAVolumeObjectFails(t *testing.T) {
	f := newTransferFake(t).
		on("GET /os-volume-transfer/t-1", http.StatusNotFound, `{"itemNotFound": {"code": 404, "message": "Transfer t-1 could not be found."}}`).
		on("GET /volumes/vol-1", http.StatusOK, `{}`)
	r, s := f.offerResource(t)
	resp, got := readOffer(t, r, s, pendingOffer())
	if !reportsNoObject(resp.Diagnostics) {
		t.Fatalf("read: %v; want an error for an answer without a volume object", resp.Diagnostics)
	}
	if got == nil || got.ID.ValueString() != "t-1" || got.AuthKey.ValueString() != "k-1" || got.Accepted.ValueBool() {
		t.Fatalf("state %+v, want the prior pending offer kept", got)
	}
}

// A create answer without a transfer object names no offer to record: it is
// an error, and nothing goes to state.
func TestVolumeTransferCreateWithoutATransferObjectFails(t *testing.T) {
	ctx := context.Background()
	f := newTransferFake(t).on("POST /os-volume-transfer", http.StatusAccepted, `{}`)
	r, s := f.offerResource(t)
	planned := volumeTransferModel{
		ID:              types.StringUnknown(),
		VolumeID:        types.StringValue("vol-1"),
		Name:            types.StringNull(),
		AuthKey:         types.StringUnknown(),
		SourceProjectID: types.StringUnknown(),
		Accepted:        types.BoolUnknown(),
		Region:          types.StringUnknown(),
	}
	plan := tfsdk.Plan{Schema: s, Raw: tftypes.NewValue(s.Type().TerraformType(ctx), nil)}
	if d := plan.Set(ctx, &planned); d.HasError() {
		t.Fatalf("building the plan: %v", d)
	}
	resp := resource.CreateResponse{State: tfsdk.State{Schema: s, Raw: tftypes.NewValue(s.Type().TerraformType(ctx), nil)}}
	r.Create(ctx, resource.CreateRequest{Plan: plan}, &resp)
	if !reportsNoObject(resp.Diagnostics) {
		t.Fatalf("create: %v; want an error for an answer without a transfer object", resp.Diagnostics)
	}
	if !resp.State.Raw.IsNull() {
		t.Fatalf("create left a row: %v", resp.State.Raw)
	}
}

func TestOfferAccepted(t *testing.T) {
	for _, tc := range []struct {
		visible       bool
		volumeProject string
		sourceProject string
		want          bool
		why           string
	}{
		{visible: false, want: true, why: "the volume left the caller's view"},
		{visible: true, volumeProject: "proj-dst", sourceProject: "proj-src", want: true, why: "an admin sees the new owner"},
		{visible: true, volumeProject: "proj-src", sourceProject: "proj-src", want: false, why: "the volume never moved"},
		{visible: true, volumeProject: "", sourceProject: "proj-src", want: false, why: "a member sees only its own project's volumes"},
		{visible: true, volumeProject: "proj-dst", sourceProject: "", want: false, why: "without the source project nothing proves a move"},
	} {
		if got := offerAccepted(tc.visible, tc.volumeProject, tc.sourceProject); got != tc.want {
			t.Errorf("offerAccepted(%v, %q, %q) = %v, want %v: %s", tc.visible, tc.volumeProject, tc.sourceProject, got, tc.want, tc.why)
		}
	}
}

// An offer for a volume that is not available is Cinder's 400. A volume
// already awaiting a transfer has an earlier offer pending, often one a failed
// apply left behind, so the error says how to find it and what to do with it.
// Cinder's own message stays, and other 400s get no hint.
func TestVolumeTransferCreateHintsAtAPendingOffer(t *testing.T) {
	for name, tc := range map[string]struct {
		message string
		hint    bool
	}{
		"the volume is not available":  {message: "Invalid volume: status must be available", hint: true},
		"a snapshot is not available":  {message: "Invalid snapshot: snapshot: snap-1 status must be available", hint: false},
		"an encrypted volume is given": {message: "Invalid volume: transferring encrypted volume is not supported", hint: false},
	} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			f := newTransferFake(t).on("POST /os-volume-transfer", http.StatusBadRequest,
				`{"badRequest": {"code": 400, "message": "`+tc.message+`"}}`)
			r, s := f.offerResource(t)
			planned := volumeTransferModel{
				ID:              types.StringUnknown(),
				VolumeID:        types.StringValue("vol-1"),
				Name:            types.StringNull(),
				AuthKey:         types.StringUnknown(),
				SourceProjectID: types.StringUnknown(),
				Accepted:        types.BoolUnknown(),
				Region:          types.StringUnknown(),
			}
			plan := tfsdk.Plan{Schema: s, Raw: tftypes.NewValue(s.Type().TerraformType(ctx), nil)}
			if d := plan.Set(ctx, &planned); d.HasError() {
				t.Fatalf("building the plan: %v", d)
			}
			resp := resource.CreateResponse{State: tfsdk.State{Schema: s, Raw: tftypes.NewValue(s.Type().TerraformType(ctx), nil)}}
			r.Create(ctx, resource.CreateRequest{Plan: plan}, &resp)
			errs := resp.Diagnostics.Errors()
			if len(errs) != 1 {
				t.Fatalf("create diagnostics = %v, want one error", resp.Diagnostics)
			}
			detail := errs[0].Detail()
			if !strings.Contains(detail, tc.message) {
				t.Fatalf("error detail %q does not carry Cinder's message", detail)
			}
			hinted := strings.Contains(detail, "pcdctl volume transfer request delete")
			if hinted != tc.hint {
				t.Fatalf("error detail %q: hint given = %v, want %v", detail, hinted, tc.hint)
			}
			if !resp.State.Raw.IsNull() {
				t.Fatalf("create left a row: %v", resp.State.Raw)
			}
		})
	}
}
