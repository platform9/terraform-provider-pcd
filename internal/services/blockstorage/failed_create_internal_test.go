// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package blockstorage

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"

	"github.com/platform9/terraform-provider-pcd/internal/clients"
)

// fakeConfig points a clients.Config at a test server, so the Cinder client the
// resources build resolves to it. The locator ignores the endpoint options, so
// it answers for every service type and availability.
func fakeConfig(url string) *clients.Config {
	return &clients.Config{
		Region: "region-one",
		Provider: &gophercloud.ProviderClient{
			EndpointLocator: func(gophercloud.EndpointOpts) (string, error) { return url + "/", nil },
		},
	}
}

// A volume whose build ends in "error" stays in Cinder. Create used to return
// without state, so Terraform forgot the volume, the next apply created another,
// and the first had to be deleted by hand. Create must return the error with the
// volume in state (Terraform then taints it), and the refresh and delete a
// destroy runs must remove the volume even though Cinder reports "error".
func TestVolumeCreateKeepsAVolumeThatFailedToBuild(t *testing.T) {
	ctx := context.Background()

	var mu sync.Mutex
	deleteCalled, getsAfterDelete := false, 0
	cinder := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch r.Method + " " + r.URL.Path {
		case "POST /volumes":
			w.WriteHeader(http.StatusAccepted)
			fmt.Fprint(w, `{"volume": {"id": "vol-1", "status": "creating"}}`)
		case "GET /volumes/vol-1":
			if deleteCalled {
				getsAfterDelete++
				if getsAfterDelete > 1 {
					w.WriteHeader(http.StatusNotFound)
					return
				}
			}
			fmt.Fprint(w, `{"volume": {"id": "vol-1", "name": "data-1", "size": 1, "status": "error",
				"description": "", "volume_type": "__DEFAULT__", "availability_zone": "nova",
				"bootable": "false", "encrypted": false, "metadata": {}}}`)
		case "DELETE /volumes/vol-1":
			deleteCalled = true
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotImplemented)
		}
	}))
	defer cinder.Close()

	r := &volumeResource{config: fakeConfig(cinder.URL)}
	var sch resource.SchemaResponse
	r.Schema(ctx, resource.SchemaRequest{}, &sch)
	s := sch.Schema

	planned := volumeModel{
		ID:               types.StringUnknown(),
		Name:             types.StringValue("data-1"),
		Size:             types.Int64Value(1),
		Description:      types.StringUnknown(),
		VolumeType:       types.StringUnknown(),
		AvailabilityZone: types.StringUnknown(),
		SnapshotID:       types.StringNull(),
		SourceVolID:      types.StringNull(),
		ImageID:          types.StringNull(),
		Metadata:         types.MapUnknown(types.StringType),
		Bootable:         types.BoolUnknown(),
		Encrypted:        types.BoolUnknown(),
		Status:           types.StringUnknown(),
		Region:           types.StringUnknown(),
	}
	plan := tfsdk.Plan{Schema: s, Raw: tftypes.NewValue(s.Type().TerraformType(ctx), nil)}
	if d := plan.Set(ctx, &planned); d.HasError() {
		t.Fatalf("building the plan: %v", d)
	}

	createResp := resource.CreateResponse{State: tfsdk.State{Schema: s, Raw: tftypes.NewValue(s.Type().TerraformType(ctx), nil)}}
	r.Create(ctx, resource.CreateRequest{Plan: plan}, &createResp)
	if !createResp.Diagnostics.HasError() {
		t.Fatal("create succeeded; want the error status reported")
	}
	if createResp.State.Raw.IsNull() {
		t.Fatal("create returned no state: Terraform forgets vol-1 and the next apply creates a second volume")
	}
	if !createResp.State.Raw.IsFullyKnown() {
		t.Fatalf("create state holds unknown values, which Terraform refuses: %v", createResp.State.Raw)
	}
	var got volumeModel
	if d := createResp.State.Get(ctx, &got); d.HasError() {
		t.Fatalf("reading the create state: %v", d)
	}
	if got.ID.ValueString() != "vol-1" || got.Name.ValueString() != "data-1" || got.Size.ValueInt64() != 1 {
		t.Fatalf("create state id=%s name=%s size=%d; want vol-1, data-1, 1",
			got.ID, got.Name, got.Size.ValueInt64())
	}

	// terraform destroy (or the replacing apply) refreshes the tainted volume first.
	readResp := resource.ReadResponse{State: createResp.State}
	r.Read(ctx, resource.ReadRequest{State: createResp.State}, &readResp)
	if readResp.Diagnostics.HasError() {
		t.Fatalf("refresh of the failed volume: %v", readResp.Diagnostics)
	}
	if d := readResp.State.Get(ctx, &got); d.HasError() {
		t.Fatalf("reading the refreshed state: %v", d)
	}
	if got.Status.ValueString() != "error" {
		t.Fatalf("refreshed status = %s, want error", got.Status)
	}

	deleteResp := resource.DeleteResponse{State: readResp.State}
	r.Delete(ctx, resource.DeleteRequest{State: readResp.State}, &deleteResp)
	if deleteResp.Diagnostics.HasError() {
		t.Fatalf("delete of a volume in error: %v", deleteResp.Diagnostics)
	}
	mu.Lock()
	defer mu.Unlock()
	if !deleteCalled {
		t.Fatal("delete never called DELETE /volumes/vol-1")
	}
	if getsAfterDelete < 2 {
		t.Fatalf("delete returned after %d polls; want it to wait out the error status until Cinder answers 404", getsAfterDelete)
	}
}

