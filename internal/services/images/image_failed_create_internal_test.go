// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package images

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"

	"github.com/platform9/terraform-provider-pcd/internal/clients"
)

// fakeConfig points a clients.Config at a test server, so the Glance client the
// resource builds resolves to it. The locator ignores the endpoint options, so
// it answers both the admin and the public lookup ImageV2Client tries.
func fakeConfig(url string) *clients.Config {
	return &clients.Config{
		Region: "region-one",
		Provider: &gophercloud.ProviderClient{
			EndpointLocator: func(gophercloud.EndpointOpts) (string, error) { return url + "/", nil },
		},
	}
}

// baseImagePlan holds the fields common to a first apply regardless of image
// source: everything the provider computes is still unknown. The caller sets
// LocalFilePath xor ImageSourceURL, which Create requires.
func baseImagePlan() imageModel {
	return imageModel{
		ID:              types.StringUnknown(),
		Name:            types.StringValue("ubuntu-24.04"),
		ContainerFormat: types.StringValue("bare"),
		DiskFormat:      types.StringValue("qcow2"),
		LocalFilePath:   types.StringNull(),
		ImageSourceURL:  types.StringNull(),
		MinDiskGB:       types.Int64Value(0),
		MinRAMMB:        types.Int64Value(0),
		Protected:       types.BoolValue(false),
		Visibility:      types.StringUnknown(),
		Hidden:          types.BoolValue(false),
		Tags:            types.SetUnknown(types.StringType),
		VerifyChecksum:  types.BoolValue(true),
		Properties:      types.MapUnknown(types.StringType),
		Checksum:        types.StringUnknown(),
		SizeBytes:       types.Int64Unknown(),
		Status:          types.StringUnknown(),
		Owner:           types.StringUnknown(),
		CreatedAt:       types.StringUnknown(),
		UpdatedAt:       types.StringUnknown(),
		Region:          types.StringUnknown(),
	}
}

// buildPlan sets planned onto a fresh tfsdk.Plan using s.
func buildPlan(ctx context.Context, t *testing.T, s schema.Schema, planned imageModel) tfsdk.Plan {
	t.Helper()
	plan := tfsdk.Plan{Schema: s, Raw: tftypes.NewValue(s.Type().TerraformType(ctx), nil)}
	if d := plan.Set(ctx, &planned); d.HasError() {
		t.Fatalf("building the plan: %v", d)
	}
	return plan
}

// imagePlan builds the plan a configuration with image_source_url produces on
// a first apply.
func imagePlan(ctx context.Context, t *testing.T, s schema.Schema) tfsdk.Plan {
	t.Helper()
	planned := baseImagePlan()
	planned.ImageSourceURL = types.StringValue("https://example.invalid/ubuntu.qcow2")
	return buildPlan(ctx, t, s, planned)
}

// imagePlanLocalFile builds the plan a configuration with local_file_path
// produces on a first apply.
func imagePlanLocalFile(ctx context.Context, t *testing.T, s schema.Schema, path string) tfsdk.Plan {
	t.Helper()
	planned := baseImagePlan()
	planned.LocalFilePath = types.StringValue(path)
	return buildPlan(ctx, t, s, planned)
}

