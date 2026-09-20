# Create must record the object before it waits

**Date:** 2026-09-19
**Status:** approved design, awaiting implementation plan
**Branch:** `pushkar/create-state-before-wait`, stacked on `pushkar/instance-failed-create`
**Scope:** `internal/tfstate` (new), `internal/services/compute/instance_resource.go`,
`internal/services/blockstorage/{volume,snapshot,backup}_resource.go`,
`internal/services/images/image_resource.go`, `internal/services/dns/zone_resource.go`
(`pcd_dns_zone`; its delete waiter, `waitForZoneDeleted` in `internal/services/dns/dns.go`,
was relaxed alongside the create-state fix so a zone abandoned in `ERROR` by a failed create
can still be deleted), `internal/services/loadbalancer/loadbalancer_resource.go`
(`pcd_lb_loadbalancer`; its delete waiter, `waitForLoadBalancerDeleted` in
`internal/services/loadbalancer/loadbalancer.go`, was relaxed the same way so a load balancer
abandoned in `ERROR` by a failed create can still be deleted), their tests, and `CHANGELOG.md`

## Problem

Four resources call their service's create, then wait for a target status, and return the wait's
error without saving state. They are the four this change fixes, not the only resources with this
problem — four more have it too (see **Other resources with the same gap** near the end):

| Resource | Wait call in `Create` | Wait |
| --- | --- | --- |
| `pcd_blockstorage_volume` | `volume_resource.go:133` | `waitForVolumeStatus(..., "available", 20m)` |
| `pcd_blockstorage_snapshot` | `snapshot_resource.go:110` | `waitForSnapshotStatus(..., "available", 20m)` |
| `pcd_blockstorage_volume_backup` | `backup_resource.go:113` | `waitForBackupStatus(..., "available", 30m)` |
| `pcd_images_image` | `image_resource.go:207` | `waitForImageActive(..., 30m)` |

The API keeps the object in all three ways that wait can end badly: the object reaches an error
status, the wait times out, or the apply is interrupted. Terraform records nothing, so it loses
track of an object that exists. The next apply creates a second one, and the first has to be
deleted through the API.

`pcd_compute_instance` was fixed this way in `13be3e3`, and `pcd_host_cluster_role`'s `Create`
already keeps state on a failed convergence wait. This change applies the same pattern to the
remaining four.

## Goal

Each of the four `Create`s records the object in state as soon as the API accepts it. A failed or
interrupted wait then returns its error with the resource in state, Terraform marks it tainted,
and the next apply or a destroy removes it.

## Non-goals

- Changing any create wait's target status, timeout, or error message.
- Changing which statuses a *create* wait treats as failures. A *delete* waiter may still relax
  which status it treats as a failure, since the same status (`ERROR` on both Designate and
  Octavia) is overloaded across the create and delete phases; see **Design** for the rule this
  applies.
- Closing the timeout-in-`creating` delete gap (see **Known gap**).
- Any change to `pcd_compute_instance` beyond moving its helper to the shared package.

## Design

### 1. `internal/tfstate.NullUnknowns`

Terraform refuses unknown values in state, and a `Create` that records its object before the API
has reported the object's computed attributes leaves them unknown. `13be3e3` solved this with an
unexported `nullUnknowns` in `package compute`. It moves to a new package rather than being copied
four more times:

```go
// Package tfstate holds helpers for resources that write Terraform state
// outside the usual happy path.
package tfstate

// NullUnknowns replaces every unknown value in state with null. Terraform
// refuses unknown values in state, and a Create that records its object before
// the API has reported the object's computed attributes leaves them unknown.
func NullUnknowns(state *tfsdk.State) diag.Diagnostics
```

The body is the existing `tftypes.Transform` walk unchanged. Its error diagnostic loses the
`compute:` prefix and reads `"Preparing Terraform state"`, since the helper no longer belongs to
one service.

`instance_resource.go` drops its private copy and calls `tfstate.NullUnknowns`. Services import
only `internal/clients` and `internal/acctest` today; this is a third shared package, which five
call sites justify.

### 2. The three Cinder resources