// A snapshot whose creation ends in "error" stays in Cinder. Create must return
// the error with the snapshot in state, and the refresh and delete a destroy
// runs must remove it even though Cinder reports "error".
func TestSnapshotCreateKeepsASnapshotThatFailed(t *testing.T) {
	ctx := context.Background()

	var mu sync.Mutex
	deleteCalled, getsAfterDelete := false, 0
	cinder := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch r.Method + " " + r.URL.Path {
		case "POST /snapshots":
			w.WriteHeader(http.StatusAccepted)
			fmt.Fprint(w, `{"snapshot": {"id": "snap-1", "status": "creating"}}`)
		case "GET /snapshots/snap-1":
			if deleteCalled {
				getsAfterDelete++
				if getsAfterDelete > 1 {
					w.WriteHeader(http.StatusNotFound)
					return
				}
			}
			fmt.Fprint(w, `{"snapshot": {"id": "snap-1", "name": "nightly", "volume_id": "vol-1",
				"status": "error", "description": "", "size": 1, "metadata": {}}}`)
		case "DELETE /snapshots/snap-1":
			deleteCalled = true
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotImplemented)
		}
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

	createResp := resource.CreateResponse{State: tfsdk.State{Schema: s, Raw: tftypes.NewValue(s.Type().TerraformType(ctx), nil)}}
	r.Create(ctx, resource.CreateRequest{Plan: plan}, &createResp)
	if !createResp.Diagnostics.HasError() {
		t.Fatal("create succeeded; want the error status reported")
	}
	if createResp.State.Raw.IsNull() {
		t.Fatal("create returned no state: Terraform forgets snap-1 and the next apply creates a second snapshot")
	}
	if !createResp.State.Raw.IsFullyKnown() {
		t.Fatalf("create state holds unknown values, which Terraform refuses: %v", createResp.State.Raw)
	}
	var got snapshotModel
	if d := createResp.State.Get(ctx, &got); d.HasError() {
		t.Fatalf("reading the create state: %v", d)
	}
	if got.ID.ValueString() != "snap-1" || got.VolumeID.ValueString() != "vol-1" || got.Name.ValueString() != "nightly" {
		t.Fatalf("create state id=%s volume_id=%s name=%s; want snap-1, vol-1, nightly",
			got.ID, got.VolumeID, got.Name)
	}

	readResp := resource.ReadResponse{State: createResp.State}
	r.Read(ctx, resource.ReadRequest{State: createResp.State}, &readResp)
	if readResp.Diagnostics.HasError() {
		t.Fatalf("refresh of the failed snapshot: %v", readResp.Diagnostics)
	}
	if d := readResp.State.Get(ctx, &got); d.HasError() {
		t.Fatalf("reading the refreshed state: %v", d)
	}
	if got.Status.ValueString() != "error" {
		t.Fatalf("refreshed status = %s, want error", got.Status)
	}

	deleteResp := resource.DeleteResponse{State: readResp.State}
	r.Delete(ctx, resource.DeleteRequest{State: readResp.State}, &deleteResp)
	if deleteResp.Diagnostics.HasError() {
		t.Fatalf("delete of a snapshot in error: %v", deleteResp.Diagnostics)
	}
	mu.Lock()
	defer mu.Unlock()
	if !deleteCalled {
		t.Fatal("delete never called DELETE /snapshots/snap-1")
	}
	if getsAfterDelete < 2 {
		t.Fatalf("delete returned after %d polls; want it to wait out the error status until Cinder answers 404", getsAfterDelete)
	}
}
