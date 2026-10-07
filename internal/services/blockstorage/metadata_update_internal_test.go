// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package blockstorage

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

// fakeCinderMetadata answers for snapshot snap-1 and volume vol-1 the way
// Cinder does, keeping each one's name and metadata. A metadata PUT whose body
// has no metadata element gets Cinder's 400, and a volume PUT replaces the
// metadata only when its body carries a metadata element.
type fakeCinderMetadata struct {
	mu                  sync.Mutex
	snapName, volName   string
	snapMeta, volMeta   map[string]string
	snapshotMetadataPUT int
}

func (f *fakeCinderMetadata) serve(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		var body map[string]json.RawMessage
		if r.Body != nil && (r.Method == http.MethodPut || r.Method == http.MethodPost) {
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decoding %s %s: %v", r.Method, r.URL.Path, err)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.Method + " " + r.URL.Path {
		case "GET /snapshots/snap-1", "PUT /snapshots/snap-1":
			if raw, ok := body["snapshot"]; ok {
				var s struct{ Name *string }
				_ = json.Unmarshal(raw, &s)
				if s.Name != nil {
					f.snapName = *s.Name
				}
			}
			meta, _ := json.Marshal(f.snapMeta)
			fmt.Fprintf(w, `{"snapshot": {"id": "snap-1", "name": %q, "volume_id": "vol-1", "status": "available",
				"description": "", "size": 1, "metadata": %s}}`, f.snapName, meta)
		case "PUT /snapshots/snap-1/metadata":
			f.snapshotMetadataPUT++
			raw, ok := body["metadata"]
			if !ok {
				w.WriteHeader(http.StatusBadRequest)
				fmt.Fprint(w, `{"badRequest": {"code": 400, "message": "Missing required element 'metadata' in request body."}}`)
				return
			}
			f.snapMeta = map[string]string{}
			_ = json.Unmarshal(raw, &f.snapMeta)
			fmt.Fprintf(w, `{"metadata": %s}`, raw)
		case "GET /volumes/vol-1", "PUT /volumes/vol-1":
			if raw, ok := body["volume"]; ok {
				var v struct {
					Name     *string
					Metadata *map[string]string
				}
				_ = json.Unmarshal(raw, &v)
				if v.Name != nil {
					f.volName = *v.Name
				}
				if v.Metadata != nil {
					f.volMeta = *v.Metadata
				}
			}
			meta, _ := json.Marshal(f.volMeta)
			fmt.Fprintf(w, `{"volume": {"id": "vol-1", "name": %q, "size": 1, "status": "available", "description": "",
				"volume_type": "nfs", "availability_zone": "nova", "bootable": "false", "encrypted": false,
				"metadata": %s}}`, f.volName, meta)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotImplemented)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// stringMapValue returns m as a Terraform map of strings.
func stringMapValue(t *testing.T, m map[string]string) types.Map {
	t.Helper()
	v, d := types.MapValueFrom(context.Background(), types.StringType, m)
	if d.HasError() {
		t.Fatalf("building the map: %v", d)
	}
	return v
}

// runModelUpdate calls Update the way the framework does, from prior to
// planned, both pointers to the resource's model, and returns the response.
func runModelUpdate(t *testing.T, r resource.Resource, prior, planned any) resource.UpdateResponse {
	t.Helper()
	ctx := context.Background()
	var sch resource.SchemaResponse
	r.Schema(ctx, resource.SchemaRequest{}, &sch)
	s := sch.Schema
	state := tfsdk.State{Schema: s, Raw: tftypes.NewValue(s.Type().TerraformType(ctx), nil)}
	if d := state.Set(ctx, prior); d.HasError() {
		t.Fatalf("building the state: %v", d)
	}
	plan := tfsdk.Plan{Schema: s, Raw: tftypes.NewValue(s.Type().TerraformType(ctx), nil)}
	if d := plan.Set(ctx, planned); d.HasError() {
		t.Fatalf("building the plan: %v", d)
	}
	resp := resource.UpdateResponse{State: state}
	r.Update(ctx, resource.UpdateRequest{Plan: plan, State: state}, &resp)
	return resp
}

// snapshotWith is snap-1 as state holds it, with metadata meta.
func snapshotWith(t *testing.T, name string, meta map[string]string) snapshotModel {
	t.Helper()
	return snapshotModel{
		ID: types.StringValue("snap-1"), VolumeID: types.StringValue("vol-1"), Name: types.StringValue(name),
		Description: types.StringValue(""), Force: types.BoolValue(false), Metadata: stringMapValue(t, meta),
		Size: types.Int64Value(1), Status: types.StringValue("available"), Region: types.StringValue("region-one"),
	}
}

// metadata has no plan modifier, so a config that leaves it unset plans it
// unknown on any update. A rename used to send that unknown as an empty
// metadata update, which Cinder refuses with 400, so the apply failed after the
// rename had already gone through. A rename must leave the metadata it does not
// manage alone.
func TestSnapshotRenameLeavesUnmanagedMetadataAlone(t *testing.T) {
	t.Parallel()
	cinder := &fakeCinderMetadata{snapName: "nightly", snapMeta: map[string]string{"owner": "ops"}}
	r := &snapshotResource{config: fakeConfig(cinder.serve(t).URL)}

	prior := snapshotWith(t, "nightly", map[string]string{"owner": "ops"})
	planned := prior
	planned.Name = types.StringValue("nightly-renamed")
	planned.Metadata = types.MapUnknown(types.StringType)

	resp := runModelUpdate(t, r, &prior, &planned)
	if resp.Diagnostics.HasError() {
		t.Fatalf("rename: %v", resp.Diagnostics)
	}
	if cinder.snapshotMetadataPUT != 0 {
		t.Fatalf("a rename sent %d metadata update(s); the config does not manage metadata", cinder.snapshotMetadataPUT)
	}
	var got snapshotModel
	if d := resp.State.Get(context.Background(), &got); d.HasError() {
		t.Fatalf("reading the state: %v", d)
	}
	if !got.Metadata.Equal(prior.Metadata) || got.Name.ValueString() != "nightly-renamed" {
		t.Fatalf("state name=%s metadata=%s; want nightly-renamed and the unchanged metadata", got.Name, got.Metadata)
	}
}

// A config that sets metadata = {} asks to clear it. The empty map used to be
// dropped from the request, which Cinder refuses with 400.
func TestSnapshotUpdateClearsMetadata(t *testing.T) {
	t.Parallel()
	cinder := &fakeCinderMetadata{snapName: "nightly", snapMeta: map[string]string{"purpose": "e2e"}}
	r := &snapshotResource{config: fakeConfig(cinder.serve(t).URL)}

	prior := snapshotWith(t, "nightly", map[string]string{"purpose": "e2e"})
	planned := prior
	planned.Metadata = stringMapValue(t, map[string]string{})

	resp := runModelUpdate(t, r, &prior, &planned)
	if resp.Diagnostics.HasError() {
		t.Fatalf("clearing the metadata: %v", resp.Diagnostics)
	}
	if len(cinder.snapMeta) != 0 {
		t.Fatalf("snapshot metadata = %v after clearing, want none", cinder.snapMeta)
	}
}

// The same for a volume: metadata = {} used to be dropped from the update, so
// Cinder kept the old metadata, and the read-back returned it against a plan
// that held none: Terraform failed the apply with "Provider produced
// inconsistent result after apply".
func TestVolumeUpdateClearsMetadata(t *testing.T) {
	t.Parallel()
	cinder := &fakeCinderMetadata{volName: "data", volMeta: map[string]string{"b": "2"}}
	r := &volumeResource{config: fakeConfig(cinder.serve(t).URL)}

	prior := volumeModel{
		ID: types.StringValue("vol-1"), Name: types.StringValue("data"), Size: types.Int64Value(1),
		Description: types.StringValue(""), VolumeType: types.StringValue("nfs"), AvailabilityZone: types.StringValue("nova"),
		SnapshotID: types.StringNull(), SourceVolID: types.StringNull(), ImageID: types.StringNull(),
		Metadata: stringMapValue(t, map[string]string{"b": "2"}), Bootable: types.BoolValue(false),
		Encrypted: types.BoolValue(false), Status: types.StringValue("available"), Region: types.StringValue("region-one"),
	}
	planned := prior
	planned.Metadata = stringMapValue(t, map[string]string{})

	resp := runModelUpdate(t, r, &prior, &planned)
	if resp.Diagnostics.HasError() {
		t.Fatalf("clearing the metadata: %v", resp.Diagnostics)
	}
	var got volumeModel
	if d := resp.State.Get(context.Background(), &got); d.HasError() {
		t.Fatalf("reading the state: %v", d)
	}
	if len(cinder.volMeta) != 0 || !got.Metadata.Equal(planned.Metadata) {
		t.Fatalf("volume metadata = %v, state %s after clearing; want none in both", cinder.volMeta, got.Metadata)
	}
}
