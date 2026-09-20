# Create Records Its Object Before It Waits — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make `pcd_blockstorage_volume`, `pcd_blockstorage_snapshot`, `pcd_blockstorage_volume_backup`, and `pcd_images_image` record the created object in Terraform state as soon as the API accepts it, so a failed or interrupted status wait leaves a tainted resource instead of an orphan.

**Architecture:** Each `Create` sets the new object's ID on the plan, writes the plan to `resp.State`, and replaces the still-unknown values with null before it starts waiting. The `nullUnknowns` helper that `pcd_compute_instance` already uses moves out of `package compute` into a new `internal/tfstate` package so all five resources share one copy. `pcd_images_image` records its image before the data upload rather than before the wait, which means its two existing "delete the image and fail" paths must also clear the state they would otherwise leave behind.

**Tech Stack:** Go 1.25.8, terraform-plugin-framework, gophercloud v2.13.0 (Cinder v3, Glance v2), `net/http/httptest` for the service fakes.

**Spec:** `docs/superpowers/specs/2026-09-19-create-state-before-wait-design.md`

## Global Constraints

- Branch is `pushkar/create-state-before-wait`, already created and based on `pushkar/instance-failed-create`. Do not rebase it and do not touch `pushkar/instance-failed-create`.
- Commit messages and any PR text carry **no** `Co-Authored-By: Claude` trailer and **no** Claude Code footer. Write them as the repository owner would.
- American English throughout code comments, the changelog, and commit messages.
- Module path is `github.com/platform9/terraform-provider-pcd`.
- Unit tests run with `go test ./internal/... -timeout 120s` (the `make test` target and what CI runs in `.github/workflows/test.yml`).
- Lint runs with `golangci-lint run ./...` (`make lint`).
- Every file gets the repository's two-line header:
  ```go
  // Copyright (c) Platform9 Systems, Inc.
  // SPDX-License-Identifier: MPL-2.0
  ```
- Do not change any create wait's target status, timeout, or error message. Do not change which statuses the waiters treat as failures.
- The timeout-in-`creating` delete gap described in the spec's **Known gap** section is deliberately out of scope. Do not add force-delete or retry logic.

### Facts the tests depend on (verified against gophercloud v2.13.0)

| Call | Method and path | Status the fake must return |
| --- | --- | --- |
| `volumes.Create` | `POST /volumes` | **202**, body `{"volume": {...}}` |
| `volumes.Get` | `GET /volumes/{id}` | 200, body `{"volume": {...}}` |
| `volumes.Delete` | `DELETE /volumes/{id}` | 202 or 204 |
| `snapshots.Create` | `POST /snapshots` | **202**, body `{"snapshot": {...}}` |
| `snapshots.Get` | `GET /snapshots/{id}` | 200, body `{"snapshot": {...}}` |
| `snapshots.Delete` | `DELETE /snapshots/{id}` | 202 or 204 |
| `backups.Create` | `POST /backups` | **202**, body `{"backup": {...}}` |
| `backups.Get` | `GET /backups/{id}` | 200, body `{"backup": {...}}` |
| `backups.Delete` | `DELETE /backups/{id}` | 202 or 204 |
| `images.Create` | `POST /v2/images` | **201**, body is the image object, unwrapped |
| `imageimport.Create` | `POST /v2/images/{id}/import` | **202**, empty body |
| `images.Get` | `GET /v2/images/{id}` | 200, body is the image object, unwrapped |
| `images.Delete` | `DELETE /v2/images/{id}` | 202 or 204 |

Glance paths carry the `/v2/` prefix because `openstack.NewImageV2` sets `ResourceBase = Endpoint + "v2/"`. Cinder paths do not, because `openstack.NewBlockStorageV3` leaves `ResourceBase` empty.

---

### Task 1: The shared `NullUnknowns` helper

**Files:**
- Create: `internal/tfstate/tfstate.go`
- Create: `internal/tfstate/tfstate_test.go`
- Modify: `internal/services/compute/instance_resource.go` (remove `nullUnknowns` at lines 786-801, its two imports, and update the call site at line 519)

**Interfaces:**
- Consumes: nothing.
- Produces: `tfstate.NullUnknowns(state *tfsdk.State) diag.Diagnostics` — replaces every unknown value in `state.Raw` with null in place, and returns error diagnostics only if the walk fails. Tasks 2 through 5 call it.

- [ ] **Step 1: Write the failing test**

Create `internal/tfstate/tfstate_test.go`:

