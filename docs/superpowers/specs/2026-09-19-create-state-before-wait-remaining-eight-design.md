# Create must record the object before it waits: the remaining eight

**Date:** 2026-09-19
**Status:** approved design, awaiting implementation plan
**Scope:** `internal/tfstate`, `internal/services/blockstorage` (helper move only),
`internal/services/dns`, `internal/services/loadbalancer`, `internal/services/keymanager`,
their tests, `CHANGELOG.md`, and the 2026-09-19 design document
**Follows:** `docs/superpowers/specs/2026-09-19-create-state-before-wait-design.md`

## Problem

Eight resources still call their service's create, then wait for a target status, then return the
wait's error without saving state. The service keeps the object, so Terraform loses track of
something that exists: the next apply creates a duplicate and the first has to be deleted through
the API by hand.

| Resource | Create | Post-create wait | Abandoned in |
| --- | --- | --- | --- |
| `pcd_dns_zone` | `zone_resource.go:123` | `:128` | `ERROR` |
| `pcd_dns_recordset` | `recordset_resource.go:111` | `:116` | `ERROR` |
| `pcd_lb_loadbalancer` | `loadbalancer_resource.go:137` | `:143` | `ERROR` |
| `pcd_lb_listener` | `listener_resource.go:156` | `:161` | root `ERROR` |
| `pcd_lb_pool` | `pool_resource.go:166` | `:171` | root `ERROR` |
| `pcd_lb_member` | `member_resource.go:141` | `:146` | root `ERROR` |
| `pcd_lb_monitor` | `monitor_resource.go:148` | `:153` | root `ERROR` |
| `pcd_keymanager_secret` | `secret_resource.go:146` | `:157` (conditional) | `ERROR` |

The five already fixed — `pcd_compute_instance`, the three Cinder resources and `pcd_images_image` —
are the pattern. These eight are not a straight port of it, for three reasons.

## The three findings that change the shape of the fix

### 1. The load balancer children cannot be destroyed while the root is in `ERROR`

The four children do not wait on themselves. Each `Delete` resolves the root load balancer and then
calls `waitForLoadBalancerActive` **before** issuing its own delete, returning on error:
`listener_resource.go:304`, `pool_resource.go:295`, `member_resource.go:274`,
`monitor_resource.go:280`. `waitForLoadBalancerActive` fails on `ERROR` and `DELETED`
(`loadbalancer.go:117-124`), and `gophercloud.WaitFor` runs its predicate once immediately
(`util.go:87-90`), so with the root in `ERROR` the child's `DELETE` is never issued.

What that costs depends on the configuration:

- Where another managed child sits between the failing resource and the root — the common case —
  **the destroy already fails today**, at the first child Terraform tries to remove. Recording state
  does not cause that; it exposes a pre-existing blocker.
- Where the failing child is the only managed load balancer resource in the tree (its parent comes
  from a data source or a literal ID), today's destroy **succeeds**: no child is in state, so
  Terraform goes straight to the root, whose `Delete` has no pre-delete wait and issues
  `loadbalancers.Delete(..., DeleteOpts{Cascade: true})` at `loadbalancer_resource.go:258`. Record the
  child and change nothing else and that destroy **fails**.

Either way the conclusion is the same and it is the governing constraint of this design: **the child
delete path must be relaxed, and verified, before any child create records state.** A tainted
resource that cannot be destroyed is worse than the orphan it replaced.

### 2. Two delete waiters bail on the status the create wait abandons

`waitForZoneDeleted` (`dns.go:129-131`) and `waitForLoadBalancerDeleted` (`loadbalancer.go:145-147`)
return an error when they see `ERROR`. That is exactly the status a failed create wait leaves the
object in, so recording state without touching them converts a silent orphan into a resource whose
destroy fails on its first poll, recoverable only with `terraform state rm`.

`waitForRecordSetDeleted` (`dns.go:165-182`) inspects no status and needs no change. The asymmetry is
correct — it matches `waitForVolumeDeleted` in the shipped work — and must not be "made consistent".

### 3. The read-back tail clobbers the record — in all eight

Every one of the eight ends with the same three lines (`zone_resource.go:133`,
`recordset_resource.go:121`, `loadbalancer_resource.go:148`, `listener_resource.go:166`,
`pool_resource.go:176`, `member_resource.go:151`, `monitor_resource.go:158`, `secret_resource.go:163`):

```go
	_, readDiags := r.readInto(ctx, client, id, &plan)
	resp.Diagnostics.Append(readDiags...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
```

`notFound` is discarded and the final `Set` is unconditional. `readInto` returns **before assigning
anything** when it 404s or errors (e.g. `zone_resource.go:239-245`), so `plan` still carries the create
plan's unknown values, and that `Set` overwrites the clean row this fix just recorded with a state full
of unknowns — which Terraform refuses.

