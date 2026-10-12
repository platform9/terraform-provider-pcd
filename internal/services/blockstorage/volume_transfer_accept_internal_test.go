// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package blockstorage

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

func (f *transferFake) acceptResource(t *testing.T) (*volumeTransferAcceptResource, schema.Schema) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(f.handler))
	t.Cleanup(srv.Close)
	r := &volumeTransferAcceptResource{config: fakeConfig(srv.URL)}
	var sch resource.SchemaResponse
	r.Schema(context.Background(), resource.SchemaRequest{}, &sch)
	return r, sch.Schema
}

// acceptedTransferModel is the state of the acceptance of t-1, which moved vol-1.
func acceptedTransferModel() volumeTransferAcceptModel {
	return volumeTransferAcceptModel{
		ID:         types.StringValue("t-1"),
		TransferID: types.StringValue("t-1"),
		AuthKey:    types.StringValue("k-1"),
		VolumeID:   types.StringValue("vol-1"),
		Region:     types.StringValue("region-one"),
	}
}

// Create accepts the offer with its key and records the volume it moved, the
// ID the receiving configuration imports.
func TestVolumeTransferAcceptCreate(t *testing.T) {
	ctx := context.Background()
	f := newTransferFake(t).on("POST /os-volume-transfer/t-1/accept", http.StatusAccepted,
		`{"transfer": {"id": "t-1", "volume_id": "vol-1", "name": null, "links": []}}`)
	r, s := f.acceptResource(t)

	planned := volumeTransferAcceptModel{
		ID:         types.StringUnknown(),
		TransferID: types.StringValue("t-1"),
		AuthKey:    types.StringValue("k-1"),
		VolumeID:   types.StringUnknown(),
		Region:     types.StringUnknown(),
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
	if len(calls) != 1 {
		t.Fatalf("create made %d requests, want one accept", len(calls))
	}
	accept, _ := calls[0].body["accept"].(map[string]any)
	if accept["auth_key"] != "k-1" || len(calls[0].body) != 1 {
		t.Fatalf("accept body = %v, want {accept: {auth_key: k-1}}", calls[0].body)
	}
	var got volumeTransferAcceptModel
	if d := resp.State.Get(ctx, &got); d.HasError() {
		t.Fatalf("reading the state: %v", d)
	}
	if got.ID.ValueString() != "t-1" || got.VolumeID.ValueString() != "vol-1" || got.Region.ValueString() != "region-one" {
		t.Fatalf("state id=%s volume_id=%s region=%s, want t-1, vol-1, region-one", got.ID, got.VolumeID, got.Region)
	}
}

// A wrong key is Cinder's 400; the apply fails and records nothing.
func TestVolumeTransferAcceptWithAWrongKeyFails(t *testing.T) {
	ctx := context.Background()
	f := newTransferFake(t).on("POST /os-volume-transfer/t-1/accept", http.StatusBadRequest,
		`{"badRequest": {"code": 400, "message": "Invalid auth key: Attempt to transfer t-1 with invalid auth key."}}`)
	r, s := f.acceptResource(t)
	planned := acceptedTransferModel()
	planned.ID, planned.VolumeID, planned.Region = types.StringUnknown(), types.StringUnknown(), types.StringUnknown()
	plan := tfsdk.Plan{Schema: s, Raw: tftypes.NewValue(s.Type().TerraformType(ctx), nil)}
	if d := plan.Set(ctx, &planned); d.HasError() {
		t.Fatalf("building the plan: %v", d)
	}
	resp := resource.CreateResponse{State: tfsdk.State{Schema: s, Raw: tftypes.NewValue(s.Type().TerraformType(ctx), nil)}}
	r.Create(ctx, resource.CreateRequest{Plan: plan}, &resp)
	if !resp.Diagnostics.HasError() {
		t.Fatal("create succeeded with a key Cinder rejected")
	}
	if !strings.Contains(resp.Diagnostics.Errors()[0].Detail(), "invalid auth key") {
		t.Fatalf("error detail %q does not carry Cinder's reason", resp.Diagnostics.Errors()[0].Detail())
	}
	if !resp.State.Raw.IsNull() {
		t.Fatal("a failed accept left state behind")
	}
}

// An accept answer without the transfer, or without the volume it moved, names
// no volume to record: it is an error, and nothing goes to state. Cinder may
// have moved the volume all the same, so the error says how to take it over.
func TestVolumeTransferAcceptWithoutATransferObjectFails(t *testing.T) {
	for name, body := range map[string]string{
		"no transfer object":          `{}`,
		"a transfer without a volume": `{"transfer": {"id": "t-1", "name": null, "links": []}}`,
	} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			f := newTransferFake(t).on("POST /os-volume-transfer/t-1/accept", http.StatusAccepted, body)
			r, s := f.acceptResource(t)
			planned := acceptedTransferModel()
			planned.ID, planned.VolumeID, planned.Region = types.StringUnknown(), types.StringUnknown(), types.StringUnknown()
			plan := tfsdk.Plan{Schema: s, Raw: tftypes.NewValue(s.Type().TerraformType(ctx), nil)}
			if d := plan.Set(ctx, &planned); d.HasError() {
				t.Fatalf("building the plan: %v", d)
			}
			resp := resource.CreateResponse{State: tfsdk.State{Schema: s, Raw: tftypes.NewValue(s.Type().TerraformType(ctx), nil)}}
			r.Create(ctx, resource.CreateRequest{Plan: plan}, &resp)
			if !reportsNoObject(resp.Diagnostics) {
				t.Fatalf("create: %v; want an error for an answer without the transferred volume", resp.Diagnostics)
			}
			if detail := resp.Diagnostics.Errors()[0].Detail(); !strings.Contains(detail, transferAcceptLostHintMarker) {
				t.Fatalf("error detail %q does not say how to take over a volume the accept may have moved", detail)
			}
			if !resp.State.Raw.IsNull() {
				t.Fatalf("create left a row: %v", resp.State.Raw)
			}
		})
	}
}