```go
// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package tfstate_test

import (
	"context"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"

	"github.com/platform9/terraform-provider-pcd/internal/tfstate"
)

type sampleModel struct {
	ID    types.String `tfsdk:"id"`
	Name  types.String `tfsdk:"name"`
	Tags  types.Set    `tfsdk:"tags"`
	Count types.Int64  `tfsdk:"count"`
}

// A Create that records its object before the API has reported the object's
// computed attributes leaves them unknown, and Terraform refuses unknown values
// in state. NullUnknowns must replace exactly those, leaving known and null
// values alone.
func TestNullUnknownsReplacesOnlyTheUnknowns(t *testing.T) {
	ctx := context.Background()
	s := schema.Schema{Attributes: map[string]schema.Attribute{
		"id":    schema.StringAttribute{Computed: true},
		"name":  schema.StringAttribute{Optional: true},
		"tags":  schema.SetAttribute{Computed: true, ElementType: types.StringType},
		"count": schema.Int64Attribute{Computed: true},
	}}

	state := tfsdk.State{Schema: s, Raw: tftypes.NewValue(s.Type().TerraformType(ctx), nil)}
	if d := state.Set(ctx, &sampleModel{
		ID:    types.StringValue("vol-1"),
		Name:  types.StringNull(),
		Tags:  types.SetUnknown(types.StringType),
		Count: types.Int64Unknown(),
	}); d.HasError() {
		t.Fatalf("building the state: %v", d)
	}
	if state.Raw.IsFullyKnown() {
		t.Fatal("test setup is wrong: the state holds no unknowns to replace")
	}

	if d := tfstate.NullUnknowns(&state); d.HasError() {
		t.Fatalf("NullUnknowns: %v", d)
	}

	if !state.Raw.IsFullyKnown() {
		t.Fatalf("state still holds unknown values, which Terraform refuses: %v", state.Raw)
	}
	var got sampleModel
	if d := state.Get(ctx, &got); d.HasError() {
		t.Fatalf("reading the state back: %v", d)
	}
	if got.ID.ValueString() != "vol-1" {
		t.Fatalf("id = %s, want vol-1: a known value must survive", got.ID)
	}
	if !got.Name.IsNull() {
		t.Fatalf("name = %s, want null: a null value must stay null", got.Name)
	}
	if !got.Tags.IsNull() {
		t.Fatalf("tags = %s, want null", got.Tags)
	}
	if !got.Count.IsNull() {
		t.Fatalf("count = %s, want null", got.Count)
	}
}

// A state with nothing unknown must come through untouched.
func TestNullUnknownsLeavesAFullyKnownStateAlone(t *testing.T) {
	ctx := context.Background()
	s := schema.Schema{Attributes: map[string]schema.Attribute{
		"id":    schema.StringAttribute{Computed: true},
		"name":  schema.StringAttribute{Optional: true},
		"tags":  schema.SetAttribute{Computed: true, ElementType: types.StringType},
		"count": schema.Int64Attribute{Computed: true},
	}}
	tags, d := types.SetValueFrom(ctx, types.StringType, []string{"a"})
	if d.HasError() {
		t.Fatalf("building tags: %v", d)
	}

	state := tfsdk.State{Schema: s, Raw: tftypes.NewValue(s.Type().TerraformType(ctx), nil)}
	if d := state.Set(ctx, &sampleModel{
		ID:    types.StringValue("vol-1"),
		Name:  types.StringValue("data-1"),
		Tags:  tags,
		Count: types.Int64Value(3),
	}); d.HasError() {
		t.Fatalf("building the state: %v", d)
	}
	before := state.Raw.String()

	if d := tfstate.NullUnknowns(&state); d.HasError() {
		t.Fatalf("NullUnknowns: %v", d)
	}
	if state.Raw.String() != before {
		t.Fatalf("state changed:\n before %s\n after  %s", before, state.Raw.String())
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/tfstate/... -v`
Expected: build failure — `no required module provides package github.com/platform9/terraform-provider-pcd/internal/tfstate`.

- [ ] **Step 3: Write the implementation**

Create `internal/tfstate/tfstate.go`. The body is the `nullUnknowns` walk from `internal/services/compute/instance_resource.go` unchanged; only the name, the doc comment, and the diagnostic summary differ.