This is latent today only because there is no record to clobber. It goes live the moment one lands.
The five shipped resources are not a template for it: `volume_resource.go:139-150` does a `Get` with an
explicit error return and then `flatten`, so it never has this shape. Fixing the tail is a strict
improvement on its own.

## Design

### Promote the helper: `tfstate.RecordCreated`

`blockstorage.recordCreated` moves to `internal/tfstate` as an exported `RecordCreated` with a
service-neutral doc comment. Call sites go from 5 to 13; four per-package copies would mean four
divergent copies of the paragraph that carries the rationale and the ID contract.

It gains an **ID guard**. Eight new call sites must each set `plan.ID` themselves, and for all five
load balancer resources the ID is assigned only inside `readInto` — which the failure path never
reaches. A recorded row with a null `id` is worse than no row: `secret_resource.go:215` would issue
`DELETE /v1/secrets/` with an empty last segment, and only `terraform state rm` clears it. The helper
therefore reads `id` back out of the state it just wrote, and on null-or-empty removes the resource and
reports a provider bug — returning to today's behavior rather than writing an undeletable row.

`NullUnknowns` keeps operating on `resp.State`, never on the plan struct. Every one of these
`readInto`s decides whether to populate an attribute with `IsNull() || IsUnknown()` tests, so nulling
the in-memory plan would change which of those fire on the success path.

### Transition-aware delete waiters

For the two waiters that bail, `ERROR` counts as a delete failure only once the object has been seen
leaving it — decided locally in the predicate, with no extra API call:

```go
	seenNonError := false
	// ... in the predicate:
	if status != errorStatus {
		seenNonError = true
		return false, nil
	}
	if seenNonError {
		return false, fmt.Errorf("... entered ERROR status during delete")
	}
	return false, nil
```

`gophercloud.WaitFor` calls the predicate serially, so the captured flag is safe. A healthy object
whose delete genuinely fails still fails fast with its existing message; an object abandoned in
`ERROR` deletes; an object that never leaves `ERROR` times out rather than being permanently wedged.
This dominates simply deleting the check, which would turn the genuine-failure case into a timeout too.

While editing `waitForLoadBalancerDeleted`, also accept `provisioning_status == DELETED` as success —
today only a 404 is, so a load balancer reporting `DELETED` spins the full timeout. Independent
one-line fix; call it out in review.

### The load balancer children's delete path

A new `waitForLoadBalancerSettled` returns when the root leaves its transient `PENDING_*` statuses and
reports what it settled on, treating `ACTIVE`, `ERROR`, `DELETED` and a 404 as settled. Octavia ties
its 409 immutability to `PENDING_*`, so `ERROR` is settled and still accepts a child delete. Enumerate
the settled set exactly as the existing switch does — do not use a `PENDING_` prefix test, which relies
on a naming convention Octavia never promised.

A `settleBeforeDelete` wrapper keeps each call site to two lines, warns rather than fails when the root
is in `ERROR`, and deletes anyway. **Both** of each child's waits move to it, not just the pre-delete
one: the post-delete wait exists so the next resource in the destroy does not hit a 409, and today it
turns a `DELETE` that succeeded into a reported failure. Its diagnostic must name which of the two
sites failed rather than hard-coding "before".

Nothing on the create path changes. `waitForLoadBalancerActive` keeps failing on `ERROR` there and in
every `Update`: building on a broken load balancer should still fail.

**A second, independent deadlock ships in the same commit.** `rootLBIDFromPool` and
`rootLBIDFromListener` (`loadbalancer.go:157-180`) return the raw gophercloud error with no 404
handling, and `member_resource.go:269`, `monitor_resource.go:275` and `pool_resource.go:290` then
`AddError` and return. A child whose parent has already been cascade-deleted can therefore never leave
state — `Delete` dies resolving the parent before it can discover the child is gone. Pre-existing, but
recording state is what makes it reachable. Each of those three sites returns cleanly on a 404.
`listener_resource.go` needs no equivalent: it reads `state.LoadbalancerID` with no API call.

### `pcd_keymanager_secret`

The record goes immediately after `id := refToID(secret.SecretRef)` and **before** the
`if payloadSet {` block, not inside it. Barbican holds the secret from the moment `secrets.Create`
returns whether or not a payload was sent, so the no-payload path can lose it too — via a transient
`readInto` failure or an interrupted apply — and one record site is better than two.

### The guarded tail, in all eight

```go
	notFound, readDiags := r.readInto(ctx, client, id, &plan)
	resp.Diagnostics.Append(readDiags...)
	if notFound {
		resp.Diagnostics.AddError(/* "<svc>: reading <obj> after create", "<obj> %s no longer exists." */)
	}
	if resp.Diagnostics.HasError() {
		return // leave the row RecordCreated wrote in place
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
```

