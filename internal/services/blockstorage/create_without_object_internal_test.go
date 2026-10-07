// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package blockstorage

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

// gophercloud decodes a snapshot create answer whose body lacks the snapshot
// key to a nil snapshot and no error. Create used to dereference it and crash
// the provider. It must report an error and leave nothing in state.
func TestSnapshotCreateRefusesAnAnswerWithoutTheSnapshot(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	cinder := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method+" "+r.URL.Path != "POST /snapshots" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotImplemented)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		fmt.Fprint(w, `{}`)
	}))
	defer cinder.Close()

	r := &snapshotResource{config: fakeConfig(cinder.URL)}
	var sch resource.SchemaResponse
	r.Schema(ctx, resource.SchemaRequest{}, &sch)
	s := sch.Schema
	planned := snapshotModel{
		ID:          types.StringUnknown(),
		VolumeID:    types.StringValue("vol-1"),
		Name:        types.StringValue("nightly"),
		Description: types.StringUnknown(),
		Force:       types.BoolValue(false),
		Metadata:    types.MapUnknown(types.StringType),
		Size:        types.Int64Unknown(),
		Status:      types.StringUnknown(),
		Region:      types.StringUnknown(),
	}
	plan := tfsdk.Plan{Schema: s, Raw: tftypes.NewValue(s.Type().TerraformType(ctx), nil)}
	if d := plan.Set(ctx, &planned); d.HasError() {
		t.Fatalf("building the plan: %v", d)
	}

	resp := resource.CreateResponse{State: tfsdk.State{Schema: s, Raw: tftypes.NewValue(s.Type().TerraformType(ctx), nil)}}
	func() {
		defer func() {
			if p := recover(); p != nil {
				t.Fatalf("create panicked: %v", p)
			}
		}()
		r.Create(ctx, resource.CreateRequest{Plan: plan}, &resp)
	}()
	if !resp.Diagnostics.HasError() {
		t.Fatal("create succeeded; want an error for an answer without the snapshot")
	}
	if !resp.State.Raw.IsNull() {
		t.Fatalf("create left a row: %v", resp.State.Raw)
	}
}