```go
// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

// Package tfstate holds helpers for resources that write Terraform state
// outside the usual happy path.
package tfstate

import (
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

// NullUnknowns replaces every unknown value in state with null. Terraform
// refuses unknown values in state, and a Create that records its object before
// the API has reported the object's computed attributes leaves them unknown.
// The next refresh reads the real values.
func NullUnknowns(state *tfsdk.State) diag.Diagnostics {
	raw, err := tftypes.Transform(state.Raw, func(_ *tftypes.AttributePath, v tftypes.Value) (tftypes.Value, error) {
		if v.IsKnown() {
			return v, nil
		}
		return tftypes.NewValue(v.Type(), nil), nil
	})
	if err != nil {
		return diag.Diagnostics{diag.NewErrorDiagnostic("Preparing Terraform state", err.Error())}
	}
	state.Raw = raw
	return nil
}
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `go test ./internal/tfstate/... -v`
Expected: PASS for both tests.

- [ ] **Step 5: Switch `pcd_compute_instance` onto the shared helper**

In `internal/services/compute/instance_resource.go`:

1. Delete the whole `nullUnknowns` function (the doc comment plus the function, currently lines 786-801):

```go
// nullUnknowns replaces every unknown value in state with null. Terraform
// refuses unknown values in state, and Create saves the planned instance before
// Nova has reported its computed attributes.
func nullUnknowns(state *tfsdk.State) diag.Diagnostics {
	raw, err := tftypes.Transform(state.Raw, func(_ *tftypes.AttributePath, v tftypes.Value) (tftypes.Value, error) {
		if v.IsKnown() {
			return v, nil
		}
		return tftypes.NewValue(v.Type(), nil), nil
	})
	if err != nil {
		return diag.Diagnostics{diag.NewErrorDiagnostic("compute: preparing instance state", err.Error())}
	}
	state.Raw = raw
	return nil
}
```

2. Change the call site in `Create` from `nullUnknowns(&resp.State)` to `tfstate.NullUnknowns(&resp.State)`:

```go
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
	resp.Diagnostics.Append(tfstate.NullUnknowns(&resp.State)...)
```

3. Fix the imports. `tfsdk` and `tftypes` were used only by the deleted function, so remove both lines:

```go
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
```

and add, in the final import group alongside `internal/clients`:

```go
	"github.com/platform9/terraform-provider-pcd/internal/tfstate"
```

Leave the `diag` import: other functions in the file still use it.

- [ ] **Step 6: Run the full unit suite**

Run: `go test ./internal/... -timeout 120s`
Expected: PASS, including `TestCreateKeepsAServerThatFailedToBoot` in `internal/services/compute`, which now exercises the shared helper.

- [ ] **Step 7: Vet and lint**

Run: `go vet ./... && golangci-lint run ./...`
Expected: no findings.

- [ ] **Step 8: Commit**

```bash
git add internal/tfstate/tfstate.go internal/tfstate/tfstate_test.go internal/services/compute/instance_resource.go
git commit -m "refactor: share nullUnknowns as internal/tfstate.NullUnknowns

Four more Creates are about to record their object in state before waiting
for it, and each needs the same walk that replaces unknown values with null.
Move the helper pcd_compute_instance added out of package compute so there is
one copy rather than five."
```

---

### Task 2: `pcd_blockstorage_volume`

**Files:**
- Create: `internal/services/blockstorage/failed_create_internal_test.go`
- Modify: `internal/services/blockstorage/volume_resource.go` (`Create`, around lines 127-136)

**Interfaces:**
- Consumes: `tfstate.NullUnknowns(state *tfsdk.State) diag.Diagnostics` from Task 1.
- Produces: `fakeConfig(url string) *clients.Config` in `package blockstorage`'s internal test files — Tasks 3 and 4 reuse it rather than redefining it.

- [ ] **Step 1: Write the failing test**

Create `internal/services/blockstorage/failed_create_internal_test.go`. This file also defines `fakeConfig`, which Tasks 3 and 4 use.

```go
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
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/services/blockstorage/ -run TestVolumeCreateKeepsAVolumeThatFailedToBuild -v`
Expected: FAIL at `create returned no state: Terraform forgets vol-1 and the next apply creates a second volume`.

- [ ] **Step 3: Write the implementation**

In `internal/services/blockstorage/volume_resource.go`, add the import in the group that holds `internal/clients`:

```go
	"github.com/platform9/terraform-provider-pcd/internal/tfstate"
