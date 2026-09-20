# `pcd_images_image`: fail fast on a failed Glance import

**Date:** 2026-09-19
**Status:** approved design, awaiting implementation plan
**Scope:** `internal/services/images/image_resource.go` and its tests, plus `CHANGELOG.md`

## Problem

When `image_source_url` is set, `Create` starts a Glance web-download import and then calls
`waitForImageActive(ctx, client, id, 30*time.Minute)`. That wait returns early only on status
`active` or `killed`.

Observed on the CE lab on 2026-09-19: the import task failed within seconds. The Glance log
recorded `Task ...: Could not import image file ...` / `failed to import image ... to the
filesystem`, because the backing Cinder service was disabled and the store could not create a
volume. Glance reverted the image to `queued` — not `killed` — so the wait saw nothing it
recognized. Terraform polled every three seconds for the full thirty minutes, printing only
`Still creating...`, and then failed with:

```
timed out waiting for image <id> to become active (last status "queued")
```

Nothing in that message pointed at the import failure. The image was also left behind in Glance
and never recorded in state, so retrying needed a manual `openstack image delete` first.

## What Glance actually exposes

Confirmed by reading the lab's Glance 19.0.0.dev1395 source (`glance-api` pod, read-only) and
gophercloud v2.13.0.

- `Image.UnmarshalJSON` in `openstack/image/v2/images/results.go` funnels every unrecognized
  top-level response key into `Properties map[string]any` through `gophercloud.RemainingKeys`.
  Glance's `api/v2/images.py:_format_image` serializes all of `image.extra_properties` except
  the `hidden_image_properties` config list, which defaults to `location` and `location_data`.
  So the `os_glance_*` bookkeeping keys are already retrievable from `images.Get` — no extra
  call and no new API surface.
- On a store failure the import flow (`async_/flows/api_image_import.py`) calls
  `add_failed_stores([backend])`, which merges the store name into
  **`os_glance_failed_import`**, reverts the image to `queued`, and then drops the import lock.
- `merge_store_list` writes the surviving stores back as a comma-joined string, and leaves the
  key as an **empty string** once every store has been removed from the list. An empty value
  therefore means "no failure", not "failed".
- **`os_glance_importing_to_stores`** holds the stores an import is currently working on.
- **`os_glance_import_task`** holds the task ID, but it is set before the import request returns
  `202` and **deleted on failure** (`drop_lock_for_task`). By the time `os_glance_failed_import`
  appears, the task ID is gone from the image.
- `GET /v2/tasks/{id}` is gated by the `tasks_api_access` policy, whose default `check_str` is
  `rule:context_is_admin`; the lab's `/etc/glance/policy.yaml` is `{}`, so an ordinary project
  user is refused with 403.

Together the last two points rule the task API out as a dependable source: the provider would
have to capture the task ID mid-poll and would then be refused for most users. The image
property is the reliable signal.

## Goal

A failed import fails the apply in seconds with a message that names what failed and where the
reason is, and leaves no orphaned image behind.

## Non-goals

- Reading the import task through `GET /v2/tasks`. Admin-gated by default and the task ID is
  no longer on the image once the import has failed.
- Changing the schema, the thirty-minute timeout, or its configurability.
- Changing the data sources or any other resource, which stay untouched. The local-upload path
  shares `waitForNewImage` with the web-download path (see section 4), but cannot trigger the new
  branch: `local_file_path` uses `imagedata.Upload` (`PUT /v2/images/{id}/file`), which never runs
  the staging/import flow, so Glance does not set `os_glance_failed_import` on it.

## Design

### 1. A sentinel error distinguishes a failure from a timeout

```go
// errImportFailed marks a wait that ended because Glance recorded a failed
// import on the image, rather than one that ran out of time.
var errImportFailed = errors.New("glance recorded a failed import")
```

`errors` is the only new import.

### 2. `waitForImageActive` checks `os_glance_failed_import`

The existing `active` / `killed` switch stays **first**, and the property check follows it.
The order is load-bearing: a multi-store import can lose one store and still bring the image
to `active`, and an active image has to win.

```go
if stores := imageProperty(img, "os_glance_failed_import"); stores != "" {
    return nil, fmt.Errorf("%w: image %s could not be imported into store(s) %s, and Glance "+
        "left it in status %q. The reason is recorded in the Glance import task and the "+
        "glance-api log; reading the task needs the admin role (\"openstack task list\"). A "+
        "store that cannot allocate backing space, for example a disabled cinder-volume "+
        "service, is a common cause", errImportFailed, id, stores, img.Status)
}
```