## Sequencing

Every delete-path relaxation ships and is verified **before** the create record that depends on it, so
that if a relaxation proves wrong on the lab nothing has regressed.

| Task | Content | Depends on |
| --- | --- | --- |
| 0 | `tfstate.RecordCreated` + ID guard; move the 3 blockstorage call sites; unit-test the guard | — |
| 1 | `pcd_dns_recordset` (clean port) then `pcd_dns_zone` (+ transition-aware `waitForZoneDeleted`) | 0 |
| 2 | `pcd_keymanager_secret` | 0 |
| 3 | **LB child delete path only** — `waitForLoadBalancerSettled`, `settleBeforeDelete`, the eight wait swaps, the three 404 short-circuits. No `Create` changes. **Lab gate.** | 0 |
| 4 | `pcd_lb_loadbalancer` (root) + transition-aware `waitForLoadBalancerDeleted` + `DELETED` success | 0 |
| 5 | The four LB children's create record | 3 verified, 4 |
| 6 | Changelog and corrections to the 2026-09-19 design doc | all |

Order: 0, then {1, 2, 4} in parallel, then 3, then 5, then 6.

Do the DNS **recordset before the zone**: it proves the Designate fake against the resource that needs
no waiter change, and the zone reuses it. Do not "apply the same change to both DNS resources" — the
zone needs the waiter change and the recordset must not get one.

Tasks 4 and 5 both want a `fakeConfig` in `internal/services/loadbalancer/failed_create_internal_test.go`.
Task 4 lands first and owns the file; Task 5 appends and must not redeclare it. The same applies to the
two DNS halves of Task 1.

## The lab gate

**Task 3 must be verified on the CE lab before Task 5 ships.** Build a load balancer tree, drive the
root into `ERROR`, and confirm a `terraform destroy` of a listener, pool, member or monitor still issues
its `DELETE` and Octavia accepts it.

Nothing in this tree proves that Octavia accepts a child `DELETE` while the root is in `ERROR`. The
package doc (`loadbalancer.go:7-11`) ties the 409 to `PENDING_*` only, and upstream Octavia's
immutability check lists only the `PENDING_*` statuses, but that is inference. If Octavia refuses with
409, **stop**: Task 5 is dropped and the four children move to the known gaps below. The relaxation
itself cannot be worse than what it replaces — a 409 from the delete is the same user-visible outcome
as today's pre-wait failure, with a better message.

Two smaller server-side claims are also unverified and safe either way: that Designate accepts a
`DELETE` on a zone in `ERROR`, and that Barbican accepts one on a secret in `ERROR`.

## Known gaps, deliberately not fixed

- **`PENDING` / `PENDING_CREATE` abandonment.** A load balancer abandoned by a timeout or a Ctrl-C sits
  in `PENDING_CREATE`, which Octavia treats as immutable, so the cascade delete returns 409 until it
  settles. Same shape as the Cinder `creating` gap already recorded. A DNS zone abandoned that way sits
  in `PENDING`, which was never caught by the `ERROR` check, so it needs nothing.
- **Waiters aborting on any transient `Get` error**, including a 5xx (`loadbalancer.go:113-116`,
  `dns.go:99-101`, `keymanager.go:67-69`). Recording state makes a blip taint a resource where today it
  orphaned one — that is the improvement. Widening the retry policy is a separate change.
- **The root's `Delete` keeps its lack of a pre-delete settle.** Going straight to the cascade is what
  makes a whole-tree destroy work.
- **`pcd_keymanager_container`** has no post-create wait, so it is correctly not one of the eight.
- **`Read`'s ordering quirk** at `loadbalancer_resource.go:166-172`. Harmless; do not fix it here.

## Corrections to the 2026-09-19 design document

- Its "Other resources with the same gap" section names the load balancer resources
  `pcd_loadbalancer_*`. The registered type names are **`pcd_lb_loadbalancer`, `pcd_lb_listener`,
  `pcd_lb_pool`, `pcd_lb_member`, `pcd_lb_monitor`** (`loadbalancer_resource.go:64`,
  `listener_resource.go:71`, `pool_resource.go:77`, `member_resource.go:65`, `monitor_resource.go:66`).
  Do not propagate the wrong names into the changelog.
- Its Non-goals say "Changing which statuses the waiters treat as failures." This change does exactly
  that, deliberately, for two delete waiters. Record the amendment rather than silently contradicting it.
- Resources fixed here move out of that section and into scope.

## Changelog

One `### Fixed` bullet per resource, American English, no ticket references. Do **not** claim that
Octavia permits at most one health monitor per pool — gophercloud's own documentation says otherwise and
nothing here establishes it.