```

Then in `Create`, insert the block between the create call and the wait. Before:

```go
	vol, err := volumes.Create(ctx, client, createOpts, nil).Extract()
	if err != nil {
		resp.Diagnostics.AddError("blockstorage: creating volume", err.Error())
		return
	}

	if _, err := waitForVolumeStatus(ctx, client, vol.ID, "available", 20*time.Minute); err != nil {
```

After:

```go
	vol, err := volumes.Create(ctx, client, createOpts, nil).Extract()
	if err != nil {
		resp.Diagnostics.AddError("blockstorage: creating volume", err.Error())
		return
	}

	plan.ID = types.StringValue(vol.ID)
	// Cinder keeps a volume whose build fails (status "error"), so record it
	// before waiting. A failed or interrupted wait then returns its error with
	// the volume in state, Terraform marks it tainted, and the next apply or a
	// destroy deletes it instead of leaving it behind and creating another. The
	// attributes Cinder has not reported yet are saved as null; the next refresh
	// reads them.
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
	resp.Diagnostics.Append(tfstate.NullUnknowns(&resp.State)...)
	if resp.Diagnostics.HasError() {
		return
	}

	if _, err := waitForVolumeStatus(ctx, client, vol.ID, "available", 20*time.Minute); err != nil {
```

Nothing else in `Create` changes: the success path still does its own `volumes.Get`, `flatten`, and `resp.State.Set`.

- [ ] **Step 4: Run the test to verify it passes**

Run: `go test ./internal/services/blockstorage/ -run TestVolumeCreateKeepsAVolumeThatFailedToBuild -v`
Expected: PASS. It takes about 3 seconds, because the delete waiter sleeps 3 seconds between its two polls.

- [ ] **Step 5: Run the full unit suite**

Run: `go test ./internal/... -timeout 120s`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/services/blockstorage/volume_resource.go internal/services/blockstorage/failed_create_internal_test.go
git commit -m "fix(blockstorage): keep a pcd_blockstorage_volume that fails to build in state

A volume whose build ends in \"error\" stays in Cinder, but Create returned the
wait error without state, so Terraform lost track of it: the next apply created
a second volume and the first had to be deleted through the API.

Create now records the volume as soon as Cinder accepts it, before waiting for
\"available\". A build that fails, a timeout, or an interrupted apply returns its
error with the volume in state, and Terraform marks it tainted: the next apply
replaces it and a destroy deletes it. The computed attributes still unknown in
the plan are saved as null, since Terraform refuses unknown values in state, and
the next refresh reads them from Cinder.

A unit test drives Create against a fake Cinder whose volume enters \"error\",
then refreshes and deletes the resulting state."
```

---

### Task 3: `pcd_blockstorage_snapshot`

**Files:**
- Modify: `internal/services/blockstorage/failed_create_internal_test.go` (append a test)
- Modify: `internal/services/blockstorage/snapshot_resource.go` (`Create`, around lines 104-110)

**Interfaces:**
- Consumes: `tfstate.NullUnknowns` from Task 1; `fakeConfig(url string) *clients.Config` from Task 2.
- Produces: nothing new.

- [ ] **Step 1: Write the failing test**

Append to `internal/services/blockstorage/failed_create_internal_test.go`. All imports it needs are already in the file from Task 2.

```go
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
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/services/blockstorage/ -run TestSnapshotCreateKeepsASnapshotThatFailed -v`
Expected: FAIL at `create returned no state: Terraform forgets snap-1 and the next apply creates a second snapshot`.

- [ ] **Step 3: Write the implementation**

In `internal/services/blockstorage/snapshot_resource.go`, add the import in the group that holds `internal/clients`:

```go
	"github.com/platform9/terraform-provider-pcd/internal/tfstate"
```

Then in `Create`, insert the block between the create call and the wait. Before:

```go
	if err != nil {
		resp.Diagnostics.AddError("blockstorage: creating snapshot", err.Error())
		return
	}

	final, err := waitForSnapshotStatus(ctx, client, snap.ID, "available", 20*time.Minute)
```

After:

```go
	if err != nil {
		resp.Diagnostics.AddError("blockstorage: creating snapshot", err.Error())
		return
	}

	plan.ID = types.StringValue(snap.ID)
	// Cinder keeps a snapshot whose creation fails (status "error"), so record it
	// before waiting. A failed or interrupted wait then returns its error with
	// the snapshot in state, Terraform marks it tainted, and the next apply or a
	// destroy deletes it instead of leaving it behind and creating another. The
	// attributes Cinder has not reported yet are saved as null; the next refresh
	// reads them.
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
	resp.Diagnostics.Append(tfstate.NullUnknowns(&resp.State)...)
	if resp.Diagnostics.HasError() {
		return
	}

	final, err := waitForSnapshotStatus(ctx, client, snap.ID, "available", 20*time.Minute)
```

Nothing else in `Create` changes: the success path still calls `setState` with the object the waiter fetched.

- [ ] **Step 4: Run the test to verify it passes**

Run: `go test ./internal/services/blockstorage/ -run TestSnapshotCreateKeepsASnapshotThatFailed -v`
Expected: PASS, in about 3 seconds.

- [ ] **Step 5: Run the full unit suite**

Run: `go test ./internal/... -timeout 120s`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/services/blockstorage/snapshot_resource.go internal/services/blockstorage/failed_create_internal_test.go
git commit -m "fix(blockstorage): keep a pcd_blockstorage_snapshot that fails in state

A snapshot whose creation ends in \"error\" stays in Cinder, but Create returned
the wait error without state, so Terraform lost track of it: the next apply
created a second snapshot and the first had to be deleted through the API.

Create now records the snapshot as soon as Cinder accepts it, before waiting for
\"available\", so a failure, a timeout, or an interrupted apply leaves a tainted
resource the next apply or a destroy removes.

A unit test drives Create against a fake Cinder whose snapshot enters \"error\",
then refreshes and deletes the resulting state."
```

---

### Task 4: `pcd_blockstorage_volume_backup`

**Files:**
- Modify: `internal/services/blockstorage/failed_create_internal_test.go` (append a test)
- Modify: `internal/services/blockstorage/backup_resource.go` (`Create`, around lines 107-113)

**Interfaces:**
- Consumes: `tfstate.NullUnknowns` from Task 1; `fakeConfig(url string) *clients.Config` from Task 2.
- Produces: nothing new.

- [ ] **Step 1: Write the failing test**

Append to `internal/services/blockstorage/failed_create_internal_test.go`. All imports it needs are already in the file from Task 2.

Note the fake's `POST /backups` body: Cinder's create response carries only the ID and name, which is exactly why the backup must be recorded from the plan rather than from a flattened create response.

```go
// A backup whose run ends in "error" stays in Cinder. Create must return the
// error with the backup in state, and the refresh and delete a destroy runs must
// remove it even though Cinder reports "error".
func TestBackupCreateKeepsABackupThatFailed(t *testing.T) {
	ctx := context.Background()

	var mu sync.Mutex
	deleteCalled, getsAfterDelete := false, 0
	cinder := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch r.Method + " " + r.URL.Path {
		case "POST /backups":
			w.WriteHeader(http.StatusAccepted)
			fmt.Fprint(w, `{"backup": {"id": "bkp-1", "name": "weekly"}}`)
		case "GET /backups/bkp-1":
			if deleteCalled {
				getsAfterDelete++
				if getsAfterDelete > 1 {
					w.WriteHeader(http.StatusNotFound)
					return
				}
			}
			fmt.Fprint(w, `{"backup": {"id": "bkp-1", "name": "weekly", "volume_id": "vol-1",
				"status": "error", "description": "", "size": 1, "container": "volumebackups",
				"is_incremental": false, "fail_reason": "Backup driver could not reach the store"}}`)
		case "DELETE /backups/bkp-1":
			deleteCalled = true
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotImplemented)
		}
	}))
	defer cinder.Close()

	r := &backupResource{config: fakeConfig(cinder.URL)}
	var sch resource.SchemaResponse
	r.Schema(ctx, resource.SchemaRequest{}, &sch)
	s := sch.Schema

	planned := backupModel{
		ID:          types.StringUnknown(),
		VolumeID:    types.StringValue("vol-1"),
		Name:        types.StringValue("weekly"),
		Description: types.StringUnknown(),
		Force:       types.BoolValue(false),
		Incremental: types.BoolValue(false),
		Container:   types.StringUnknown(),
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
		t.Fatal("create returned no state: Terraform forgets bkp-1 and the next apply creates a second backup")
	}
	if !createResp.State.Raw.IsFullyKnown() {
		t.Fatalf("create state holds unknown values, which Terraform refuses: %v", createResp.State.Raw)
	}
	var got backupModel
	if d := createResp.State.Get(ctx, &got); d.HasError() {
		t.Fatalf("reading the create state: %v", d)
	}
	if got.ID.ValueString() != "bkp-1" || got.VolumeID.ValueString() != "vol-1" || got.Name.ValueString() != "weekly" {
		t.Fatalf("create state id=%s volume_id=%s name=%s; want bkp-1, vol-1, weekly",
			got.ID, got.VolumeID, got.Name)
	}

	readResp := resource.ReadResponse{State: createResp.State}
	r.Read(ctx, resource.ReadRequest{State: createResp.State}, &readResp)
	if readResp.Diagnostics.HasError() {
		t.Fatalf("refresh of the failed backup: %v", readResp.Diagnostics)
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
		t.Fatalf("delete of a backup in error: %v", deleteResp.Diagnostics)
	}
	mu.Lock()
	defer mu.Unlock()
	if !deleteCalled {
		t.Fatal("delete never called DELETE /backups/bkp-1")
	}
	if getsAfterDelete < 2 {
		t.Fatalf("delete returned after %d polls; want it to wait out the error status until Cinder answers 404", getsAfterDelete)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/services/blockstorage/ -run TestBackupCreateKeepsABackupThatFailed -v`
Expected: FAIL at `create returned no state: Terraform forgets bkp-1 and the next apply creates a second backup`.

- [ ] **Step 3: Write the implementation**

In `internal/services/blockstorage/backup_resource.go`, add the import in the group that holds `internal/clients`:

```go
	"github.com/platform9/terraform-provider-pcd/internal/tfstate"
```

Then in `Create`, insert the block between the create call and the wait. Before:

```go
	if err != nil {
		resp.Diagnostics.AddError("blockstorage: creating volume backup", err.Error())
		return
	}

	final, err := waitForBackupStatus(ctx, client, backup.ID, "available", 30*time.Minute)
```

After:

```go
	if err != nil {
		resp.Diagnostics.AddError("blockstorage: creating volume backup", err.Error())
		return
	}

	plan.ID = types.StringValue(backup.ID)
	// Cinder keeps a backup whose run fails (status "error"), so record it before
	// waiting. A failed or interrupted wait then returns its error with the
	// backup in state, Terraform marks it tainted, and the next apply or a
	// destroy deletes it instead of leaving it behind and creating another. The
	// attributes Cinder has not reported yet are saved as null; the next refresh
	// reads them.
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
	resp.Diagnostics.Append(tfstate.NullUnknowns(&resp.State)...)
	if resp.Diagnostics.HasError() {
		return
	}

	final, err := waitForBackupStatus(ctx, client, backup.ID, "available", 30*time.Minute)
```

Nothing else in `Create` changes: the success path still calls `setState` with the object the waiter fetched.

- [ ] **Step 4: Run the test to verify it passes**

Run: `go test ./internal/services/blockstorage/ -run TestBackupCreateKeepsABackupThatFailed -v`
Expected: PASS, in about 3 seconds.

- [ ] **Step 5: Run the full unit suite**

Run: `go test ./internal/... -timeout 120s`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/services/blockstorage/backup_resource.go internal/services/blockstorage/failed_create_internal_test.go
git commit -m "fix(blockstorage): keep a pcd_blockstorage_volume_backup that fails in state

A backup whose run ends in \"error\" stays in Cinder, but Create returned the wait
error without state, so Terraform lost track of it: the next apply created a
second backup and the first had to be deleted through the API.

Create now records the backup as soon as Cinder accepts it, before waiting for
\"available\", so a failure, a timeout, or an interrupted apply leaves a tainted
resource the next apply or a destroy removes. Cinder's create response carries
only the backup's ID and name, so the record comes from the plan with the
computed attributes saved as null until the next refresh.

A unit test drives Create against a fake Cinder whose backup enters \"error\",
then refreshes and deletes the resulting state."
```

---

### Task 5: `pcd_images_image`

**Files:**
- Create: `internal/services/images/image_failed_create_internal_test.go`
- Modify: `internal/services/images/image_resource.go` (`Create`, lines 183-211)

**Interfaces:**
- Consumes: `tfstate.NullUnknowns` from Task 1.
- Produces: nothing later tasks use.

This task covers two coupled changes. The image is recorded right after `images.Create`, **before** the data upload, because Glance has the image from that moment and an apply interrupted mid-upload orphans a `queued` image today. That puts the record ahead of the two existing paths that delete the image and fail, so those paths must clear the state — but only when the delete actually worked.

- [ ] **Step 1: Write the failing tests**

Create `internal/services/images/image_failed_create_internal_test.go`:

```go
// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package images

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

// imagePlan builds the plan a configuration with image_source_url produces on a
// first apply: everything the provider computes is still unknown.
func imagePlan(ctx context.Context, t *testing.T, s schema.Schema) tfsdk.Plan {
	t.Helper()
	planned := imageModel{
		ID:              types.StringUnknown(),
		Name:            types.StringValue("ubuntu-24.04"),
		ContainerFormat: types.StringValue("bare"),
		DiskFormat:      types.StringValue("qcow2"),
		LocalFilePath:   types.StringNull(),
		ImageSourceURL:  types.StringValue("https://example.invalid/ubuntu.qcow2"),
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
	plan := tfsdk.Plan{Schema: s, Raw: tftypes.NewValue(s.Type().TerraformType(ctx), nil)}
	if d := plan.Set(ctx, &planned); d.HasError() {
		t.Fatalf("building the plan: %v", d)
	}
	return plan
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
```

`imagePlan` takes a `schema.Schema`, so add `"github.com/hashicorp/terraform-plugin-framework/resource/schema"` to the import block. `package images` already imports it in `image_resource.go`; the resource's own `Schema` method is a method name, not a package name, so there is no collision.

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/services/images/ -run 'TestImageCreate' -v`
Expected: `TestImageCreateKeepsAnImageThatFailedToLoad` FAILs at `create returned no state`, `TestImageCreateKeepsTheStateOfAnImageItCouldNotDelete` FAILs at `create dropped the state of an image its cleanup delete could not remove`. `TestImageCreateDropsTheStateOfAnImageItDeleted` PASSes already, because `Create` does not record anything yet — it guards the behavior the next step must not break.

- [ ] **Step 3: Write the implementation**

In `internal/services/images/image_resource.go`, add the import in the group that holds `internal/clients`:

```go
	"github.com/platform9/terraform-provider-pcd/internal/tfstate"
```

Then replace the block that runs from the create call through the two load paths. Before:

```go
	img, err := images.Create(ctx, client, createOpts).Extract()
	if err != nil {
		resp.Diagnostics.AddError("images: creating image", err.Error())
		return
	}

	// Load image data, then wait for it to become active.
	if localPath != "" {
		if err := r.uploadLocalFile(ctx, client, img.ID, localPath, plan.VerifyChecksum.ValueBool()); err != nil {
			_ = images.Delete(ctx, client, img.ID).ExtractErr()
			resp.Diagnostics.AddError("images: uploading image data", err.Error())
			return
		}
	} else {
		if err := imageimport.Create(ctx, client, img.ID, imageimport.CreateOpts{
			Name: imageimport.WebDownloadMethod,
			URI:  sourceURL,
		}).ExtractErr(); err != nil {
			_ = images.Delete(ctx, client, img.ID).ExtractErr()
			resp.Diagnostics.AddError("images: starting web-download import", err.Error())
			return
		}
	}
```

After:

```go
	img, err := images.Create(ctx, client, createOpts).Extract()
	if err != nil {
		resp.Diagnostics.AddError("images: creating image", err.Error())
		return
	}

	plan.ID = types.StringValue(img.ID)
	// Glance holds the image from here on, before any data is loaded into it, so
	// record it now. A failed or interrupted upload, import, or wait then returns
	// its error with the image in state, Terraform marks it tainted, and the next
	// apply or a destroy deletes it instead of leaving it behind and creating
	// another. The attributes Glance has not reported yet are saved as null; the
	// next refresh reads them.
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
	resp.Diagnostics.Append(tfstate.NullUnknowns(&resp.State)...)
	if resp.Diagnostics.HasError() {
		return
	}

	// Load image data, then wait for it to become active.
	if localPath != "" {
		if err := r.uploadLocalFile(ctx, client, img.ID, localPath, plan.VerifyChecksum.ValueBool()); err != nil {
			deleteCreatedImage(ctx, client, img.ID, resp)
			resp.Diagnostics.AddError("images: uploading image data", err.Error())
			return
		}
	} else {
		if err := imageimport.Create(ctx, client, img.ID, imageimport.CreateOpts{
			Name: imageimport.WebDownloadMethod,
			URI:  sourceURL,
		}).ExtractErr(); err != nil {
			deleteCreatedImage(ctx, client, img.ID, resp)
			resp.Diagnostics.AddError("images: starting web-download import", err.Error())
			return
		}
	}
```

Add the helper next to `Create`, after the function:

```go
// deleteCreatedImage removes an image Create made but could not load data into,
// and takes it back out of state when Glance confirms it is gone. A delete that
// fails leaves the state in place: the image is still in Glance, so Terraform
// taints it and a destroy retries the deletion rather than orphaning it.
func deleteCreatedImage(ctx context.Context, client *gophercloud.ServiceClient, id string, resp *resource.CreateResponse) {
	err := images.Delete(ctx, client, id).ExtractErr()
	if err == nil || gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
		resp.State.RemoveResource(ctx)
	}
}
```

Leave the wait and everything after it alone.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/services/images/ -run 'TestImageCreate' -v`
Expected: PASS for all three.