// An image whose import Glance gives up on stays in Glance with status "killed".
// Create must return the error with the image in state (Terraform then taints
// it), and the refresh and delete a destroy runs must remove it.
func TestImageCreateKeepsAnImageThatFailedToLoad(t *testing.T) {
	ctx := context.Background()

	var mu sync.Mutex
	deleteCalled := false
	glance := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch r.Method + " " + r.URL.Path {
		case "POST /v2/images":
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{"id": "img-1", "name": "ubuntu-24.04", "status": "queued",
				"container_format": "bare", "disk_format": "qcow2", "visibility": "shared",
				"protected": false, "os_hidden": false, "tags": [], "min_disk": 0, "min_ram": 0,
				"owner": "proj-1", "created_at": "2026-09-19T00:00:00Z", "updated_at": "2026-09-19T00:00:00Z"}`)
		case "POST /v2/images/img-1/import":
			w.WriteHeader(http.StatusAccepted)
		case "GET /v2/images/img-1":
			if deleteCalled {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			fmt.Fprint(w, `{"id": "img-1", "name": "ubuntu-24.04", "status": "killed",
				"container_format": "bare", "disk_format": "qcow2", "visibility": "shared",
				"protected": false, "os_hidden": false, "tags": [], "min_disk": 0, "min_ram": 0,
				"owner": "proj-1", "created_at": "2026-09-19T00:00:00Z", "updated_at": "2026-09-19T00:00:00Z"}`)
		case "DELETE /v2/images/img-1":
			deleteCalled = true
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotImplemented)
		}
	}))
	defer glance.Close()

	r := &imageResource{config: fakeConfig(glance.URL)}
	var sch resource.SchemaResponse
	r.Schema(ctx, resource.SchemaRequest{}, &sch)
	plan := imagePlan(ctx, t, sch.Schema)

	createResp := resource.CreateResponse{State: tfsdk.State{Schema: sch.Schema, Raw: tftypes.NewValue(sch.Schema.Type().TerraformType(ctx), nil)}}
	r.Create(ctx, resource.CreateRequest{Plan: plan}, &createResp)
	if !createResp.Diagnostics.HasError() {
		t.Fatal("create succeeded; want the killed image reported")
	}
	if createResp.State.Raw.IsNull() {
		t.Fatal("create returned no state: Terraform forgets img-1 and the next apply creates a second image")
	}
	if !createResp.State.Raw.IsFullyKnown() {
		t.Fatalf("create state holds unknown values, which Terraform refuses: %v", createResp.State.Raw)
	}
	var got imageModel
	if d := createResp.State.Get(ctx, &got); d.HasError() {
		t.Fatalf("reading the create state: %v", d)
	}
	if got.ID.ValueString() != "img-1" || got.Name.ValueString() != "ubuntu-24.04" {
		t.Fatalf("create state id=%s name=%s; want img-1, ubuntu-24.04", got.ID, got.Name)
	}

	readResp := resource.ReadResponse{State: createResp.State}
	r.Read(ctx, resource.ReadRequest{State: createResp.State}, &readResp)
	if readResp.Diagnostics.HasError() {
		t.Fatalf("refresh of the failed image: %v", readResp.Diagnostics)
	}
	if d := readResp.State.Get(ctx, &got); d.HasError() {
		t.Fatalf("reading the refreshed state: %v", d)
	}
	if got.Status.ValueString() != "killed" {
		t.Fatalf("refreshed status = %s, want killed", got.Status)
	}

	deleteResp := resource.DeleteResponse{State: readResp.State}
	r.Delete(ctx, resource.DeleteRequest{State: readResp.State}, &deleteResp)
	if deleteResp.Diagnostics.HasError() {
		t.Fatalf("delete of a killed image: %v", deleteResp.Diagnostics)
	}
	mu.Lock()
	defer mu.Unlock()
	if !deleteCalled {
		t.Fatal("delete never called DELETE /v2/images/img-1")
	}
}

// When the import request fails, Create deletes the image it created. That
// delete succeeds here, so nothing is left in Glance and nothing may be left in
// state either.
func TestImageCreateDropsTheStateOfAnImageItDeleted(t *testing.T) {
	ctx := context.Background()

	var mu sync.Mutex
	deleteCalled := false
	glance := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch r.Method + " " + r.URL.Path {
		case "POST /v2/images":
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{"id": "img-1", "name": "ubuntu-24.04", "status": "queued",
				"container_format": "bare", "disk_format": "qcow2", "visibility": "shared",
				"protected": false, "os_hidden": false, "tags": [], "min_disk": 0, "min_ram": 0,
				"owner": "proj-1", "created_at": "2026-09-19T00:00:00Z", "updated_at": "2026-09-19T00:00:00Z"}`)
		case "POST /v2/images/img-1/import":
			w.WriteHeader(http.StatusInternalServerError)
		case "DELETE /v2/images/img-1":
			deleteCalled = true
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotImplemented)
		}
	}))
	defer glance.Close()

	r := &imageResource{config: fakeConfig(glance.URL)}
	var sch resource.SchemaResponse
	r.Schema(ctx, resource.SchemaRequest{}, &sch)
	plan := imagePlan(ctx, t, sch.Schema)

	createResp := resource.CreateResponse{State: tfsdk.State{Schema: sch.Schema, Raw: tftypes.NewValue(sch.Schema.Type().TerraformType(ctx), nil)}}
	r.Create(ctx, resource.CreateRequest{Plan: plan}, &createResp)
	if !createResp.Diagnostics.HasError() {
		t.Fatal("create succeeded; want the failed import request reported")
	}
	mu.Lock()
	called := deleteCalled
	mu.Unlock()
	if !called {
		t.Fatal("create never deleted the image it had created")
	}
	if !createResp.State.Raw.IsNull() {
		t.Fatalf("create left state for an image it deleted: %v", createResp.State.Raw)
	}
}

