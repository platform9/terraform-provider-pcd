# `pcd_host_cluster_role`: a clean plan after import

**Date:** 2026-09-03
**Status:** approved design, awaiting implementation plan
**Scope:** `internal/services/resmgr/host_cluster_role_resource.go` and its tests

## Problem

Importing a `pcd_host_cluster_role` (`terraform import` or an `import {}` block) always
leaves the next plan with one change:

```
  # pcd_host_cluster_role.hyp1["hypervisor"] will be updated in-place
  ~ resource "pcd_host_cluster_role" "hyp1" {
      + wait_until_converged = false
    }
```

`wait_until_converged` is declared `Optional + Computed` with `Default: false`.
`ImportState` sets only `id`, `host_id` and `role`, and `Read` never touches the flag, so
the imported state holds `null`. At plan time the schema default fills `false`, and
`null -> false` is a diff. Applying it runs `Update`, which unconditionally re-PUTs the
role assignment to `PUT /resmgr/v2/hosts/<host_id>/roles/<role>` — a real resmgr write
(an empty body for `hypervisor` and `image-library`) for a change that concerns the
provider alone. `lifecycle { ignore_changes }` cannot suppress it because the default is
applied on the provider side.

Observed on 2026-09-03 while importing the CE lab's three roles for the PCD-9783 destroy
test; the imports had to be done with the CLI to avoid the PUTs.

This is the only resource in the resmgr package with a schema default. `pcd_host_role`
and `pcd_host_config_assignment` carry no client-side attributes and import clean.

## Goal

After import, `terraform plan` reports no changes when the configuration matches the
server, and a change that touches only `wait_until_converged` never calls resmgr.

## Non-goals

- Reading `host_cluster` or `backends` back from the server. Both stay echo-only: an
  imported hypervisor whose configuration sets `host_cluster` will still plan an update
  and PUT it. Closing that gap needs a Nova aggregate lookup and a lossy mapping from
  rendered `pf9-cindervolume-config` settings back to blueprint keys; it is a separate
  piece of work.
- Changing the schema. The attribute stays `Optional + Computed + Default(false)` so
  existing states, which already hold `false`, see no upgrade diff.

## Design

### 1. `ImportState` writes the default

`ImportState` sets `wait_until_converged = false` alongside `id`, `host_id` and `role`.
The imported state then equals what the default will plan, and the diff disappears.
`Read` is unchanged: it already carries the prior state's flag forward.

`false` rather than `true` because the flag is a create/update-time behavior with no
server-side meaning; an imported role has nothing to wait for.

### 2. `Update` skips resmgr when only client-side attributes changed

A pure helper decides whether the server-side options differ between the plan and the
prior state:

```go
// roleOptionsChanged reports whether the PUT body would differ from what the
// server already has: host_cluster (hypervisor) or backends (persistent-storage).
func roleOptionsChanged(plan, state *hostClusterRoleModel) bool
```

Equality rules:

- `host_cluster`: compared as strings after treating null and `""` as the same value,
  because `assignBody` omits both from the PUT.
- `backends`: compared as string lists, order-sensitive, after treating null and
  unknown as "not sent"; an empty list is sent as `[]` and so differs from null. This
  mirrors what `assignBody` puts on the wire, so "changed" means "the body changes".

`Update` reads both plan and prior state, and when the helper returns `false` it writes
the plan to state and returns — no `putRole`, no `waitConverged`. When it returns
`true`, the existing path runs unchanged: `assignBody`, `putRole` with its 409 retry,
and `waitConverged` if the flag is set.

A flag flip alone (`false -> true`) therefore stores the new value without blocking on
convergence. Waiting is a side effect of an assignment, not a standalone action; a user
who wants to block on an already-assigned role can taint or replace it.

### 3. Unchanged

`Create`, `Read`, `Delete`, `ValidateConfig`, `assignBody`, `putRole`, `waitConverged`,
the schema and the generated docs.

## Behavior after the change

| Situation | Plan | resmgr calls |
| --- | --- | --- |
| Import, config omits the flag | no changes | none |
| Import, config sets `wait_until_converged = true` | update in place (flag) | none |
| Import, config sets `host_cluster` | update in place (`host_cluster`) | PUT, as today |
| Existing state (`false`), flip flag to `true` | update in place | none (was: PUT) |
| Change `backends` on persistent-storage | update in place | PUT + optional wait, as today |

## Testing

- **Unit** (package `resmgr`, no lab): table test for `roleOptionsChanged` covering
  identical options, null vs `""` host cluster, changed host cluster, null vs empty vs
  changed backends, and unknown backends. The existing `_internal_test.go` files show the
  pattern.
- **Unit**: `ImportState` invoked directly with a `tfsdk.State` built from the resource
  schema, asserting the four attributes it sets. If constructing the state proves
  disproportionate, the acceptance step below is the fallback and the plan says so.
- **Acceptance** (lab, gated by `PCD_ACC_RESMGR=1` like `TestAccResmgrHostConfig`): assign
  `image-library` to the lab host, then an `ImportState: true, ImportStateVerify: true`
  step, then a plan-only step asserting no changes. `image-library` is chosen because it
  has no options, so the test exercises exactly the flag.

## Changelog and docs

`CHANGELOG.md` gains a new `## [Unreleased]` heading with one `Fixed` entry describing
the spurious import diff and the no-op PUT. No documentation changes: the schema text is
unchanged and the import syntax is already documented.

## Compatibility

No schema change, no state upgrade. Roles created before this change already store
`false` and keep planning clean. The only behavioral difference a user can observe is
that flipping the flag no longer re-PUTs the role.

## Risks

- A future option added to the role must be added to `roleOptionsChanged`, or changes to
  it would be silently skipped. The helper's doc comment and its test say so; keeping
  the comparison next to `assignBody` makes the pairing visible.
- If resmgr ever attaches server-side meaning to a repeated PUT (for example, forcing a
  re-render of role settings), users who relied on the flag flip to trigger it lose that
  side channel. Nothing documents such a use, and `terraform apply -replace` covers it.