- [ ] **Step 5: Run the full unit suite**

Run: `go test ./internal/... -timeout 120s`
Expected: PASS.

- [ ] **Step 6: Vet and lint**

Run: `go vet ./... && golangci-lint run ./...`
Expected: no findings.

- [ ] **Step 7: Commit**

```bash
git add internal/services/images/image_resource.go internal/services/images/image_failed_create_internal_test.go
git commit -m "fix(images): keep a pcd_images_image that fails to load in state

An image whose data Glance cannot load stays in Glance with status \"killed\", but
Create returned the wait error without state, so Terraform lost track of it: the
next apply created a second image and the first had to be deleted through the
API. An apply interrupted while the data was uploading left the same orphan,
with the image in \"queued\".

Create now records the image as soon as Glance accepts it, before any data is
loaded, so a failed load, a failed wait, a timeout, or an interrupted apply
leaves a tainted resource the next apply or a destroy removes.

The two paths that delete the image after a failed upload or import request now
take it back out of state only when the delete succeeded. A delete that fails
keeps the state, since the image is still in Glance and dropping the state would
orphan it; those paths previously discarded the delete's error.

Unit tests drive Create against a fake Glance for a killed image, a cleanup
delete that succeeds, and a cleanup delete that fails."
```

---

### Task 6: Changelog