The same block goes into each `Create`, immediately after the create call returns and before the
wait. For `volume_resource.go`:

```go
plan.ID = types.StringValue(vol.ID)
// Cinder keeps a volume whose build fails (status "error"), so record it before
// waiting. A failed or interrupted wait then returns its error with the volume in
// state, Terraform marks it tainted, and the next apply or a destroy deletes it
// instead of leaving it behind and creating another. The attributes Cinder has not
// reported yet are saved as null; the next refresh reads them.
resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
resp.Diagnostics.Append(tfstate.NullUnknowns(&resp.State)...)
if resp.Diagnostics.HasError() {
	return
}
```

`snapshot_resource.go` and `backup_resource.go` take the same block with their own object and
noun. Nothing else in those functions changes: each success path already overwrites state from
`flatten`/`setState`, and each of those covers every attribute that can be unknown in a create
plan. `force`, `incremental`, and `protected` are Optional+Computed with static defaults, so they
are already known in the plan and `NullUnknowns` does not touch them.

### 3. `pcd_images_image`

State is recorded right after `images.Create`, **before** the data upload or the import request,
not just before the wait. Glance has the image from that moment, and an apply interrupted
mid-upload orphans a `queued` image today.

That moves the record earlier than the two existing cleanup paths — `uploadLocalFile` failing and
`imageimport.Create` failing — which delete the image and return. They currently discard the
delete's error with `_ =`. They become:

```go
if delErr := images.Delete(ctx, client, img.ID).ExtractErr(); delErr == nil ||
	gophercloud.ResponseCodeIs(delErr, http.StatusNotFound) {
	resp.State.RemoveResource(ctx)
}
```

A delete that succeeds leaves no state and no orphan, exactly as today. A delete that fails keeps
the state, so Terraform taints the image and a destroy retries the deletion — which is the same
goal as the rest of this change, and is why the error is no longer discarded.

`Delete` needs no change: Glance accepts `DELETE` on a `killed` image, there is no delete waiter,
and `protected` is known in the plan so the existing clear-then-delete still runs.

### 4. Delete works on the status each wait gives up on

Checked against the waiters in the tree; no waiter changes are needed.

| Resource | Status the create wait abandons | Delete accepted | Delete waiter |
| --- | --- | --- | --- |
| volume | `error` | Cinder accepts `DELETE` on `error` | `waitForVolumeDeleted` fails only on `error_deleting` |
| snapshot | `error` | Cinder accepts `DELETE` on `error` | `waitForSnapshotDeleted` fails only on `error_deleting` |
| backup | `error` | Cinder accepts `DELETE` on `error` | `waitForBackupDeleted` fails only on `error_deleting` |
| image | `killed` | Glance accepts `DELETE` on `killed` | none |

The Cinder create waits also abandon on `error_deleting`, which the delete waiters do treat as a
failure. That combination cannot arise from a create: `error_deleting` is reached only by a
delete, so it is not a status a create wait leaves an object in.

### 5. Interaction with `pushkar/image-import-failure`

That branch carries an approved design (docs only, unimplemented) for failing fast when a Glance
web-download import fails. Its `waitForNewImage` deletes the image on `errImportFailed`, and
`Create` then returns without state — which this change makes untrue, because state is now
recorded before the import starts.

The two compose with one addition on that side: the `errImportFailed` deletion must be paired with
`resp.State.RemoveResource(ctx)` in `Create`, guarded the same way as section 3, so a deletion that
fails keeps the state. Its timeout and `killed` paths deliberately do not delete, and they want the
state kept, which is what this change gives them.

### 6. A delete waiter may relax a failure status only behind a seen-transition latch

Recording state before the wait means a delete waiter now regularly meets an object a failed
create wait abandoned in the same status (`ERROR`, for both Designate and Octavia) that a
genuinely failing delete also produces. A delete waiter that fails the instant it sees that status
turns every one of those abandoned objects into a destroy that fails on its first poll, forever,
with `terraform state rm` as the only way out — worse than the silent orphan this change fixes.