// transferAcceptLostHintMarker is part of the hint an accept error carries
// when an earlier apply may already have moved the volume.
const transferAcceptLostHintMarker = "import it with pcd_blockstorage_volume"

// An accept Cinder refuses writes nothing. A 404 is also the answer to an
// accept that already went through, since Cinder hides a spent transfer, for
// example when an earlier apply moved the volume but never recorded it, so
// that error says how to take the volume over. A full quota (413) gets no
// such hint.
func TestVolumeTransferAcceptRefusedRecordsNothing(t *testing.T) {
	for name, tc := range map[string]struct {
		status         int
		fault, message string
		hint           bool
	}{
		"the transfer is gone": {http.StatusNotFound, "itemNotFound", "Transfer t-1 could not be found.", true},
		"the quota is full": {http.StatusRequestEntityTooLarge, "overLimit",
			"Maximum number of volumes allowed (10) exceeded for quota 'volumes'.", false},
	} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			f := newTransferFake(t).on("POST /os-volume-transfer/t-1/accept", tc.status,
				`{"`+tc.fault+`": {"code": `+strconv.Itoa(tc.status)+`, "message": "`+tc.message+`"}}`)
			r, s := f.acceptResource(t)
			planned := acceptedTransferModel()
			planned.ID, planned.VolumeID, planned.Region = types.StringUnknown(), types.StringUnknown(), types.StringUnknown()
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
			if hinted := strings.Contains(detail, transferAcceptLostHintMarker); hinted != tc.hint {
				t.Fatalf("error detail %q: hint given = %v, want %v", detail, hinted, tc.hint)
			}
			if !resp.State.Raw.IsNull() {
				t.Fatalf("a refused accept left a row: %v", resp.State.Raw)
			}
		})
	}
}

// Read and Delete never call Cinder. The accepting project cannot read the
// transfer (Cinder hides it once accepted), reading the volume instead would
// drop this resource when the volume is deleted later, and an acceptance
// cannot be undone, so Delete only says so.
func TestVolumeTransferAcceptReadAndDeleteCallNothing(t *testing.T) {
	ctx := context.Background()
	f := newTransferFake(t)
	r, s := f.acceptResource(t)
	prior := acceptedTransferModel()
	state := tfsdk.State{Schema: s, Raw: tftypes.NewValue(s.Type().TerraformType(ctx), nil)}
	if d := state.Set(ctx, &prior); d.HasError() {
		t.Fatalf("building the state: %v", d)
	}

	readResp := resource.ReadResponse{State: state}
	r.Read(ctx, resource.ReadRequest{State: state}, &readResp)
	if readResp.Diagnostics.HasError() || !readResp.State.Raw.Equal(state.Raw) {
		t.Fatalf("read changed the state or failed: %v", readResp.Diagnostics)
	}

	deleteResp := resource.DeleteResponse{State: state}
	r.Delete(ctx, resource.DeleteRequest{State: state}, &deleteResp)
	if deleteResp.Diagnostics.HasError() {
		t.Fatalf("delete: %v", deleteResp.Diagnostics)
	}
	if w := deleteResp.Diagnostics.Warnings(); len(w) != 1 || !strings.Contains(w[0].Detail(), "vol-1") {
		t.Fatalf("delete warnings = %v, want one naming vol-1", w)
	}
	if calls := f.recorded(); len(calls) != 0 {
		t.Fatalf("read and delete made %d requests, want none", len(calls))
	}
}

// The key is a secret the configuration supplies: required, sensitive, and a
// new key (a new offer) means a new acceptance. A new region does too.
func TestVolumeTransferAcceptSchema(t *testing.T) {
	ctx := context.Background()
	var sch resource.SchemaResponse
	(&volumeTransferAcceptResource{}).Schema(ctx, resource.SchemaRequest{}, &sch)
	key := sch.Schema.Attributes["auth_key"].(schema.StringAttribute)
	if !key.Required || !key.Sensitive || key.WriteOnly {
		t.Fatalf("auth_key required=%v sensitive=%v write_only=%v, want required and sensitive, not write-only", key.Required, key.Sensitive, key.WriteOnly)
	}
	for _, name := range []string{"transfer_id", "auth_key", "region"} {
		attr := sch.Schema.Attributes[name].(schema.StringAttribute)
		replaces := false
		for _, m := range attr.PlanModifiers {
			replaces = replaces || m.Description(ctx) == "If the value of this attribute changes, Terraform will destroy and recreate the resource."
		}
		if !replaces {
			t.Errorf("%s does not force a new acceptance", name)
		}
	}
}