**Files:**
- Modify: `CHANGELOG.md` (the `### Fixed` list under `## [Unreleased]`)

**Interfaces:**
- Consumes: the behavior from Tasks 2 through 5.
- Produces: nothing.

The `## [Unreleased]` heading and its `### Fixed` list already exist on this branch, added by the `pcd_compute_instance` commit this branch is based on. Append to that list; do not create a second heading.

- [ ] **Step 1: Add the entries**

In `CHANGELOG.md`, after the existing `pcd_compute_instance` bullet under `## [Unreleased]` / `### Fixed`, add:

```markdown
- `pcd_blockstorage_volume`, `pcd_blockstorage_snapshot`, `pcd_blockstorage_volume_backup`, and
  `pcd_images_image`: an object the service accepts and then fails to finish (Cinder status
  `error`, Glance status `killed`) now stays in state, and so does one whose create wait times out
  or whose apply is interrupted. The apply still fails with the service's reason, and Terraform
  marks the resource tainted, so the next apply deletes and recreates it and a destroy deletes it.
  Before, the failed apply left no state: Terraform lost track of an object the service kept, the
  next apply created a second one, and the first had to be deleted through the API. The attributes
  the service had not reported yet are saved empty until the next refresh.
- `pcd_images_image`: an apply interrupted while the image data is uploading no longer leaves a
  `queued` image in Glance with nothing in state. When an upload or an import request fails, the
  provider still deletes the image it created, but it now keeps the image in state if that
  deletion fails, so a destroy retries it instead of the image being left behind.
```