The rule: **a delete waiter may treat a status as a failure only once the object has been observed
leaving it.** A boolean latched by the first non-failure status distinguishes "already in that
status when the delete began" (not a failure — keep polling) from "entered that status while being
deleted" (a real failure — fail fast, as before). `gophercloud.WaitFor` calls its predicate
serially, so the latch needs no synchronization. This does not reopen the Non-goals bullet above:
that bullet is about a *create* wait's failure statuses, which are unchanged everywhere in this
change; only two delete waiters (`waitForZoneDeleted`, `waitForLoadBalancerDeleted`) apply the
latch, and both keep failing fast for the case where the object was healthy when the delete began.

## Testing

Unit tests only, following `internal/services/compute/instance_failed_create_internal_test.go`:
build a `tfsdk.Plan` from the resource's own schema, and point a `clients.Config` at an `httptest`
fake through `ProviderClient.EndpointLocator`.

- `internal/services/blockstorage/failed_create_internal_test.go` — one test per resource. Three
  tests share a file because they share the fake-server shape; their plans and models do not.
- `internal/services/images/image_failed_create_internal_test.go` — one test, driving the
  `image_source_url` path so no file on disk is involved.

Each test runs the full sequence a failed apply and the destroy after it would:

1. `Create` against a fake whose object reaches the abandoned status (`error`, or `killed` for
   Glance). Assert it errors, that `resp.State.Raw` is neither null nor holding unknown values, and
   that the ID and the locally resolved attributes are in state.
2. `Read`, as the destroy's refresh does. Assert it succeeds and reports the failure status.
3. `Delete`. Assert the fake recorded the `DELETE`, that there are no diagnostics, and — for the
   three Cinder resources — that the delete waiter polled past the failure status until the fake
   answered 404.

## Known gap

A create wait that **times out** leaves the object in `creating`, not `error`. Cinder may refuse
`DELETE` on a volume in `creating` unless it has no host or the request forces it, in which case
destroying the tainted resource would fail with a 400. This is unverified against the lab and is a
different failure mode from the one this change addresses; closing it would mean adding force or
retry logic to three `Delete`s. It is deliberately out of scope. Keeping state is still an
improvement in that case: Terraform at least knows the object exists.

An Octavia load balancer has the same gap in a different status. A create wait that times out, or
an apply interrupted while the load balancer builds, leaves it in `PENDING_CREATE`, which Octavia
treats as immutable: `loadbalancers.Delete` (`internal/services/loadbalancer/loadbalancer_resource.go:258`)
returns 409 and the destroy fails until Octavia settles the load balancer on its own. Same shape as
the Cinder `creating` gap above, and deliberately not fixed here. Keeping state is still an
improvement: Terraform at least knows the load balancer exists.

## Other resources with the same gap

Four more resources call their service's create and then wait for a target status without saving
state first — the same gap the **Problem** section above describes for the four resources this
change fixes. `pcd_dns_recordset` and `pcd_keymanager_secret`, both listed here originally, were
fixed by `pushkar/create-state-remaining-eight`; the four load balancer children below are the
only ones left, and that same branch found they cannot simply be ported — see its design doc.
None of the four are touched by this change; fixing them is separate follow-up work.

- `pcd_lb_listener` — `internal/services/loadbalancer/listener_resource.go:161` (the wait
  after `listeners.Create`; `Create` also waits for the parent load balancer at line 125, before the
  listener exists, which is not this gap)
- `pcd_lb_pool` — `internal/services/loadbalancer/pool_resource.go:171` (likewise; line 143
  is the pre-create wait for the parent load balancer)
- `pcd_lb_member` — `internal/services/loadbalancer/member_resource.go:146` (likewise; line
  117 is the pre-create wait)
- `pcd_lb_monitor` — `internal/services/loadbalancer/monitor_resource.go:153` (likewise;
  line 118 is the pre-create wait)

## Changelog

One `### Fixed` bullet per resource under the `## [Unreleased]` heading that
`pushkar/instance-failed-create` adds, in the existing style and American English, with no ticket
reference. Each names the resource, what the API keeps, that the apply still fails, and that the
resource is tainted so the next apply or a destroy removes it. The `pcd_images_image` bullet also
covers the interrupted upload.