// When the import request fails and the cleanup delete fails too, the image is
// still in Glance. Dropping the state would orphan it, so the state stays and
// Terraform taints the image for the next destroy.
func TestImageCreateKeepsTheStateOfAnImageItCouldNotDelete(t *testing.T) {
	ctx := context.Background()

	glance := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "POST /v2/images":
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{"id": "img-1", "name": "ubuntu-24.04", "status": "queued",
				"container_format": "bare", "disk_format": "qcow2", "visibility": "shared",
				"protected": false, "os_hidden": false, "tags": [], "min_disk": 0, "min_ram": 0,
				"owner": "proj-1", "created_at": "2026-09-19T00:00:00Z", "updated_at": "2026-09-19T00:00:00Z"}`)
		case "POST /v2/images/img-1/import":
			w.WriteHeader(http.StatusInternalServerError)
		case "DELETE /v2/images/img-1":
			w.WriteHeader(http.StatusInternalServerError)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotImplemented)
		}
	}))
	defer glance.Close()

	r := &imageResource{config: fakeConfig(glance.URL)}
	var sch resource.SchemaResponse
	r.Schema(ctx, resource.SchemaRequest{}, &sch)
	plan := imagePlan(ctx, t, sch.Schema)

	createResp := resource.CreateResponse{State: tfsdk.State{Schema: sch.Schema, Raw: tftypes.NewValue(sch.Schema.Type().TerraformType(ctx), nil)}}
	r.Create(ctx, resource.CreateRequest{Plan: plan}, &createResp)
	if !createResp.Diagnostics.HasError() {
		t.Fatal("create succeeded; want the failed import request reported")
	}
	if createResp.State.Raw.IsNull() {
		t.Fatal("create dropped the state of an image its cleanup delete could not remove: the image is orphaned in Glance")
	}
	var got imageModel
	if d := createResp.State.Get(ctx, &got); d.HasError() {
		t.Fatalf("reading the create state: %v", d)
	}
	if got.ID.ValueString() != "img-1" {
		t.Fatalf("create state id = %s, want img-1", got.ID)
	}
}

// When the local_file_path upload fails, Create deletes the image it created,
// the same as the image_source_url import-failure path above. That delete
// succeeds here, so nothing is left in Glance and nothing may be left in state
// either. Every other test in this file drives image_source_url; this is the
// only one that exercises the local-file branch of deleteCreatedImage.
func TestImageCreateKeepsAnImageThatFailedToUpload(t *testing.T) {
	ctx := context.Background()

	localPath := filepath.Join(t.TempDir(), "ubuntu.qcow2")
	if err := os.WriteFile(localPath, []byte("not a real qcow2, just test data"), 0o600); err != nil {
		t.Fatalf("writing the local image file: %v", err)
	}

	var mu sync.Mutex
	deleteCalled := false
	glance := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch r.Method + " " + r.URL.Path {
		case "POST /v2/images":
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{"id": "img-1", "name": "ubuntu-24.04", "status": "queued",
				"container_format": "bare", "disk_format": "qcow2", "visibility": "shared",
				"protected": false, "os_hidden": false, "tags": [], "min_disk": 0, "min_ram": 0,
				"owner": "proj-1", "created_at": "2026-09-19T00:00:00Z", "updated_at": "2026-09-19T00:00:00Z"}`)
		case "PUT /v2/images/img-1/file":
			w.WriteHeader(http.StatusInternalServerError)
		case "DELETE /v2/images/img-1":
			deleteCalled = true
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotImplemented)
		}
	}))
	defer glance.Close()

	r := &imageResource{config: fakeConfig(glance.URL)}
	var sch resource.SchemaResponse
	r.Schema(ctx, resource.SchemaRequest{}, &sch)
	plan := imagePlanLocalFile(ctx, t, sch.Schema, localPath)

	createResp := resource.CreateResponse{State: tfsdk.State{Schema: sch.Schema, Raw: tftypes.NewValue(sch.Schema.Type().TerraformType(ctx), nil)}}
	r.Create(ctx, resource.CreateRequest{Plan: plan}, &createResp)
	if !createResp.Diagnostics.HasError() {
		t.Fatal("create succeeded; want the failed upload reported")
	}
	mu.Lock()
	called := deleteCalled
	mu.Unlock()
	if !called {
		t.Fatal("create never deleted the image it had created")
	}
	if !createResp.State.Raw.IsNull() {
		t.Fatalf("create left state for an image it deleted: %v", createResp.State.Raw)
	}
}