- [ ] **Step 2: Check the spelling and the heading**

Run: `grep -nE "behaviour|colour|organis|recognis|analyse|TBD|TODO" CHANGELOG.md`
Expected: no output.

Run: `grep -c '^## \[Unreleased\]' CHANGELOG.md`
Expected: `1`.

- [ ] **Step 3: Run the full unit suite one more time**

Run: `go test ./internal/... -timeout 120s && go vet ./... && golangci-lint run ./...`
Expected: PASS, no findings.

- [ ] **Step 4: Commit**

```bash
git add CHANGELOG.md
git commit -m "docs: changelog for keeping failed creates in state"
```

---

## After the plan

The branch is stacked on `pushkar/instance-failed-create`, which has no pull request yet and is ten commits behind `main`. Before opening a pull request for this work:

1. Push `pushkar/instance-failed-create` and open its pull request first.
2. Open this branch's pull request with `--base pushkar/instance-failed-create`, so the diff shows only these commits.

Do not rebase either branch onto `main` as part of this plan, and do not open either pull request without checking first.

### A note for `pushkar/image-import-failure`

That branch carries an approved, unimplemented design for failing fast when a Glance web-download
import fails. Its `waitForNewImage` deletes the image on `errImportFailed` and expects `Create` to
return without state — which Task 5 makes untrue, because the image is now recorded before the
import starts.

The two compose with one addition on that side: the `errImportFailed` deletion must call
`resp.State.RemoveResource(ctx)`, guarded the way `deleteCreatedImage` guards it, so a deletion
that fails keeps the state. Its timeout and `killed` paths deliberately do not delete, and they
want the state kept, which is what Task 5 gives them. This plan does not implement any of that.