`%w` renders as `glance recorded a failed import`, so the diagnostic reads
`images: waiting for active image` / `glance recorded a failed import: image <id> could not be
imported into store(s) file, and Glance left it in status "queued". ...`.

The `!= ""` test is required behavior, not defensive coding, for the reason given above.

`imageProperty` is a small helper that looks the key up in `img.Properties` and type-asserts a
`string`. A non-string value reads as absent rather than panicking or stringifying into the
message. It does not reuse `flatten`'s `fmt.Sprintf("%v", v)`, which would turn a missing or
oddly typed value into a non-empty string and trip the check.

### 3. The timeout message reports whether Glance was still working

When the deadline passes, `os_glance_importing_to_stores` is appended when it is non-empty:

```
timed out waiting for image <id> to become active (last status "queued", still importing into store(s) file)
```

Without the property the message is unchanged. This separates a slow import from a stuck one.

### 4. `Create` deletes the image when the import failed

A wrapper gives both source paths in `Create` one call site and keeps the cleanup testable:

```go
// waitForNewImage waits for a freshly created image to become active. An image
// whose import Glance reports as failed is deleted: it holds no data, Create is
// about to fail without recording it in state, and the orphan blocks the retry.
func waitForNewImage(ctx context.Context, client *gophercloud.ServiceClient, id string, timeout time.Duration) (*images.Image, error) {
    img, err := waitForImageActive(ctx, client, id, timeout)
    if errors.Is(err, errImportFailed) {
        if derr := images.Delete(ctx, client, id).ExtractErr(); derr != nil {
            return img, fmt.Errorf("%w. The image could not be removed either (%v); delete it before retrying", err, derr)
        }
        return img, fmt.Errorf("%w. The image has been deleted, so the apply can be retried once the store is fixed", err)
    }
    return img, err
}
```

This matches the two failure paths already above it in `Create`, which delete the image and
return when `imagedata.Upload` or `imageimport.Create` errors.

A timeout deliberately does **not** delete: the import may still be running, and thirty minutes
of transfer should not be thrown away on the provider's initiative. `killed` deliberately does
not delete either — it is the upload path's existing behavior and out of scope here.

The delete's own error is not discarded: it is folded into the returned message alongside the
import failure, so the message always states what the provider did about the image — deleted it,
or tried and was refused (a `protected` image, a transient 5xx, a policy rule) and names it for
manual removal instead.

### 5. `Create` keeps `img` pointing at the created image

Line 207 currently reads `img, err = waitForImageActive(ctx, client, img.ID, 30*time.Minute)`,
which overwrites `img` with `nil` when the wait fails. Today the error branch only formats a
diagnostic, so that is harmless, but assigning straight into `img` is not something to rely on:
`waitForNewImage` returns a nil image on every failure path, and `img` should not be clobbered
with it. The result therefore goes into a new variable and is assigned to `img` only on success —
defensive discipline, not a correctness requirement of the code as it stands today.

## Testing

A new `internal/services/images/image_resource_internal_test.go` in `package images`, using
`httptest` and a bare `gophercloud.ServiceClient`, following
`internal/services/resmgr/absence_internal_test.go`.

Over `waitForImageActive`:

- `os_glance_failed_import: "file"` on a `queued` image returns on the first poll, not after
  thirty minutes; `errors.Is(err, errImportFailed)` holds, and the message carries the store
  name and the status.
- `os_glance_failed_import: ""` is not a failure: a zero timeout over a `queued` image with the
  key present and empty returns the timeout error, not `errImportFailed`. Asserted this way
  rather than by letting the loop poll through to `active`, which would make the test sleep
  through the three-second interval for no extra coverage.
- Comma-joined stores appear in the message.
- `active` with `os_glance_failed_import` set still succeeds — the partial multi-store case.
- `killed` keeps its existing error and is **not** `errImportFailed`.
- A zero timeout returns the timeout message, with and without the
  `os_glance_importing_to_stores` suffix, and without sleeping.
- A non-string property value is treated as absent.

Over `waitForNewImage`: the test server records a `DELETE` on import failure, and none on
`killed` or on timeout. A refused delete (403) still returns an error for which
`errors.Is(err, errImportFailed)` holds, and whose message says the image could not be removed;
a successful delete returns an error for which `errors.Is(err, errImportFailed)` holds, and whose
message says the image has been deleted.

Unit tests only. Reproducing this end to end on the lab needs a deliberately broken import, and
the lab holds a standing region.

## Changelog

An `## [Unreleased]` / `### Fixed` entry in the existing style, American English, no ticket
reference.
