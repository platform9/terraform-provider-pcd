# `pcd_images_image` Import Failure Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A `pcd_images_image` create with `image_source_url` fails within seconds when Glance reports the web-download import failed, naming the store and where the reason is recorded, and deletes the image it created instead of leaving an orphan — rather than polling `queued` for thirty minutes and reporting only a timeout.

**Architecture:** Glance records a failed store import on the image itself, in the `os_glance_failed_import` property, and reverts the image to `queued`. gophercloud already surfaces that property through `images.Get` (`Image.Properties`), so the fix is entirely inside `waitForImageActive`: a sentinel error plus a property check, ordered after the existing `active`/`killed` switch. A thin `waitForNewImage` wrapper deletes the image when — and only when — that sentinel fires, matching the two cleanup paths already in `Create`.

**Tech Stack:** Go 1.25.8, gophercloud/v2 v2.13.0 (`openstack/image/v2/images`), terraform-plugin-framework, `net/http/httptest` for unit tests. No new module dependencies.

## Global Constraints

- Scope is `internal/services/images/image_resource.go`, a new `internal/services/images/image_resource_internal_test.go`, and `CHANGELOG.md`. Do not touch any other resource, data source, or package.
- The only new import in `image_resource.go` is `errors`. Do not add `strings`, and do not add the `openstack/image/v2/tasks` package.
- Unit tests only. Do not run `terraform apply` or `terraform destroy` against the CE lab; it holds a standing region. `make testacc` is out of scope.
- The whole unit suite must stay fast — every test in the new file uses either a first-poll return or a zero timeout, so none of them sleep through the 3-second poll interval.
- American English throughout code comments, the changelog, and commit messages.
- Branch is `pushkar/image-import-failure` (already renamed). No Claude or Claude Code attribution in commit messages or PR text — the repo's `CLAUDE.md` overrides any harness default that adds a footer or `Co-Authored-By` trailer.
- Follow the file's existing style: terse lowercase `fmt.Errorf` messages prefixed by the resource area, comments that explain *why* rather than restating the code.

## File Structure

| File | Responsibility |
| --- | --- |
| `internal/services/images/image_resource.go` | Modify. Adds `errImportFailed`, `imageProperty`, the failed-import check and the timeout detail inside `waitForImageActive`, the `waitForNewImage` wrapper, and the `Create` call-site change. |
| `internal/services/images/image_resource_internal_test.go` | Create. `package images` (internal, so it can reach the unexported wait functions). httptest fixtures plus the table tests. Follows `internal/services/resmgr/absence_internal_test.go`. |
| `CHANGELOG.md` | Modify. New `## [Unreleased]` / `### Fixed` section above `## [0.1.13]`. |

`internal/services/images/image_resource_test.go` stays untouched: it is `package images_test` and holds acceptance tests that need a lab.

## Background the implementer needs

Verified on 2026-09-19 by reading the CE lab's Glance 19.0.0.dev1395 source and gophercloud v2.13.0. Do not re-derive this; it is why the design looks the way it does.

- gophercloud's `Image.UnmarshalJSON` funnels every unrecognized top-level response key into `Properties map[string]any` via `gophercloud.RemainingKeys`. Glance's `_format_image` serializes all `extra_properties` except `location`/`location_data`. So `os_glance_failed_import` reaches `images.Get` callers as a `string` value in `Properties`, with no extra API call.
- On a store failure, Glance's import flow calls `add_failed_stores([backend])` — merging the store name into `os_glance_failed_import` — then reverts the image to `queued` and drops the import lock.
- `merge_store_list` comma-joins the surviving stores and **writes the key back as an empty string** once the list empties. A present-but-empty value means "no failure". This was confirmed against a live gophercloud decode: the key arrives present, typed `string`, valued `""`.
- `os_glance_import_task` holds the task ID but is *deleted on failure*, and `GET /v2/tasks/{id}` is gated by the `tasks_api_access` policy (`rule:context_is_admin` by default; the lab's `policy.yaml` is `{}`). That is why the task API is not used.

---

### Task 1: Detect a failed import and fail fast

**Files:**
- Modify: `internal/services/images/image_resource.go` (imports block at 11-38; `waitForImageActive` at 447-469)
- Create: `internal/services/images/image_resource_internal_test.go`

**Interfaces:**
- Consumes: nothing from earlier tasks.
- Produces, for Tasks 2 and 3:
  - `var errImportFailed error` — sentinel, matched with `errors.Is`.
  - `func imageProperty(img *images.Image, key string) string`
  - Test helpers `func newImageClient(t *testing.T, bodies ...string) (*gophercloud.ServiceClient, *imageServer)` and `func imageBody(status string, extra map[string]string) string`, plus the `imageServer` struct, whose `gets` and `deletes` are `atomic.Int64` and are read with `.Load()`.

- [ ] **Step 1: Write the failing tests**

Create `internal/services/images/image_resource_internal_test.go`:

```go
// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package images

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/image/v2/images"
)

// imageServer answers GET /images/<id> with each body in turn, repeating the
// last one once the list runs out, and counts the requests it saw so a test can
// assert both how often the wait polled and whether it cleaned up. The counters
// are atomic because the handler runs on the server's goroutine and the test
// reads them from its own.
type imageServer struct {
	bodies  []string
	gets    atomic.Int64
	deletes atomic.Int64
}

func newImageClient(t *testing.T, bodies ...string) (*gophercloud.ServiceClient, *imageServer) {
	t.Helper()
	s := &imageServer{bodies: bodies}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			s.deletes.Add(1)
			w.WriteHeader(http.StatusNoContent)
			return
		}
		i := int(s.gets.Add(1)) - 1
		if i >= len(s.bodies) {
			i = len(s.bodies) - 1
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(s.bodies[i]))
	}))
	t.Cleanup(srv.Close)
	return &gophercloud.ServiceClient{
		ProviderClient: &gophercloud.ProviderClient{},
		Endpoint:       srv.URL + "/",
	}, s
}

// imageBody renders a Glance image record. Keys in extra are plain response
// fields, which is how the os_glance_* bookkeeping reaches Image.Properties.
func imageBody(status string, extra map[string]string) string {
	fields := []string{
		`"id":"img-1"`,
		`"name":"tf-test"`,
		`"container_format":"bare"`,
		`"disk_format":"qcow2"`,
		`"tags":[]`,
		fmt.Sprintf(`"status":%q`, status),
	}
	for k, v := range extra {
		fields = append(fields, fmt.Sprintf("%q:%q", k, v))
	}
	return "{" + strings.Join(fields, ",") + "}"
}

// A web-download import that Glance could not complete leaves the image queued,
// not killed. Before this check the wait polled that queued image for the full
// 30 minutes and then reported only a timeout.
func TestWaitForImageActiveFailsFastOnAFailedImport(t *testing.T) {
	client, srv := newImageClient(t, imageBody("queued", map[string]string{
		"os_glance_failed_import": "file",
	}))

	start := time.Now()
	_, err := waitForImageActive(context.Background(), client, "img-1", 30*time.Minute)
	if err == nil {
		t.Fatal("got no error; the apply would keep polling a dead import")
	}
	if !errors.Is(err, errImportFailed) {
		t.Fatalf("errors.Is(%v, errImportFailed) = false; Create would not clean up the image", err)
	}
	if got := srv.gets.Load(); got != 1 {
		t.Fatalf("polled %d times, want 1: the failure is on the first read", got)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("took %s; failing fast is the point of the change", elapsed)
	}
	for _, want := range []string{"img-1", "file", `"queued"`, "glance-api"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not mention %q, which the user needs to act on it", err, want)
		}
	}
}

// Every failed store has to be named, not just the first.
func TestWaitForImageActiveNamesEveryFailedStore(t *testing.T) {
	client, _ := newImageClient(t, imageBody("queued", map[string]string{
		"os_glance_failed_import": "file,ceph",
	}))

	_, err := waitForImageActive(context.Background(), client, "img-1", 30*time.Minute)
	if err == nil {
		t.Fatal("got no error")
	}
	if !strings.Contains(err.Error(), "file,ceph") {
		t.Fatalf("error %q drops one of the failed stores", err)
	}
}

// Glance's merge_store_list writes the key back as "" once every store has been
// removed from the list, so a present-but-empty value is not a failure. Reading
// it as one would break every import on a cloud that had ever cleared the list.
func TestWaitForImageActiveTreatsAnEmptyFailedImportAsNoFailure(t *testing.T) {
	client, _ := newImageClient(t, imageBody("queued", map[string]string{
		"os_glance_failed_import": "",
	}))

	_, err := waitForImageActive(context.Background(), client, "img-1", 0)
	if err == nil {
		t.Fatal("got no error at a zero timeout")
	}
	if errors.Is(err, errImportFailed) {
		t.Fatalf("empty os_glance_failed_import reported as a failure: %v", err)
	}
}

// A value the provider cannot read as a store list must not be stringified into
// the message or mistaken for a failure.
func TestWaitForImageActiveIgnoresANonStringFailedImport(t *testing.T) {
	client, _ := newImageClient(t, `{"id":"img-1","name":"tf-test","container_format":"bare",`+
		`"disk_format":"qcow2","tags":[],"status":"queued","os_glance_failed_import":42}`)

	_, err := waitForImageActive(context.Background(), client, "img-1", 0)
	if err == nil {
		t.Fatal("got no error at a zero timeout")
	}
	if errors.Is(err, errImportFailed) {
		t.Fatalf("a non-string value read as a failed store list: %v", err)
	}
	if strings.Contains(err.Error(), "42") {
		t.Fatalf("error %q stringified a value that is not a store list", err)
	}
}

// A multi-store import can lose one store and still bring the image to active.
// An active image is usable, so the status check has to win over the property.
func TestWaitForImageActiveSucceedsWhenOnlyOneStoreFailed(t *testing.T) {
	client, _ := newImageClient(t, imageBody("active", map[string]string{
		"os_glance_failed_import": "ceph",
	}))

	img, err := waitForImageActive(context.Background(), client, "img-1", 30*time.Minute)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if img.Status != images.ImageStatusActive {
		t.Fatalf("status = %q, want active", img.Status)
	}
}

// killed keeps its own error and must stay distinguishable: Task 3 deletes the
// image for an import failure and deliberately leaves a killed one alone.
func TestWaitForImageActiveKilledIsNotAnImportFailure(t *testing.T) {
	client, _ := newImageClient(t, imageBody("killed", nil))

	_, err := waitForImageActive(context.Background(), client, "img-1", 30*time.Minute)
	if err == nil {
		t.Fatal("got no error for a killed image")
	}
	if errors.Is(err, errImportFailed) {
		t.Fatalf("killed reported as an import failure: %v", err)
	}
	if !strings.Contains(err.Error(), "killed") {
		t.Fatalf("error %q does not mention the killed state", err)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/services/images -run TestWaitForImageActive -v`

Expected: FAIL to compile, with `undefined: errImportFailed`.

- [ ] **Step 3: Add the sentinel and the property helper**

In `internal/services/images/image_resource.go`, add `"errors"` to the standard-library import block (after `"encoding/hex"`, before `"fmt"`).

Then add, immediately above `waitForImageActive`:

```go
// errImportFailed marks a wait that ended because Glance recorded a failed
// import on the image, rather than one that ran out of time. Create deletes the
// image for the first and leaves it alone for the second.
var errImportFailed = errors.New("glance recorded a failed import")

// imageProperty reads one of Glance's os_glance_* bookkeeping keys, which
// arrive as ordinary response fields and land in Properties. A value that is
// not a string reads as absent: these keys hold comma-joined store lists, and
// anything else is not one. Unlike flatten, this does not go through
// fmt.Sprintf, which would turn a missing key into a non-empty string.
func imageProperty(img *images.Image, key string) string {
	s, _ := img.Properties[key].(string)
	return s
}
```

- [ ] **Step 4: Add the failed-import check to the poll loop**

In `waitForImageActive`, insert this immediately after the existing `switch img.Status { ... }` block and before the `if time.Now().After(deadline)` check. The order matters: a multi-store import can lose one store and still reach `active`, and an active image has to win.

```go
		// A web-download import that fails leaves the image queued, not killed:
		// Glance records the stores it could not write and reverts the status,
		// so without this the wait polls a dead import until it times out.
		if stores := imageProperty(img, "os_glance_failed_import"); stores != "" {
			return nil, fmt.Errorf("%w: image %s could not be imported into store(s) %s, and Glance "+
				"left it in status %q. The reason is recorded in the Glance import task and the "+
				"glance-api log; reading the task needs the admin role (\"openstack task list\"). A "+
				"store that cannot allocate backing space, for example a disabled cinder-volume "+
				"service, is a common cause", errImportFailed, id, stores, img.Status)
		}
```

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test ./internal/services/images -run TestWaitForImageActive -v`

Expected: PASS, all six tests, in well under a second.

- [ ] **Step 6: Commit**

```bash
git add internal/services/images/image_resource.go internal/services/images/image_resource_internal_test.go
git commit -m "fix: fail a pcd_images_image create as soon as Glance reports a failed import

A web-download import that Glance cannot complete leaves the image in
queued, not killed, and records the stores it could not write in the
image's os_glance_failed_import property. The wait recognized only
active and killed, so it polled the dead import for its full 30-minute
timeout and then reported nothing but the last status.

The wait now reads that property and fails immediately, naming the
stores, the status Glance left the image in, and where the underlying
reason is recorded. A present-but-empty value is not a failure: Glance
writes the key back as an empty string once every store has been
removed from the list."
```

---

### Task 2: Say whether Glance was still importing when the wait timed out

**Files:**
- Modify: `internal/services/images/image_resource.go` (the deadline branch of `waitForImageActive`)
- Modify: `internal/services/images/image_resource_internal_test.go`

**Interfaces:**
- Consumes: `imageProperty`, `errImportFailed`, `newImageClient`, `imageBody` from Task 1.
- Produces: nothing new for Task 3.

- [ ] **Step 1: Write the failing test**

Append to `internal/services/images/image_resource_internal_test.go`:

```go
// A timeout that says nothing but the status cannot be acted on. Glance keeps
// the stores an import is working on in os_glance_importing_to_stores, which
// separates a slow import from a wedged one.
func TestWaitForImageActiveTimeoutReportsImportProgress(t *testing.T) {
	for _, tc := range []struct {
		name  string
		extra map[string]string
		want  string
	}{
		{
			name: "nothing in flight",
			want: `timed out waiting for image img-1 to become active (last status "queued")`,
		},
		{
			name:  "still importing",
			extra: map[string]string{"os_glance_importing_to_stores": "file"},
			want:  `(last status "queued", still importing into store(s) file)`,
		},
		{
			name:  "empty importing list reads as nothing in flight",
			extra: map[string]string{"os_glance_importing_to_stores": ""},
			want:  `timed out waiting for image img-1 to become active (last status "queued")`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client, srv := newImageClient(t, imageBody("queued", tc.extra))

			_, err := waitForImageActive(context.Background(), client, "img-1", 0)
			if err == nil {
				t.Fatal("got no error at a zero timeout")
			}
			if errors.Is(err, errImportFailed) {
				t.Fatalf("a timeout reported as an import failure: Create would delete an image that may still be importing: %v", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not contain %q", err, tc.want)
			}
			if got := srv.gets.Load(); got != 1 {
				t.Fatalf("polled %d times at a zero timeout, want 1", got)
			}
		})
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/services/images -run TestWaitForImageActiveTimeoutReportsImportProgress -v`

Expected: FAIL on the `still importing` subtest — the message has no `still importing into store(s)` clause. The other two subtests already pass against the current message.

- [ ] **Step 3: Add the progress detail to the timeout message**

Replace the deadline branch in `waitForImageActive`:

```go
		if time.Now().After(deadline) {
			// Naming the stores Glance is still working on separates a slow
			// import from one that is not making progress at all.
			progress := ""
			if stores := imageProperty(img, "os_glance_importing_to_stores"); stores != "" {
				progress = fmt.Sprintf(", still importing into store(s) %s", stores)
			}
			return nil, fmt.Errorf("timed out waiting for image %s to become active (last status %q%s)",
				id, img.Status, progress)
		}
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `go test ./internal/services/images -run TestWaitForImageActive -v`

Expected: PASS, all seven tests.

- [ ] **Step 5: Commit**

```bash
git add internal/services/images/image_resource.go internal/services/images/image_resource_internal_test.go
git commit -m "fix: name the stores Glance was still importing into on a timeout

A timeout that reports only the last status does not say whether the
import was making progress. Glance keeps the stores an import is working
on in os_glance_importing_to_stores, so the message now carries them
when the key is set."
```

---

### Task 3: Delete the image a failed import left behind

**Files:**
- Modify: `internal/services/images/image_resource.go` (`Create` at 207-211; new `waitForNewImage` beside `waitForImageActive`)
- Modify: `internal/services/images/image_resource_internal_test.go`

**Interfaces:**
- Consumes: `errImportFailed`, `newImageClient`, `imageBody`, and `imageServer.deletes` from Task 1.
- Produces: `func waitForNewImage(ctx context.Context, client *gophercloud.ServiceClient, id string, timeout time.Duration) (*images.Image, error)` — the only wait `Create` calls.

- [ ] **Step 1: Write the failing test**

Append to `internal/services/images/image_resource_internal_test.go`:

```go
// Create never records a failed image in state, so one left in Glance is an
// orphan that blocks the retry with a name or checksum clash. A timeout is a
// different matter: the import may still finish, and a killed image is the
// upload path's existing behavior, which this change does not touch.
func TestWaitForNewImageDeletesOnlyAFailedImport(t *testing.T) {
	for _, tc := range []struct {
		name       string
		body       string
		timeout    time.Duration
		wantDelete int64
	}{
		{
			name:       "failed import is deleted",
			body:       imageBody("queued", map[string]string{"os_glance_failed_import": "file"}),
			timeout:    30 * time.Minute,
			wantDelete: 1,
		},
		{
			name:    "killed is left alone",
			body:    imageBody("killed", nil),
			timeout: 30 * time.Minute,
		},
		{
			name:    "timeout is left alone",
			body:    imageBody("queued", nil),
			timeout: 0,
		},
		{
			name:    "active is left alone",
			body:    imageBody("active", nil),
			timeout: 30 * time.Minute,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client, srv := newImageClient(t, tc.body)

			_, _ = waitForNewImage(context.Background(), client, "img-1", tc.timeout)

			if got := srv.deletes.Load(); got != tc.wantDelete {
				t.Fatalf("deleted %d times, want %d", got, tc.wantDelete)
			}
		})
	}
}

// The successful path still hands the image back to Create.
func TestWaitForNewImageReturnsTheActiveImage(t *testing.T) {
	client, _ := newImageClient(t, imageBody("active", nil))

	img, err := waitForNewImage(context.Background(), client, "img-1", 30*time.Minute)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if img == nil || img.ID != "img-1" {
		t.Fatalf("img = %+v, want the image Create will flatten into state", img)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/services/images -run TestWaitForNewImage -v`

Expected: FAIL to compile, with `undefined: waitForNewImage`.

- [ ] **Step 3: Add the wrapper**

In `internal/services/images/image_resource.go`, add immediately after `waitForImageActive`:

```go
// waitForNewImage waits for a freshly created image to become active. An image
// whose import Glance reports as failed is deleted: it holds no data, Create is
// about to fail without recording it in state, and the orphan blocks the retry.
// This matches what Create already does when the upload or the import request
// itself fails. A timeout is left alone, because the import may still finish.
func waitForNewImage(ctx context.Context, client *gophercloud.ServiceClient, id string, timeout time.Duration) (*images.Image, error) {
	img, err := waitForImageActive(ctx, client, id, timeout)
	if errors.Is(err, errImportFailed) {
		_ = images.Delete(ctx, client, id).ExtractErr()
	}
	return img, err
}
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `go test ./internal/services/images -run TestWaitForNewImage -v`

Expected: PASS, both tests and all four subtests.

- [ ] **Step 5: Point `Create` at the wrapper**

In `Create`, replace these five lines (currently at 207-211):

```go
	img, err = waitForImageActive(ctx, client, img.ID, 30*time.Minute)
	if err != nil {
		resp.Diagnostics.AddError("images: waiting for active image", err.Error())
		return
	}
```

with:

```go
	// The result goes into its own variable: the wait returns a nil image on
	// failure, and img.ID is still needed to clean up after one.
	active, err := waitForNewImage(ctx, client, img.ID, 30*time.Minute)
	if err != nil {
		resp.Diagnostics.AddError("images: waiting for active image", err.Error())
		return
	}
	img = active
```

- [ ] **Step 6: Verify the package builds and every test still passes**

Run: `go build ./... && go test ./internal/services/images -v`

Expected: builds clean; the nine unit tests pass. The acceptance tests in `image_resource_test.go` skip without `TF_ACC`.

- [ ] **Step 7: Commit**

```bash
git add internal/services/images/image_resource.go internal/services/images/image_resource_internal_test.go
git commit -m "fix: delete the image a failed pcd_images_image import left behind

A create whose import failed never records the image in state, so the
image stayed in Glance as an orphan that a retry had to delete by hand.
Create now deletes it, matching what it already does when the upload or
the import request itself fails. A timeout still leaves the image in
place, because the import may yet finish.

Create takes the wait's result in its own variable: the wait returns a
nil image on failure, and the ID is still needed for the cleanup."
```

---

### Task 4: Changelog and full verification

**Files:**
- Modify: `CHANGELOG.md` (new section above `## [0.1.13] - 2026-09-19` at line 7)

**Interfaces:**
- Consumes: the behavior from Tasks 1-3.
- Produces: nothing.

- [ ] **Step 1: Add the `## [Unreleased]` section**

Insert directly above `## [0.1.13] - 2026-09-19` in `CHANGELOG.md`:

```markdown
## [Unreleased]

### Fixed

- `pcd_images_image`: a create with `image_source_url` now fails as soon as Glance reports the
  web-download import failed, instead of polling for the full 30 minutes and reporting only a
  timeout. A failed import leaves the image in `queued`, not `killed`, and records the stores it
  could not write in the image's `os_glance_failed_import` property; the provider reads that
  property and reports the store names, the status Glance left the image in, and where the
  underlying reason is recorded — the Glance import task and the `glance-api` log, both of which
  need the admin role to read. Observed on Community Edition 2026.4 with the Cinder service
  disabled, where the store could not create a volume and the apply spent half an hour printing
  `Still creating...`. The image a failed create made is now deleted, matching what the provider
  already does when the upload or the import request itself fails, so a retry no longer needs a
  manual `openstack image delete`. A create that times out still leaves the image in place,
  because the import may yet finish, and its message now names the stores Glance was still
  importing into.
```

- [ ] **Step 2: Run the formatter and the full static analysis**

Run: `make fmt && make vet && make lint`

Expected: `gofmt -s -w .` reports nothing, `go vet ./...` is silent, `golangci-lint run ./...` reports `0 issues`.

If `gofmt` rewrote `image_resource.go` or the test file, re-run `make test` before committing.

- [ ] **Step 3: Run the whole unit suite**

Run: `make test`

Expected: every package `ok` or `no test files`, well inside the 120s timeout. Confirm `internal/services/images` is `ok` and takes under a second — a multi-second figure means a test is sleeping through the 3-second poll interval, which none of them should.

- [ ] **Step 4: Confirm the diff is exactly the intended scope**

Run: `git status --short && git diff --stat main...HEAD`

Expected: exactly these five paths across the branch — `CHANGELOG.md`, `docs/superpowers/plans/2026-09-19-image-import-failure.md`, `docs/superpowers/specs/2026-09-19-image-import-failure-design.md`, `internal/services/images/image_resource.go`, `internal/services/images/image_resource_internal_test.go`. Nothing under any other `internal/services/` package, nothing under `docs/resources/`, `examples/` or `templates/`.

- [ ] **Step 5: Commit**

```bash
git add CHANGELOG.md
git commit -m "docs: changelog entry for the failed image import fix"
```

---

## Verification checklist

Run before declaring the work done. Evidence, not assertions — paste the actual output.

- [ ] `make test` passes, and `internal/services/images` completes in under a second.
- [ ] `make vet` is silent.
- [ ] `make lint` reports 0 issues.
- [ ] `make build` succeeds.
- [ ] `git diff --stat main...HEAD` touches only the five files listed in Task 4 Step 4.
- [ ] `grep -rn "image/v2/tasks" internal/` returns nothing — the tasks API was deliberately not used.
- [ ] `git log main..HEAD --format='%an <%ae>%n%b'` contains no `Co-Authored-By: Claude` trailer and no `Generated with Claude Code` footer.
- [ ] `git branch --show-current` is `pushkar/image-import-failure`.

No lab run. Reproducing this end to end needs a deliberately broken import, and the lab holds a standing region.
