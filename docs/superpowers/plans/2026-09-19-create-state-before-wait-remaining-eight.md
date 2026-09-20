# Create Records Its Object Before It Waits: The Remaining Eight — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make the eight remaining resources record the created object in Terraform state as soon as the service accepts it, so a failed or interrupted wait leaves a tainted resource instead of an orphan — without making any destroy worse in the process.

**Architecture:** `blockstorage.recordCreated` is promoted to `tfstate.RecordCreated` with an ID guard and reused by all thirteen call sites. Two delete waiters become transition-aware so they stop bailing on the very status a failed create wait abandons. The four load balancer children get their delete path unblocked *first*, as its own lab-verified task, because recording them before that would leave resources that cannot be destroyed. Every one of the eight also has its unconditional read-back tail guarded, or the record it just wrote would be clobbered with unknown values.

**Tech Stack:** Go 1.25.8, terraform-plugin-framework, gophercloud v2.13.0 (Designate v2, Octavia v2.0, Barbican v1), `net/http/httptest` for the service fakes.

**Spec:** `docs/superpowers/specs/2026-09-19-create-state-before-wait-remaining-eight-design.md`

## Global Constraints

- Branch from `main` as `pushkar/<topic>`. No `Co-Authored-By: Claude` trailer and no Claude Code footer in commits or PRs — write them as the repository owner would. American English throughout.
- Module path `github.com/platform9/terraform-provider-pcd`. Every new file starts with:
  ```go
  // Copyright (c) Platform9 Systems, Inc.
  // SPDX-License-Identifier: MPL-2.0
  ```
- Unit tests: `go test ./internal/... -timeout 180s`. Lint: `golangci-lint run ./...`. Vet: `go vet ./...`. Format: `gofmt -l .`. All must be clean before each commit.
- **The registered type names are `pcd_lb_loadbalancer`, `pcd_lb_listener`, `pcd_lb_pool`, `pcd_lb_member`, `pcd_lb_monitor`** — not `pcd_loadbalancer_*`. The 2026-09-19 design document has these wrong; Task 6 fixes it. Never write the wrong form into a comment, changelog entry, or commit message.
- Do **not** change any create-side wait's target status, timeout, or error message, and do not change `waitForLoadBalancerActive` itself. The only waiter changes in this plan are on **delete** paths, and they are named explicitly in the task that makes them.
- Task ordering is load-bearing: **0, then {1, 2, 4} in parallel, then 3, then 5, then 6.** Task 5 must not ship until Task 3 has been verified on the CE lab.

#### Facts every task's test fake depends on (verified against gophercloud v2.13.0)

| Service | `ResourceBase` the client sets | Paths a fake must answer | Create OK | Get OK | Delete OK |
| --- | --- | --- | --- | --- | --- |
| Designate (dns) | `v2/` | `/v2/zones`, `/v2/zones/{id}/recordsets` | 201 **or** 202 | 200 | **202 only** |
| Octavia (loadbalancer) | `v2.0/` | `/v2.0/lbaas/loadbalancers`, `/v2.0/lbaas/listeners`, `/v2.0/lbaas/pools`, `/v2.0/lbaas/pools/{id}/members`, `/v2.0/lbaas/healthmonitors` | 201 or 202 | 200 | 202 or 204 |
| Barbican (keymanager) | `v1/` | `/v1/secrets` | **201 only** | 200 | 202 or 204 |

All three set `ResourceBase`, so every path carries its version prefix — verified in `openstack/client.go` at lines 443 (dns), 463 (load-balancer, which also strips a `v2.0/` already present on the catalog endpoint) and 492 (key-manager). Note the consequence for the fakes: `applyOverride` in `internal/clients/config.go:359-364` blanks `ResourceBase` when an `endpoint_overrides` entry exists, which would drop the prefix, so a test's `clients.Config` must leave `EndpointOverrides` empty and point the client at the fake through `ProviderClient.EndpointLocator` only.

Barbican timestamps parse only as `gophercloud.RFC3339NoZ` — emit them without a trailing `Z`, or omit them. Designate's delete accepts **202 alone**: a fake answering 204 turns a successful destroy into an unexpected-response-code error.

A fake that answers the wrong status code fails with an unexpected-response-code error rather than the behavior under test. Barbican timestamps parse only as `gophercloud.RFC3339NoZ` — emit them without a trailing `Z`, or omit them.

---

### Task 0: Promote the helper to `tfstate.RecordCreated`

**Files:**
- Modify: `internal/tfstate/tfstate.go` (add `RecordCreated`)
- Modify: `internal/tfstate/tfstate_test.go` (test the ID guard)
- Modify: `internal/services/blockstorage/blockstorage.go` (delete `recordCreated`)
- Modify: `internal/services/blockstorage/volume_resource.go:134`, `snapshot_resource.go:111`, `backup_resource.go:114` (call sites)
- Modify: `internal/services/blockstorage/failed_create_internal_test.go` (two comment references, around lines 149-152)

**Interfaces:**
- Consumes: `tfstate.NullUnknowns(state *tfsdk.State) diag.Diagnostics`.
- Produces: `tfstate.RecordCreated(ctx context.Context, resp *resource.CreateResponse, plan any) bool` — every later task calls it.

This is a pure refactor plus one new guard. No resource behavior changes. `grep -rn recordCreated internal/` finds exactly the seven lines to touch.

- [ ] **Step 1: Write the failing test for the ID guard**

In `internal/tfstate/tfstate_test.go`, add a test that builds a throwaway schema with a root `id` attribute, calls `RecordCreated` with a plan whose `id` is null, and asserts that it returns `false`, that `resp.State.Raw` is null (the row was dropped, not written), and that the diagnostics carry an error. Add a second test for the happy path: a plan with a known `id` and some unknown computed attributes returns `true`, leaves state non-null and fully known, and preserves the id.

The guard exists because a state row without an ID names nothing a destroy could delete — `secret_resource.go:215` would issue `DELETE /v1/secrets/` with an empty last segment, recoverable only with `terraform state rm`.

- [ ] **Step 2: Run it and watch it fail**

Run: `go test ./internal/tfstate/... -v`
Expected: compile failure — `RecordCreated` is undefined.

- [ ] **Step 3: Add `RecordCreated`**

In `internal/tfstate/tfstate.go`:

```go
// RecordCreated saves a just-created object to state before Create waits for it
// to become usable. The service keeps an object whose build fails or whose wait
// is interrupted, so the wait's error has to be returned with the object in
// state: Terraform then marks the resource tainted, and the next apply or a
// destroy deletes it instead of leaving it behind and creating another. The
// attributes the service has not reported yet are saved as null, since Terraform
// refuses unknown values in state, and the next refresh reads them. It reports
// whether Create should carry on. Callers must set the object's ID on plan
// first: a state row without one names nothing the destroy could delete, so
// RecordCreated drops the row rather than record it.
func RecordCreated(ctx context.Context, resp *resource.CreateResponse, plan any) bool {
	resp.Diagnostics.Append(resp.State.Set(ctx, plan)...)
	resp.Diagnostics.Append(NullUnknowns(&resp.State)...)
	if resp.Diagnostics.HasError() {
		return false
	}
	var id types.String
	resp.Diagnostics.Append(resp.State.GetAttribute(ctx, path.Root("id"), &id)...)
	if resp.Diagnostics.HasError() {
		return false
	}
	if id.IsNull() || id.ValueString() == "" {
		resp.State.RemoveResource(ctx)
		resp.Diagnostics.AddError("Recording the created object",
			"The object was created but no ID was recorded for it, so it was left out of state. "+
				"This is a bug in the provider.")
		return false
	}
	return true
}
```

New imports: `context`, `terraform-plugin-framework/path`, `terraform-plugin-framework/resource`, `terraform-plugin-framework/types`.

- [ ] **Step 4: Move the blockstorage call sites onto it**

Delete `recordCreated` from `internal/services/blockstorage/blockstorage.go` along with the imports it alone needed, and change the three call sites to `tfstate.RecordCreated(ctx, resp, &plan)`. Update the two comment references in `failed_create_internal_test.go` that name `recordCreated`.

- [ ] **Step 5: Verify**

Run: `go test ./internal/... -timeout 180s && go vet ./... && golangci-lint run ./... && gofmt -l .`
Expected: all pass, no output from `gofmt`. In particular every existing blockstorage and images test still passes — this task changes no behavior.

- [ ] **Step 6: Commit**

```bash
git add internal/tfstate/ internal/services/blockstorage/
git commit -m "refactor: share recordCreated as tfstate.RecordCreated, and guard the ID

Eight more Creates are about to record their object before waiting for it, in
three more packages. Promote the block storage helper rather than copy it, and
have it verify that the caller set an ID: a state row without one names nothing
a destroy could delete, so it is worse than no row at all. It now drops the row
and reports a provider bug instead of recording one that cannot be removed."
```

---
### Task 1a: `pcd_dns_recordset`

Depends on Task 0. Do this **before** the zone: it proves the Designate fake against the resource that needs no waiter change. It creates `internal/services/dns/failed_create_internal_test.go` and owns `fakeConfig` in that package — Task 1b appends to this file and must not redeclare it.

**Files:**
#### Prerequisite

This task consumes `tfstate.RecordCreated`, which **Task 0** adds. Do not start until Task 0 has landed. Do **not** inline the three lines or add a package-local `dns.recordCreated` as a workaround — package `dns` has exactly two call sites (this one and `zone_resource.go`), and the settled design routes all eleven through the one shared helper.

#### Modify

- **`internal/services/dns/recordset_resource.go`**
  - **line 26** (import block, lines 10–27): add `"github.com/platform9/terraform-provider-pcd/internal/tfstate"` immediately after the existing `internal/clients` import, in the same group. This matches `internal/services/images/image_resource.go:38-39`, which groups `clients` and `tfstate` together.
  - **lines 111–124** (`Create`, currently lines 84–124): insert the ID assignment and the record between the create-error check and the wait, and guard the post-wait tail. After the edit `Create` spans lines 85–141.

#### Create (test)

- **`internal/services/dns/failed_create_internal_test.go`** — new file, `package dns`, 173 lines. Name per the settled design's convention: package `dns` has two resources sharing one fake-server file, so it is `failed_create_internal_test.go` (matching `internal/services/blockstorage/failed_create_internal_test.go`), not `recordset_failed_create_internal_test.go`.
  - **lines 23–33**: the `fakeConfig` helper this file introduces for the whole package. **Task 1b (`pcd_dns_zone`) reuses it — do not define a second copy there.**
  - **lines 41–173**: `TestRecordSetCreateKeepsARecordSetThatFailedToBuild`.

#### Deliberately NOT modified

- **`internal/services/dns/dns.go`** — no waiter change. `waitForRecordSetDeleted` (dns.go:165-182) inspects no status at all: its predicate calls `recordsets.Get`, returns `true` on 404 (dns.go:171-173), returns the error on any other error (dns.go:174), and otherwise `return false, nil` (dns.go:176). It therefore polls straight through `ERROR` and `PENDING_DELETE` until Designate answers 404. The recordset needs nothing to become destroyable once tainted. **`waitForZoneDeleted` (dns.go:118-138) does bail on ERROR at dns.go:129-131 — that is Task 1b's problem and must not be "made consistent" with the recordset waiter. The asymmetry is the correct direction.**
- **`internal/services/dns/recordset_resource.go:196-221` (`Delete`)** — unchanged. `recordsets.Delete` is issued immediately at line 211 with no pre-delete wait of any kind, and a 404 is swallowed as success at lines 212–214.
- **`internal/services/dns/recordset_resource.go:186`** (the Update-path wait) — out of scope.

#### Doc bookkeeping (belongs to Tasks 0 and 6, listed here so it is not lost)

- `docs/superpowers/specs/2026-09-19-create-state-before-wait-design.md:186` — the line `` - `pcd_dns_recordset` — `internal/services/dns/recordset_resource.go:116` `` moves out of **Other resources with the same gap** (lines 179–197) and into **Scope** (line 6). The count at line 181 ("Eight more resources") drops accordingly.
- `CHANGELOG.md` — one `### Fixed` bullet under `## [Unreleased]` (currently line 7).

**Interfaces:**
#### Consumes

```go
// internal/tfstate/tfstate.go — added by Task 0.
func RecordCreated(ctx context.Context, resp *resource.CreateResponse, plan any) bool
```
Callers must set the object's ID on `plan` before calling; `RecordCreated` drops the row and errors loudly if the recorded `id` is null or empty. This resource satisfies that with `plan.ID = types.StringValue(rr.ID)` on the line above.

```go
// internal/services/dns/dns.go:141 — unchanged.
func waitForRecordSetActive(ctx context.Context, client *gophercloud.ServiceClient, zoneID, rrID string, timeout time.Duration) error

// internal/services/dns/dns.go:165 — unchanged, status-blind, needs no fix.
func waitForRecordSetDeleted(ctx context.Context, client *gophercloud.ServiceClient, zoneID, rrID string, timeout time.Duration) error

// internal/services/dns/recordset_resource.go:233 — unchanged signature; the tail now uses the
// notFound return it currently discards with `_`.
func (r *recordSetResource) readInto(ctx context.Context, client *gophercloud.ServiceClient, zoneID, rrID string, m *recordSetModel) (notFound bool, diags diag.Diagnostics)
```

```go
// internal/services/dns/dns.go:75 — unchanged.
const defaultDNSTimeout = 10 * time.Minute
// internal/services/dns/dns.go:70-72 — unchanged.
const dnsActive = "ACTIVE"; const dnsError = "ERROR"
```

#### Produces

```go
// internal/services/dns/failed_create_internal_test.go:26 — NEW, package-scoped test helper.
// Task 1b (pcd_dns_zone) MUST reuse this and MUST NOT declare its own; a second
// declaration in package dns is a compile error.
func fakeConfig(url string) *clients.Config
```

No exported API changes. `recordSetModel` (recordset_resource.go:44-54), the schema (lines 60–78) and every method signature are untouched, so nothing outside package `dns` recompiles differently.

- [ ] **Step 1: Write the failing test**

Add to the test file named above:

```go
// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package dns

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

// fakeConfig points a clients.Config at a test server, so the Designate client
// the resources build resolves to it. The locator ignores the endpoint options,
// so it answers for every service type and availability. EndpointOverrides is
// deliberately left empty: applyOverride clears the client's ResourceBase, which
// would drop the "v2/" prefix NewDNSV2 sets and make every path below a miss.
func fakeConfig(url string) *clients.Config {
	return &clients.Config{
		Region: "region-one",
		Provider: &gophercloud.ProviderClient{
			EndpointLocator: func(gophercloud.EndpointOpts) (string, error) { return url + "/", nil },
		},
	}
}

// A recordset whose creation ends in ERROR stays in Designate. Create used to
// return without state, so Terraform forgot the recordset, the next apply
// created another, and the first had to be deleted by hand. Create must return
// the error with the recordset in state (Terraform then taints it), and the
// refresh and delete a destroy runs must remove the recordset even though
// Designate reports ERROR.
func TestRecordSetCreateKeepsARecordSetThatFailedToBuild(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	var mu sync.Mutex
	deleteCalled, getsAfterDelete := false, 0
	designate := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch r.Method + " " + r.URL.Path {
		case "POST /v2/zones/zone-1/recordsets":
			// Designate answers 202 for a recordset create, and gophercloud
			// decodes the body regardless of the status, so it must not be empty.
			w.WriteHeader(http.StatusAccepted)
			fmt.Fprint(w, `{"id": "rr-1", "zone_id": "zone-1", "name": "www.tf-acc-example.com.",
				"type": "A", "records": ["10.1.0.1"], "ttl": 300, "status": "PENDING",
				"action": "CREATE", "description": ""}`)
		case "GET /v2/zones/zone-1/recordsets/rr-1":
			if deleteCalled {
				getsAfterDelete++
				if getsAfterDelete > 1 {
					w.WriteHeader(http.StatusNotFound)
					return
				}
			}
			// No created_at/updated_at: RecordSet.UnmarshalJSON parses them with
			// gophercloud's no-Z layout, so a Z-suffixed value fails Extract.
			fmt.Fprint(w, `{"id": "rr-1", "zone_id": "zone-1", "name": "www.tf-acc-example.com.",
				"type": "A", "records": ["10.1.0.1"], "ttl": 300, "status": "ERROR",
				"action": "CREATE", "description": ""}`)
		case "DELETE /v2/zones/zone-1/recordsets/rr-1":
			deleteCalled = true
			w.WriteHeader(http.StatusAccepted)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotImplemented)
		}
	}))
	defer designate.Close()

	r := &recordSetResource{config: fakeConfig(designate.URL)}
	var sch resource.SchemaResponse
	r.Schema(ctx, resource.SchemaRequest{}, &sch)
	s := sch.Schema

	records, d := types.SetValueFrom(ctx, types.StringType, []string{"10.1.0.1"})
	if d.HasError() {
		t.Fatalf("building the records set: %v", d)
	}
	planned := recordSetModel{
		ID:          types.StringUnknown(),
		ZoneID:      types.StringValue("zone-1"),
		Name:        types.StringValue("www.tf-acc-example.com."),
		Type:        types.StringValue("A"),
		Records:     records,
		TTL:         types.Int64Unknown(),
		Description: types.StringUnknown(),
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
		t.Fatal("create succeeded; want the ERROR status reported")
	}
	if createResp.State.Raw.IsNull() {
		t.Fatal("create returned no state: Terraform forgets rr-1 and the next apply creates a second recordset")
	}
	if !createResp.State.Raw.IsFullyKnown() {
		t.Fatalf("create state holds unknown values, which Terraform refuses: %v", createResp.State.Raw)
	}
	var got recordSetModel
	if d := createResp.State.Get(ctx, &got); d.HasError() {
		t.Fatalf("reading the create state: %v", d)
	}
	if got.ID.ValueString() != "rr-1" || got.ZoneID.ValueString() != "zone-1" ||
		got.Name.ValueString() != "www.tf-acc-example.com." || got.Type.ValueString() != "A" {
		t.Fatalf("create state id=%s zone_id=%s name=%s type=%s; want rr-1, zone-1, www.tf-acc-example.com., A",
			got.ID, got.ZoneID, got.Name, got.Type)
	}
	// records is Required and echo-only, so the configured value must survive
	// into the recorded row: readInto refills it only when it is null.
	var recorded []string
	if d := got.Records.ElementsAs(ctx, &recorded, false); d.HasError() {
		t.Fatalf("reading records from the create state: %v", d)
	}
	if len(recorded) != 1 || recorded[0] != "10.1.0.1" {
		t.Fatalf("create state records = %v, want [10.1.0.1]", recorded)
	}

	// terraform destroy (or the replacing apply) refreshes the tainted recordset first.
	readResp := resource.ReadResponse{State: createResp.State}
	r.Read(ctx, resource.ReadRequest{State: createResp.State}, &readResp)
	if readResp.Diagnostics.HasError() {
		t.Fatalf("refresh of the failed recordset: %v", readResp.Diagnostics)
	}
	if readResp.State.Raw.IsNull() {
		t.Fatal("refresh dropped rr-1 from state")
	}
	if d := readResp.State.Get(ctx, &got); d.HasError() {
		t.Fatalf("reading the refreshed state: %v", d)
	}
	if got.Status.ValueString() != "ERROR" {
		t.Fatalf("refreshed status = %s, want ERROR", got.Status)
	}
	if got.TTL.ValueInt64() != 300 {
		t.Fatalf("refreshed ttl = %d, want 300", got.TTL.ValueInt64())
	}
	if got.Region.ValueString() != "region-one" {
		t.Fatalf("refreshed region = %s, want region-one", got.Region)
	}

	deleteResp := resource.DeleteResponse{State: readResp.State}
	r.Delete(ctx, resource.DeleteRequest{State: readResp.State}, &deleteResp)
	if deleteResp.Diagnostics.HasError() {
		t.Fatalf("delete of a recordset in ERROR: %v", deleteResp.Diagnostics)
	}
	mu.Lock()
	defer mu.Unlock()
	if !deleteCalled {
		t.Fatal("delete never called DELETE /v2/zones/zone-1/recordsets/rr-1")
	}
	if getsAfterDelete < 2 {
		t.Fatalf("delete returned after %d polls; want it to wait out the ERROR status until Designate answers 404", getsAfterDelete)
	}
}
```

- [ ] **Step 2: Run it and watch it fail**

Run the focused test for this resource. It must fail because `Create` returns no state — not for a compile error or a fake-server mismatch. If it fails for another reason, fix the test before touching the resource.

- [ ] **Step 3: Write the implementation**

#### Edit 1 — import block, `internal/services/dns/recordset_resource.go`

**Before (lines 10–27):**

```go
import (
	"context"
	"fmt"
	"net/http"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/dns/v2/recordsets"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/int64planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/platform9/terraform-provider-pcd/internal/clients"
)
```

**After (lines 10–28):**

```go
import (
	"context"
	"fmt"
	"net/http"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/dns/v2/recordsets"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/int64planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/platform9/terraform-provider-pcd/internal/clients"
	"github.com/platform9/terraform-provider-pcd/internal/tfstate"
)
```

`fmt` (line 12), `types` (line 24) and `path` (line 18) are already imported — the new tail guard and the ID assignment need no further imports.

#### Edit 2 — `Create`, `internal/services/dns/recordset_resource.go`

**Before (lines 111–124, the tail of `Create`; line 110 is the blank line after the `if resp.Diagnostics.HasError()` guard at 107–109):**

```go
	rr, err := recordsets.Create(ctx, client, zoneID, createOpts).Extract()
	if err != nil {
		resp.Diagnostics.AddError("dns: creating recordset", err.Error())
		return
	}
	if err := waitForRecordSetActive(ctx, client, zoneID, rr.ID, defaultDNSTimeout); err != nil {
		resp.Diagnostics.AddError("dns: waiting for recordset to become active", err.Error())
		return
	}

	_, readDiags := r.readInto(ctx, client, zoneID, rr.ID, &plan)
	resp.Diagnostics.Append(readDiags...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}
```

**After (lines 112–141, after the import shifted everything by one):**

```go
	rr, err := recordsets.Create(ctx, client, zoneID, createOpts).Extract()
	if err != nil {
		resp.Diagnostics.AddError("dns: creating recordset", err.Error())
		return
	}

	plan.ID = types.StringValue(rr.ID)
	// Designate holds the recordset from here on, whatever the wait does next.
	if !tfstate.RecordCreated(ctx, resp, &plan) {
		return
	}

	if err := waitForRecordSetActive(ctx, client, zoneID, rr.ID, defaultDNSTimeout); err != nil {
		resp.Diagnostics.AddError("dns: waiting for recordset to become active", err.Error())
		return
	}

	notFound, readDiags := r.readInto(ctx, client, zoneID, rr.ID, &plan)
	resp.Diagnostics.Append(readDiags...)
	if notFound {
		resp.Diagnostics.AddError("dns: reading recordset after create",
			fmt.Sprintf("Recordset %s no longer exists.", rr.ID))
		resp.State.RemoveResource(ctx)
		return
	}
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}
```

#### Why the tail guard is part of this edit, not a separate cleanup

`readInto` (recordset_resource.go:233-241) returns **before assigning anything to the model** on a 404 and on any other error. So on either path `plan` still carries the create plan's unknowns — `ttl`, `description`, `status`, `region` — and today's unconditional `resp.State.Set(ctx, &plan)` at line 123 would write them straight into state. Terraform core rejects unknown values in state outright. That is latent today only because there is no recorded row to clobber; the moment `RecordCreated` lands it is live, and it would clobber the clean, unknown-free row this fix just wrote. This is the reason the five already-shipped resources are **not** a complete template: `volume_resource.go:139-150` does an explicit `volumes.Get` with an error return plus `r.flatten`, so it never had this shape.

The two branches need opposite handling and must not be collapsed:

- **`notFound`** — the recordset is proven gone. `AddError` + `RemoveResource(ctx)` + `return`. Keeping a row that asserts an object Designate has deleted would leave the user with a state entry no destroy can clear. This matches this resource's own `Read` (recordset_resource.go:140-145) and the shipped `deleteCreatedImage` precedent, which removes state only when the object is known gone.
- **`readInto` errored** — the recordset exists but could not be read (a transient 5xx, say). `return` early and **leave the recorded row in place**, so Terraform taints it and the next apply or destroy deletes it.

**Do not** substitute a second `tfstate.NullUnknowns(&resp.State)` after the final `State.Set` as a cheaper alternative. It would make the `notFound` case "succeed" by writing a row whose every computed attribute is null, which is the undeletable-phantom outcome the guard exists to prevent. This was considered and rejected.

#### Before and after, traced

| Abandonment | Today | After this change |
| --- | --- | --- |
| Designate sets `ERROR` (dns.go:152-153) | Apply fails, no state. Terraform has forgotten `rr-1`. The next apply POSTs a second recordset with the same name and type, and Designate returns 409 `duplicate recordset` — the apply is wedged until someone runs `openstack recordset delete` by hand. | Apply fails with the same message, `rr-1` is in state and tainted. `terraform destroy` issues `DELETE /v2/zones/{z}/recordsets/rr-1`, and `waitForRecordSetDeleted` polls past `ERROR` and `PENDING_DELETE` to the 404. No manual step. |
| 10-minute timeout or `Ctrl-C` during the wait (dns.go:142, util.go:102-103) | Same orphan, with the recordset left in `PENDING` rather than `ERROR`. | Recorded before the wait starts, so a canceled context loses only the wait, never the record. `PENDING` is not a status any delete path inspects, so the destroy is unaffected. |
| One transient non-404 `recordsets.Get` failure during the wait (dns.go:146-148 — `gophercloud.WaitFor` aborts on the first predicate error) | Same orphan, from a single network blip. | Tainted instead of orphaned. Widening the retry policy is explicitly out of scope. |
| Happy path | — | Unchanged. Lines 129–140 overwrite the provisional row wholesale via `readInto`, which covers every attribute that can be unknown in a create plan (`id`, `ttl`, `description`, `status`, `region`). |

#### Notes for whoever executes this

- **`plan.ID` is load-bearing.** `recordSetModel.ID` is assigned **only** inside `readInto` (recordset_resource.go:243), which the failure path never reaches. Omit `plan.ID = types.StringValue(rr.ID)` and `RecordCreated`'s ID guard drops the row and reports a provider bug — which is the safe outcome, but the resource is then no better off than today. The line is not optional.
- **The recorded row's `status` is null, not `ERROR`.** `NullUnknowns` nulls it because Designate had not reported it when the record was written. `terraform show` displays it as empty until the next refresh fills it in. This is identical to the shipped volume/snapshot/backup behavior and is not a defect.
- **`records` must stay known.** It is `Required` (recordset_resource.go:71) and echo-only: `readInto` repopulates it from the server *only* when it is null or unknown (recordset_resource.go:255-263), because Designate canonicalizes some record types — TXT is rewritten to the RFC-1035 quoted form — and overwriting the configured value would fail the apply consistency check. It is known in the plan, so recording the plan keeps it. Do not try to clear it before recording.

- [ ] **Step 4: Run the test and watch it pass**

Run the focused test again. Then the full suite.

- [ ] **Step 5: Verify**

```bash
go build ./...
```
```bash
gofmt -l internal/services/dns internal/tfstate
```
```bash
go vet ./internal/services/dns/
```
```bash
go test ./internal/services/dns/ -run TestRecordSetCreateKeepsARecordSetThatFailedToBuild -v -count=1
```
```bash
git stash push -u -m 'verify-recordset-fix' -- internal/services/dns/recordset_resource.go && go test ./internal/services/dns/ -run TestRecordSetCreateKeepsARecordSetThatFailedToBuild -count=1; git stash list --format='%H %gs' | head -1
```
```bash
make test
```
```bash
golangci-lint run ./internal/services/dns/...
```

- [ ] **Step 6: Commit**

```
fix(dns): keep a recordset whose create wait fails in state

A recordset Designate accepts and then leaves in ERROR stayed in Designate
while the failed apply saved no state at all: Create called recordsets.Create,
waited for ACTIVE, and returned the wait's error with no resp.State.Set in
between. Terraform lost track of a recordset that exists, so the next apply
POSTed a second one with the same name and type, Designate refused it as a
duplicate, and the apply stayed wedged until someone ran
`openstack recordset delete` by hand. A wait that timed out after ten minutes
and an apply interrupted with Ctrl-C lost the recordset the same way, as did a
single transient poll failure, because gophercloud.WaitFor gives up on the
first error its predicate returns.

Create now sets the recordset's ID on the plan and records it through
tfstate.RecordCreated before it starts waiting. The apply still fails with
Designate's reason, and Terraform marks the recordset tainted, so the next
apply deletes and recreates it and a destroy deletes it. The attributes
Designate had not reported yet -- ttl, description, status and region -- are
saved empty until the next refresh, since Terraform refuses unknown values in
state.

The read-back after the wait was also made conditional. It discarded the
notFound flag and set state unconditionally, so a recordset that 404ed or
could not be read between the wait and the read-back would have written the
plan's unknown values over the row just recorded. A recordset that is gone is
now dropped from state; one that exists but cannot be read keeps its recorded
row, so Terraform taints it rather than forgetting it.

No delete path changed, and none needed to: waitForRecordSetDeleted inspects
no status and polls straight through ERROR and PENDING_DELETE to the 404, and
Delete issues the DELETE immediately with no pre-delete wait. The two DNS
resources are NOT a matched pair -- pcd_dns_zone's waitForZoneDeleted does bail
on ERROR and needs its own fix. Do not make the two waiters consistent; the
asymmetry is deliberate and correct.
```

**Open questions and traps recorded during planning:**

- Unverified against a real Designate, and cheap to check on the CE lab: that a recordset DELETE returns 202 and not 204. gophercloud pins recordsets.Delete's OkCodes to {202} alone (recordsets/requests.go:190), so a 204 would be turned into a synthesized ErrUnexpectedResponseCode and the destroy would fail. The Designate v2 API reference documents 202. This is pre-existing behavior that the fix neither improves nor worsens -- today the same 204 would break a destroy of a healthy recordset -- but recording state makes the ERROR-recordset destroy a path users will actually hit, so confirm it while the lab is up.
- Pre-existing and out of scope, but worth stating in the plan so nobody reports it as a regression: if the recordset is in ERROR because its parent ZONE is in ERROR or stuck PENDING, Designate accepts the DELETE (202) but the recordset can sit in PENDING_DELETE indefinitely, so waitForRecordSetDeleted times out after the 10-minute defaultDNSTimeout and Delete reports 'dns: waiting for recordset deletion'. Identical before and after this change. Task 1b's zone work is what makes that parent recoverable.
- Task sequencing: the settled design describes Task 1 as one task covering both DNS resources sharing one fake-server file. This section is written as the recordset half, to be done first because it proves the Designate fake (the /v2/ prefix, the 202-with-body create, the 202 delete, the no-Z timestamps) against the resource that needs no waiter change. If the two halves ship as ONE commit, the commit message above must be merged with Task 1b's and must keep the closing paragraph that says the two resources are not a matched pair -- that sentence is what stops a later reviewer or rebaser from 'fixing the inconsistency' between waitForZoneDeleted and waitForRecordSetDeleted.
- Per the Consistency-first graft, no cancel-during-wait test is added here: blockstorage's TestVolumeCreateKeepsAVolumeWhenTheWaitIsCanceled pins that mechanism for every call site once RecordCreated is shared. That depends on Task 0 also updating the comment at internal/services/blockstorage/failed_create_internal_test.go:149-152 to name tfstate.RecordCreated instead of the package-local recordCreated. If Task 0 skips that comment edit, the justification for omitting the test here is no longer written down anywhere and a reviewer will reasonably ask for it.
- Optional and not proposed: testAccCheckZoneDestroy (internal/services/dns/dns_test.go:111-131) iterates only pcd_dns_zone resources and never checks recordsets directly, so the acceptance suite would not notice a leaked recordset in a healthy zone -- exactly the orphan this fix addresses. Adding a recordset arm to CheckDestroy would close that, but it is a change to acceptance-test coverage rather than to this fix, and the settled design does not call for it.
- The settled design's UseStateForUnknown replan check (one CE lab run confirming a tainted resource re-applies without 'Provider produced inconsistent result after apply') is scoped there to the already-shipped pcd_blockstorage_volume, on the grounds that the exposure is identical across all thirteen call sites. pcd_dns_recordset has six UseStateForUnknown modifiers (recordset_resource.go:67, 72, 73, 74, 75 plus the shared useState slice), so it carries the same exposure and is covered by that single run. No separate lab run is proposed for this resource; flag it here only so nobody adds a redundant one.

---
### Task 1b: `pcd_dns_zone`

Depends on Task 1a (reuses its `fakeConfig`). This one **does** change a delete waiter: `waitForZoneDeleted` at `dns.go:129-131` becomes transition-aware. `waitForRecordSetDeleted` must NOT get the same treatment — it inspects no status and is correct as it is.

**Files:**
**Part of Task 1 (dns).** Do the `pcd_dns_recordset` section FIRST — it builds the Designate fake against the resource that needs no waiter change. This section then reuses that fake file and adds the one waiter change the recordset must *not* get.

**Prerequisite:** Task 0 must be merged. This section calls `tfstate.RecordCreated`, which Task 0 promotes out of `internal/services/blockstorage`.

#### Modify

1. `/Users/pushkarmulay/Documents/Claude Code Projects/Terraform Provider/terraform-provider-pcd/.claude/worktrees/inspiring-kilby-0bbd0b/internal/services/dns/dns.go`
   - **lines 117–138** — `waitForZoneDeleted`. Replace the unconditional ERROR bail at **lines 129–131** with a transition-aware latch, and name the last observed status in the timeout wrap at **line 135**. Sole caller is `zone_resource.go:228`, so the change is contained to this resource. File grows 138 → 197 lines in this function's region (whole file 182 → 197).
   - **Do NOT touch `waitForZoneActive` (lines 94–115)** — it is shared with `Update` at `zone_resource.go:197`, and a create wait that gives up on ERROR is correct.
   - **Do NOT touch `waitForRecordSetDeleted` (lines 165–182)** — it already inspects no status. The two DNS delete waiters are *not* a matched pair; see the commit message.
   - The package doc at **dns.go:5–11** stays accurate ("wait … to 404 after delete") and needs no edit.

2. `/Users/pushkarmulay/Documents/Claude Code Projects/Terraform Provider/terraform-provider-pcd/.claude/worktrees/inspiring-kilby-0bbd0b/internal/services/dns/zone_resource.go`
   - **line 28** — add the `internal/tfstate` import to the existing project-import group. `fmt` (line 12) and `types` (line 26) are already imported; nothing else is needed.
   - **after line 127** (the closing `}` of the `zones.Create` error check) — insert the record, immediately before the wait at line 128.
   - **lines 133–135** — replace the discarded-`notFound` tail with the guarded tail.
   - File grows 278 → 295 lines.

3. `/Users/pushkarmulay/Documents/Claude Code Projects/Terraform Provider/terraform-provider-pcd/.claude/worktrees/inspiring-kilby-0bbd0b/docs/superpowers/specs/2026-09-19-create-state-before-wait-design.md`
   - **line 185** — delete `- `pcd_dns_zone` — `internal/services/dns/zone_resource.go:128`` from **Other resources with the same gap**, and add `pcd_dns_zone` to the **Scope** section. Adjust the count at **line 14** and **line 181** ("eight more" → the remaining number) as each section lands.

4. `/Users/pushkarmulay/Documents/Claude Code Projects/Terraform Provider/terraform-provider-pcd/.claude/worktrees/inspiring-kilby-0bbd0b/CHANGELOG.md`
   - one `### Fixed` bullet under `## [Unreleased]` (after the existing `pcd_images_image` bullet). Exact text in the implementation diff, Edit 5.

#### Create (or extend, if the recordset section already created it)

5. `/Users/pushkarmulay/Documents/Claude Code Projects/Terraform Provider/terraform-provider-pcd/.claude/worktrees/inspiring-kilby-0bbd0b/internal/services/dns/failed_create_internal_test.go` — `package dns`, 164 lines. Naming follows the convention: several resources sharing one fake → `failed_create_internal_test.go` (as in `internal/services/blockstorage/`), not `zone_failed_create_internal_test.go`.
   - **If the recordset section already added this file**, it already contains `fakeConfig`. Append only `TestZoneCreateKeepsAZoneThatFailedToBuild` and drop the duplicate `fakeConfig` — `go build` fails on a redeclaration otherwise. The `fakeConfig` body below is identical either way.
   - There is no existing `fakeConfig` anywhere in `package dns` today (`internal/services/dns/pools_config_internal_test.go` is `package dns` and declares none), so there is no collision with committed code.

#### Do not add

- **No cancel-during-wait test.** `internal/services/blockstorage/failed_create_internal_test.go:153` (`TestVolumeCreateKeepsAVolumeWhenTheWaitIsCanceled`) pins the shared record-before-wait mechanism; once Task 0 makes `RecordCreated` provider-wide, that single test covers this call site too. Task 0 updates that test's comment at lines 149–152 to name `tfstate.RecordCreated`.

**Interfaces:**
#### Consumes

- `func RecordCreated(ctx context.Context, resp *resource.CreateResponse, plan any) bool` — `internal/tfstate/tfstate.go`, introduced by **Task 0**. Sets state from `plan`, nulls unknowns, then checks `path.Root("id")` is a non-empty known string; on a missing ID it calls `resp.State.RemoveResource(ctx)`, adds a "this is a bug in the provider" error and returns `false`. Callers must set `plan.ID` first.
- `func (r *zoneResource) readInto(ctx context.Context, client *gophercloud.ServiceClient, id string, m *zoneModel) (notFound bool, diags diag.Diagnostics)` — `internal/services/dns/zone_resource.go:265` (currently :237). Unchanged. Returns `(true, nil)` on a 404 **without assigning anything to `m`**; that is why the tail must branch on `notFound`. It backfills `m.Region` from `r.config.Region` only when region is null or unknown (`zone_resource.go:274-276`, currently), which is what repairs the null region `RecordCreated` writes.
- `func waitForZoneActive(ctx context.Context, client *gophercloud.ServiceClient, zoneID string, timeout time.Duration) error` — `internal/services/dns/dns.go:94`. **Unchanged.** Also called by `Update` at `zone_resource.go:197`.
- `zones.Create(ctx, client, opts) CreateResult` / `zones.Get(ctx, client, id) GetResult` / `zones.Delete(ctx, client, id) DeleteResult` — gophercloud v2.13.0 `openstack/dns/v2/zones`. `Create` OkCodes `[201, 202]` (`requests.go:146`); `Get` defaults to `[200]`; `Delete` OkCodes `[202]` **and** `JSONResponse: &r.Body` (`requests.go:203-206`).
- `gophercloud.WaitFor(ctx, predicate)` — `util.go:87-105`. Runs the predicate **immediately**, before the first 1-second tick, and calls it serially, which is what makes the captured `seenNonError`/`lastStatus` locals safe without a mutex.
- `openstack.NewDNSV2` sets `sc.ResourceBase = sc.Endpoint + "v2/"` (`openstack/client.go:441-445`), so every fake path carries a `/v2/` prefix. `Config.applyOverride` (`internal/clients/config.go:359-364`) blanks `ResourceBase`, but only when `EndpointOverrides["dns"]` is non-empty — the test fake sets none, so the prefix is present.

#### Produces

- `func waitForZoneDeleted(ctx context.Context, client *gophercloud.ServiceClient, zoneID string, timeout time.Duration) error` — `internal/services/dns/dns.go`. **Signature unchanged; semantics changed.** ERROR is now a delete failure only after the zone has been seen in some other status. Sole caller `zone_resource.go:228`.
- `func fakeConfig(url string) *clients.Config` — new package-level helper in `internal/services/dns/failed_create_internal_test.go`, shared with the recordset test in the same file. Returns `Region: "region-one"` and a `ProviderClient` whose `EndpointLocator` ignores its options and answers `url + "/"` for every service type and availability.
- `func TestZoneCreateKeepsAZoneThatFailedToBuild(t *testing.T)` — same file.

#### Unchanged public surface

No schema change, no new or renamed attribute, no change to `Metadata` (`pcd_dns_zone`), `Read`, `Update`, `Delete` or `ImportState`. No regeneration of `docs/resources/dns_zone.md` is needed (`make generate` produces no diff).

- [ ] **Step 1: Write the failing test**

Add to the test file named above:

```go
// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package dns

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

// fakeConfig points a clients.Config at a test server, so the Designate client
// the resources build resolves to it. The locator ignores the endpoint options,
// so it answers for every service type and availability. openstack.NewDNSV2
// appends "v2/" to the endpoint, so the server answers paths under /v2/.
//
// NOTE: if the pcd_dns_recordset section already added this file, fakeConfig is
// already declared here — append only the test function below.
func fakeConfig(url string) *clients.Config {
	return &clients.Config{
		Region: "region-one",
		Provider: &gophercloud.ProviderClient{
			EndpointLocator: func(gophercloud.EndpointOpts) (string, error) { return url + "/", nil },
		},
	}
}

// A zone whose build ends in ERROR stays in Designate. Create used to return
// without state, so Terraform forgot the zone, the next apply created another,
// and the first had to be deleted by hand. Create must return the error with
// the zone in state (Terraform then taints it), and the refresh and delete a
// destroy runs must remove the zone even though Designate still reports ERROR
// on the first poll after the DELETE is accepted.
func TestZoneCreateKeepsAZoneThatFailedToBuild(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	const zoneJSON = `{"id": "zone-1", "name": "tf-acc-failed.example.com.",
		"email": "dns@example.com", "type": "PRIMARY", "ttl": 3600, "description": "",
		"status": "ERROR", "action": "CREATE", "serial": 1, "pool_id": "pool-1",
		"project_id": "proj-1", "attributes": {}, "masters": []}`

	var mu sync.Mutex
	deleteCalled, getsAfterDelete := false, 0
	designate := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch r.Method + " " + r.URL.Path {
		case "POST /v2/zones":
			// Designate answers 201 or 202; the zone is bare, with no wrapper key.
			w.WriteHeader(http.StatusAccepted)
			fmt.Fprint(w, `{"id": "zone-1", "name": "tf-acc-failed.example.com.",
				"status": "PENDING", "action": "CREATE"}`)
		case "GET /v2/zones/zone-1":
			if deleteCalled {
				getsAfterDelete++
				// The first poll after the DELETE still reports ERROR: the delete
				// waiter must poll through it rather than bail.
				if getsAfterDelete > 1 {
					w.WriteHeader(http.StatusNotFound)
					return
				}
			}
			fmt.Fprint(w, zoneJSON)
		case "DELETE /v2/zones/zone-1":
			// zones.Delete accepts 202 only, and decodes the body, so an empty
			// body fails with io.EOF.
			deleteCalled = true
			w.WriteHeader(http.StatusAccepted)
			fmt.Fprint(w, `{"id": "zone-1", "status": "PENDING", "action": "DELETE"}`)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotImplemented)
		}
	}))
	defer designate.Close()

	r := &zoneResource{config: fakeConfig(designate.URL)}
	var sch resource.SchemaResponse
	r.Schema(ctx, resource.SchemaRequest{}, &sch)
	s := sch.Schema

	// No attribute in the zone schema carries a static default, so every
	// Optional+Computed attribute the config does not set is unknown here.
	planned := zoneModel{
		ID:          types.StringUnknown(),
		Name:        types.StringValue("tf-acc-failed.example.com."),
		Type:        types.StringValue("PRIMARY"),
		Email:       types.StringValue("dns@example.com"),
		TTL:         types.Int64Unknown(),
		Description: types.StringUnknown(),
		Masters:     types.ListUnknown(types.StringType),
		Attributes:  types.MapUnknown(types.StringType),
		Serial:      types.Int64Unknown(),
		Status:      types.StringUnknown(),
		PoolID:      types.StringUnknown(),
		ProjectID:   types.StringUnknown(),
		Region:      types.StringUnknown(),
	}
	plan := tfsdk.Plan{Schema: s, Raw: tftypes.NewValue(s.Type().TerraformType(ctx), nil)}
	if d := plan.Set(ctx, &planned); d.HasError() {
		t.Fatalf("building the plan: %v", d)
	}

	createResp := resource.CreateResponse{State: tfsdk.State{Schema: s, Raw: tftypes.NewValue(s.Type().TerraformType(ctx), nil)}}
	r.Create(ctx, resource.CreateRequest{Plan: plan}, &createResp)
	if !createResp.Diagnostics.HasError() {
		t.Fatal("create succeeded; want the ERROR status reported")
	}
	if createResp.State.Raw.IsNull() {
		t.Fatal("create returned no state: Terraform forgets zone-1 and the next apply creates a second zone")
	}
	if !createResp.State.Raw.IsFullyKnown() {
		t.Fatalf("create state holds unknown values, which Terraform refuses: %v", createResp.State.Raw)
	}
	var got zoneModel
	if d := createResp.State.Get(ctx, &got); d.HasError() {
		t.Fatalf("reading the create state: %v", d)
	}
	if got.ID.ValueString() != "zone-1" || got.Name.ValueString() != "tf-acc-failed.example.com." {
		t.Fatalf("create state id=%s name=%s; want zone-1, tf-acc-failed.example.com.", got.ID, got.Name)
	}
	if !got.Status.IsNull() {
		t.Fatalf("create state status = %s; want null, since Designate has not reported it yet", got.Status)
	}

	// terraform destroy (or the replacing apply) refreshes the tainted zone first.
	readResp := resource.ReadResponse{State: createResp.State}
	r.Read(ctx, resource.ReadRequest{State: createResp.State}, &readResp)
	if readResp.Diagnostics.HasError() {
		t.Fatalf("refresh of the failed zone: %v", readResp.Diagnostics)
	}
	if d := readResp.State.Get(ctx, &got); d.HasError() {
		t.Fatalf("reading the refreshed state: %v", d)
	}
	if got.Status.ValueString() != "ERROR" {
		t.Fatalf("refreshed status = %s, want ERROR", got.Status)
	}
	if got.Region.ValueString() != "region-one" {
		t.Fatalf("refreshed region = %s, want region-one backfilled from the provider config", got.Region)
	}

	deleteResp := resource.DeleteResponse{State: readResp.State}
	r.Delete(ctx, resource.DeleteRequest{State: readResp.State}, &deleteResp)
	if deleteResp.Diagnostics.HasError() {
		t.Fatalf("delete of a zone in ERROR: %v", deleteResp.Diagnostics)
	}
	mu.Lock()
	defer mu.Unlock()
	if !deleteCalled {
		t.Fatal("delete never called DELETE /v2/zones/zone-1")
	}
	if getsAfterDelete < 2 {
		t.Fatalf("delete returned after %d polls; want it to poll through the ERROR status until Designate answers 404", getsAfterDelete)
	}
}
```

- [ ] **Step 2: Run it and watch it fail**

Run the focused test for this resource. It must fail because `Create` returns no state — not for a compile error or a fake-server mismatch. If it fails for another reason, fix the test before touching the resource.

- [ ] **Step 3: Write the implementation**

#### Edit 1 — `internal/services/dns/dns.go` lines 117–138: make the delete waiter transition-aware

Designate reports `ERROR` on the zone that the create wait abandoned, and it can still report it on the very first poll after the `DELETE` is accepted. `gophercloud.WaitFor` runs its predicate with zero delay, so today that first poll fires the bail on essentially **every** destroy of an abandoned zone — this is the expected path, not an unlucky race. The fix keeps the fast-fail that matters (a healthy zone that goes into `ERROR` *while being deleted*) by only treating `ERROR` as a failure once the zone has been seen leaving it.

**Before:**

```go
// waitForZoneDeleted blocks until the zone is gone (404).
func waitForZoneDeleted(ctx context.Context, client *gophercloud.ServiceClient, zoneID string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	err := gophercloud.WaitFor(ctx, func(ctx context.Context) (bool, error) {
		z, err := zones.Get(ctx, client, zoneID).Extract()
		if err != nil {
			if gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
				return true, nil
			}
			return false, err
		}
		if z.Status == dnsError {
			return false, fmt.Errorf("zone %s entered ERROR status during delete", zoneID)
		}
		return false, nil
	})
	if err != nil {
		return fmt.Errorf("waiting for zone %s to delete: %w", zoneID, err)
	}
	return nil
}
```

**After:**

```go
// waitForZoneDeleted blocks until the zone is gone (404).
//
// A zone a failed create wait abandoned in ERROR is still deletable, and
// Designate can still report ERROR on the first poll after the DELETE is
// accepted, so ERROR counts as a delete failure only once the zone has been
// seen leaving it. gophercloud.WaitFor calls the predicate serially, so the
// captured flag needs no synchronization.
func waitForZoneDeleted(ctx context.Context, client *gophercloud.ServiceClient, zoneID string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	seenNonError, lastStatus := false, ""
	err := gophercloud.WaitFor(ctx, func(ctx context.Context) (bool, error) {
		z, err := zones.Get(ctx, client, zoneID).Extract()
		if err != nil {
			if gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
				return true, nil
			}
			return false, err
		}
		lastStatus = z.Status
		if z.Status != dnsError {
			seenNonError = true
			return false, nil
		}
		if seenNonError {
			return false, fmt.Errorf("zone %s entered ERROR status during delete", zoneID)
		}
		return false, nil
	})
	if err != nil {
		if lastStatus != "" {
			return fmt.Errorf("waiting for zone %s to delete (last status %q): %w", zoneID, lastStatus, err)
		}
		return fmt.Errorf("waiting for zone %s to delete: %w", zoneID, err)
	}
	return nil
}
```

Note the two-branch wrap: when no status was ever observed (a transport error on the first poll), the message keeps its existing shape rather than printing `(last status "")`.

**What the operator gets, traced, in each branch:**

| Branch | Before | After |
|---|---|---|
| Healthy zone; the delete genuinely fails and Designate sets ERROR | first poll sees `PENDING`, the later `ERROR` fails fast with `zone <id> entered ERROR status during delete` | identical — `seenNonError` is already true. **Preserved.** |
| Zone abandoned in ERROR; DELETE accepted; first poll still ERROR, then PENDING, then 404 | destroy fails on the first poll although the DELETE succeeded; a second `terraform destroy` 404s and succeeds | succeeds on the first attempt. **Fixed.** This is the common case. |
| Zone abandoned in ERROR and the backend delete also fails, so it never leaves ERROR | every attempt fails instantly on its first poll; the row can only leave state via `terraform state rm` | polls to the 10-minute `defaultDNSTimeout` and fails with `waiting for zone <id> to delete (last status "ERROR"): context deadline exceeded`. The destroy still fails — correctly, the zone is still there — but the status alone no longer wedges it permanently, and the message names why. |

The third row is a deliberate trade: slower to report, but debuggable and not status-wedged. Do **not** describe this as "the destroy can never be permanently blocked" — a zone the backend refuses to delete still blocks the destroy, as it should.

Simply deleting the three-line check (the survey's "Option A") is strictly worse: it would turn the first row — the one genuine signal — into a 10-minute timeout too.

#### Edit 2 — `internal/services/dns/zone_resource.go` line 28: add the import

**Before:**

```go
	"github.com/platform9/terraform-provider-pcd/internal/clients"
)
```

**After:**

```go
	"github.com/platform9/terraform-provider-pcd/internal/clients"
	"github.com/platform9/terraform-provider-pcd/internal/tfstate"
)
```

#### Edit 3 — `internal/services/dns/zone_resource.go` lines 123–135: record before the wait, and guard the tail

Two changes in one hunk. The record is the fix. The tail guard is what stops the fix from being undone: today `notFound` is discarded with `_` and the final `resp.State.Set` is unconditional, so when `readInto` 404s or errors it returns **without assigning anything to `plan`** — leaving `plan` carrying the create plan's unknowns for `id`, `ttl`, `serial`, `status`, `pool_id`, `project_id`, `region`, `description`, `masters`, `attributes` — and that `Set` would overwrite the clean, unknown-free row with a state full of unknowns, which Terraform core rejects outright. The hazard is latent today only because there is no recorded row to clobber.

**Before** (lines 123–135):

```go
	zone, err := zones.Create(ctx, client, createOpts).Extract()
	if err != nil {
		resp.Diagnostics.AddError("dns: creating zone", err.Error())
		return
	}
	if err := waitForZoneActive(ctx, client, zone.ID, defaultDNSTimeout); err != nil {
		resp.Diagnostics.AddError("dns: waiting for zone to become active", err.Error())
		return
	}

	_, readDiags := r.readInto(ctx, client, zone.ID, &plan)
	resp.Diagnostics.Append(readDiags...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}
```

**After** (lines 124–153):

```go
	zone, err := zones.Create(ctx, client, createOpts).Extract()
	if err != nil {
		resp.Diagnostics.AddError("dns: creating zone", err.Error())
		return
	}
	plan.ID = types.StringValue(zone.ID)
	// Designate keeps a zone whose build fails (status ERROR), so record it
	// before the wait, which is the step that gives up on it.
	if !tfstate.RecordCreated(ctx, resp, &plan) {
		return
	}

	if err := waitForZoneActive(ctx, client, zone.ID, defaultDNSTimeout); err != nil {
		resp.Diagnostics.AddError("dns: waiting for zone to become active", err.Error())
		return
	}

	notFound, readDiags := r.readInto(ctx, client, zone.ID, &plan)
	resp.Diagnostics.Append(readDiags...)
	if notFound {
		resp.Diagnostics.AddError("dns: reading zone after create",
			fmt.Sprintf("Zone %s no longer exists.", zone.ID))
		resp.State.RemoveResource(ctx)
		return
	}
	if resp.Diagnostics.HasError() {
		return // leave the row RecordCreated wrote in place
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}
```

The two tail branches are deliberately opposite, and this matches `Read`'s own 404 branch in the same file (`zone_resource.go:152-157`, which warns and calls `RemoveResource`) and the shipped `deleteCreatedImage` precedent:

- **`notFound`** — the zone is *proven gone*. Keeping the recorded row would leave state asserting an object that does not exist, so remove it.
- **`readInto` errored** — the zone *exists but could not be read*. Return early and leave the recorded row so Terraform taints it and the next apply or destroy deals with it.

**Explicitly rejected shortcut:** do **not** re-run `tfstate.NullUnknowns` after the final `State.Set` as a cheaper substitute for the `notFound` guard. It would make the `notFound` case "succeed" by writing a row whose every computed attribute is null — a state row for a zone that is gone. This was considered and rejected.

**Why `NullUnknowns` must stay on `resp.State` and never on the `plan` struct:** `plan.Region` stays *unknown in memory* after `RecordCreated`, so `readInto`'s `if m.Region.IsNull() || m.Region.IsUnknown()` backfill at `zone_resource.go:274-276` still fires on the success path. Nulling the in-memory plan would not change that one, but the same `IsNull() || IsUnknown()` shape governs sibling resources, so the rule is uniform.

**Do not "clean up" the `_, readDiags :=` in `Update` at `zone_resource.go:202-204`** as part of this commit — see open questions.

#### Edit 4 — design doc

Delete line 185 of `docs/superpowers/specs/2026-09-19-create-state-before-wait-design.md`:

```
- `pcd_dns_zone` — `internal/services/dns/zone_resource.go:128`
```

and add `pcd_dns_zone` to the **Scope** section, with a sentence recording that its delete waiter was relaxed along with it. Decrement the counts at lines 14 and 181.

#### Edit 5 — CHANGELOG.md, one `### Fixed` bullet under `## [Unreleased]`

```markdown
- `pcd_dns_zone`: a zone whose create wait gives up now stays in state. Designate keeps a zone
  whose build fails (status `ERROR`), and it keeps one abandoned in `PENDING` by a ten-minute
  timeout or an interrupted apply. The apply still fails with the Designate status, and Terraform
  marks the zone tainted, so the next apply deletes and recreates it and a destroy deletes it.
  Before, the failed apply left no state: Terraform lost track of a zone Designate kept, the next
  apply created a second one with the same name, and the first had to be deleted through the API.
  The attributes Designate had not reported when the build failed are saved empty until the next
  refresh. Destroying such a zone also used to fail: the delete wait treated `ERROR` as a delete
  failure on its very first poll, even though the `DELETE` had already been accepted, so the
  destroy reported an error and kept the zone in state. It now polls through an `ERROR` the delete
  inherited and reports one the zone enters while being deleted, and a delete that times out names
  the last status it saw.
```

American English throughout; no `pcd_loadbalancer_*` names appear in this bullet.

- [ ] **Step 4: Run the test and watch it pass**

Run the focused test again. Then the full suite.

- [ ] **Step 5: Verify**

```bash
# All of the following were run against an isolated copy of this worktree with the edits applied; each one passed.
```
```bash
cd "/Users/pushkarmulay/Documents/Claude Code Projects/Terraform Provider/terraform-provider-pcd/.claude/worktrees/inspiring-kilby-0bbd0b"
```
```bash
gofmt -l internal/services/dns internal/tfstate   # must print nothing
```
```bash
go build ./...
```
```bash
go vet ./...
```
```bash
golangci-lint run ./internal/services/dns/... ./internal/tfstate/...   # expect '0 issues.'
```
```bash
go test ./internal/services/dns/ -run TestZoneCreateKeepsAZoneThatFailedToBuild -count=1 -v   # PASS, ~1.0s (one 1s WaitFor tick)
```
```bash
make test   # go test ./internal/... -timeout 120s; whole tree must stay green
```
```bash
# Negative control 1 - prove the test covers the waiter change. Temporarily restore the old bail in internal/services/dns/dns.go (replace the seenNonError branch with 'if z.Status == dnsError { return false, fmt.Errorf(...) }') and rerun:
```
```bash
go test ./internal/services/dns/ -run TestZoneCreateKeepsAZoneThatFailedToBuild -count=1   # must FAIL with: delete of a zone in ERROR: [... zone zone-1 entered ERROR status during delete ...]. Then revert.
```
```bash
# Negative control 2 - prove the test covers the record. Temporarily remove the plan.ID + tfstate.RecordCreated block from zone_resource.go and rerun:
```
```bash
go test ./internal/services/dns/ -run TestZoneCreateKeepsAZoneThatFailedToBuild -count=1   # must FAIL with: create returned no state: Terraform forgets zone-1 .... Then revert.
```
```bash
# Happy path is unchanged (checked with a throwaway fake serving status ACTIVE): Create succeeds, state is fully known, status=ACTIVE, ttl=3600, region backfilled to region-one. Not shipped as a test; the acceptance test covers it.
```
```bash
TF_ACC=1 go test ./internal/services/dns/ -run TestAccDNSZoneAndRecordSet_basic -v -timeout 30m   # requires the CE lab OS_* env; confirms the happy path end to end
```
```bash
make generate && git diff --exit-code docs/   # no schema change, so the registry docs must not move
```
```bash
# CE lab verification (see open questions 1 and 5):
```
```bash
#  a) Create a zone Designate will fail (e.g. a name the pool rejects), confirm the apply fails, 'terraform state list' still shows the zone, and 'terraform destroy' removes it in one attempt.
```
```bash
#  b) openstack zone delete <id> on a zone left in ERROR, to confirm Designate accepts DELETE in that status.
```
```bash
#  c) Create a pcd_blockstorage_volume, force its create to fail, then re-apply the tainted resource and confirm no 'Provider produced inconsistent result after apply' error (UseStateForUnknown replan check; validates the already-shipped five as well).
```

- [ ] **Step 6: Commit**

```
fix(dns): keep a zone in state when its create wait gives up

Designate keeps a zone whose build fails, and it keeps one abandoned in
PENDING by a ten-minute timeout or an interrupted apply. Create returned
the wait's error without saving state, so Terraform lost track of a zone
that exists: the next apply created a second one with the same name and
the first had to be deleted through the API by hand.

Create now records the zone before it waits, so the failed apply returns
its error with the zone in state. Terraform marks the resource tainted,
the next apply deletes and recreates it, and a destroy deletes it. The
attributes Designate has not reported yet are saved as null, since
Terraform refuses unknown values in state, and the next refresh reads
them.

Recording state alone would have made the destroy worse, so the delete
path is fixed in the same commit. waitForZoneDeleted treated ERROR as a
delete failure on its very first poll, and gophercloud.WaitFor runs its
predicate with no delay, so a zone the create wait abandoned in ERROR
reported a failed destroy even though the DELETE had already been
accepted — and a zone the backend refused to delete could never leave
state except with terraform state rm. ERROR now counts as a delete
failure only once the zone has been seen leaving it, which keeps the
fast-fail for a healthy zone that breaks while being deleted and drops
it for one that was already broken. A delete that times out names the
last status it saw. A zone the backend genuinely will not delete still
fails the destroy, after the existing ten-minute timeout rather than
instantly; that trade is deliberate.

Create's tail is guarded at the same time. It discarded readInto's
notFound flag and set state unconditionally, and readInto returns
without touching the model on a 404 or an error, so that final set would
have overwritten the recorded row with the create plan's unknown values.
A 404 now drops the row, since the zone is proven gone, and a read error
returns with the recorded row in place, since the zone still exists.

waitForRecordSetDeleted is deliberately left alone: it inspects no
status, which is already the right shape for a delete waiter whose
service has no delete-only failure status. The two DNS delete waiters
are not a matched pair, and making them "consistent" by adding a check
to the recordset would reintroduce the bug this commit removes.
```

**Open questions and traps recorded during planning:**

- UNVERIFIED SERVER BEHAVIOR, and the one thing the lab must settle: that Designate accepts a DELETE on a zone in ERROR status. Nothing in this repository or in gophercloud v2.13.0 proves it; the survey asserted it as fact ('the normal CLI recovery path') without evidence. If it is false, the failure moves to zone_resource.go:221 (`zones.Delete`) and no waiter change helps — the user-visible outcome would then be the same failed destroy as today, with a better message and the zone preserved in state, so the change is still not a regression, but the claim must not be repeated as fact in the changelog. Check with: create a zone, drive it to ERROR, then `openstack zone delete <id>`.
- RELATED UNVERIFIED PREMISE: that a zone whose backend delete fails is returned to ERROR with action DELETE. The third row of the Edit 1 table depends on it. If Designate instead leaves it in PENDING, that row simply becomes the second row (succeeds), which is better, not worse. Nothing needs to change either way; it is stated here so a reviewer does not read the table as measured behavior.
- THE SURVEY'S 'OPTION A' BENEFIT IS WRONG AND MUST NOT BE COPIED INTO THE CHANGELOG. Removing the ERROR check outright does NOT make the destroy unblockable: the waiter would poll to the 10-minute defaultDNSTimeout (dns.go:75, ctx at :119-120), WaitFor returns ctx.Err(), and zone_resource.go:229 still AddErrors. The honest claim, which the commit message and changelog bullet above both make, is narrower: the delete waiter now polls through an ERROR the delete inherited, and a zone the backend will not delete still fails the destroy, just after a timeout instead of instantly.
- OUT OF SCOPE BUT NOTICED, AND A REAL BUG: Update has the same discarded-notFound plus unconditional State.Set shape at zone_resource.go:202-204. Unlike Create, this one is live today with no record needed — if readInto errors during Update, `plan` still carries the update plan's unknowns for the computed attributes and that Set writes them to state, which Terraform core rejects. The settled design scopes this change to the eight Creates, so I have NOT included it. It deserves its own commit; the same shape is worth grepping for in the other seven resources' Update methods while their sections are being written.
- THE UseStateForUnknown REPLAN CHECK APPLIES HERE, and the zone schema is one of the heavier users: `useState` is attached to id, email, description, status, pool_id, project_id and region, plus the Int64/List/Map equivalents on ttl, masters, attributes and serial (zone_resource.go:66-88). A tainted zone's null computed attributes could in principle be copied into the replacement plan. This is NOT catchable by the unit test above, which calls Create/Read/Delete directly and never goes through Terraform core's plan. Per the settled design it is covered by one CE lab run against the already-shipped pcd_blockstorage_volume, which carries identical exposure; no extra work is needed in this section beyond running it.
- MINOR DEVIATION FROM THE SETTLED DESIGN'S lastStatus GRAFT: I wrapped it in a two-branch conditional so that a transport error on the first poll (where no status was ever read) keeps the original message shape instead of printing `(last status "")`. One extra `if`; flagging it only so a reviewer comparing against the design text is not surprised.
- NAMING COLLISION TO WATCH: the settled design has the recordset section land first and create internal/services/dns/failed_create_internal_test.go with fakeConfig in it. If both sections are executed from these drafts verbatim, `fakeConfig` is declared twice and the package will not build. Whoever lands second must drop their copy. There is no collision with committed code — package dns has no fakeConfig today.
- NOT APPLICABLE TO THIS RESOURCE: the settled design's load-balancer material — waitForLoadBalancerSettled, settleBeforeDelete, the rootLBIDFromPool 404 short-circuits, the lbPending constant, and the loadbalancer.go package-doc correction. internal/services/dns imports nothing from loadbalancer and zone Delete (zone_resource.go:207-231) waits on no parent object. I did not invent an analogue.

---
### Task 2: `pcd_keymanager_secret`

Depends on Task 0 only; runs in parallel with Tasks 1 and 4. The cleanest of the eight: no delete waiter exists in this package, and `Delete` issues `secrets.Delete` immediately. The record goes **before** the `if payloadSet {` block, not inside it.

**Files:**
All paths are relative to the worktree root `/Users/pushkarmulay/Documents/Claude Code Projects/Terraform Provider/terraform-provider-pcd/.claude/worktrees/inspiring-kilby-0bbd0b/`.

**Depends on (do not start until it is merged):** Task 0, which must have published

```go
func RecordCreated(ctx context.Context, resp *resource.CreateResponse, plan any) bool
```

in `internal/tfstate` (today the equivalent is `recordCreated`, unexported, at `internal/services/blockstorage/blockstorage.go:44`, which package `keymanager` cannot import).

**Task 2 is otherwise independent.** It shares no file with Tasks 1, 3, 4 or 5 and can run in parallel with Task 1 and Task 4.

#### Modify

- `internal/services/keymanager/secret_resource.go`
  - **line 28** (inside the import block, the line `"github.com/platform9/terraform-provider-pcd/internal/clients"`): add the `internal/tfstate` import directly beneath it.
  - **lines 152–165** (the body of `Create` from `id := refToID(secret.SecretRef)` through the final `resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)`): insert the record between the create call and the conditional wait, and guard the read-back tail. After the edit this span is lines 153–182 and the file grows by 17 lines (271 → 288 wait, exactly: the wait call moves from line 157 to line 165, and the closing brace of `Create` moves from 166 to 183).
  - Nothing else in the file changes. `Read` (168–193), `Update` (196–200), `Delete` (202–221), `ImportState` (223–225), `readInto` (231–279), `unset` (283–285) and `formatTime` (288–293) are untouched.

#### Do NOT modify

- `internal/services/keymanager/keymanager.go` — **no waiter change for this resource.** `grep -n "func waitFor" internal/services/keymanager/` returns exactly one hit, `keymanager.go:62` `waitForSecretActive`, which is a *create* waiter. `Delete` at `secret_resource.go:215` issues `secrets.Delete` immediately with no pre-wait and no post-wait and swallows a 404 at `:216-218`, and `readInto` at `:232-243` copies `secret.Status` verbatim without inspecting it. Nothing in this package's destroy path can refuse a secret in `ERROR` or `PENDING`, so none of the settled design's delete-waiter work (`waitForLoadBalancerSettled`, `settleBeforeDelete`, the transition-aware `waitForZoneDeleted`/`waitForLoadBalancerDeleted` latch) applies here. This is the cleanest of the eight.
- `internal/services/keymanager/container_resource.go` — `pcd_keymanager_container` has no post-create wait, so it is correctly not one of the eight.
- `internal/services/keymanager/keymanager_test.go` — `package keymanager_test` (external), TF_ACC-gated, unchanged. Its external package means the new `fakeConfig` in `package keymanager` cannot collide with anything in it.

#### Test (new file)

- `internal/services/keymanager/secret_failed_create_internal_test.go` — **new**, `package keymanager` (internal test, so it can reach `secretResource`, `secretModel` and `refToID`). Naming follows the convention for a package contributing one resource: `<resource>_failed_create_internal_test.go`, matching `internal/services/images/image_failed_create_internal_test.go` and `internal/services/compute/instance_failed_create_internal_test.go`. This is the keymanager package's **first unit test** — there are zero today — so it carries its own `fakeConfig`, as `blockstorage` (`failed_create_internal_test.go:26`) and `images` (`image_failed_create_internal_test.go:29`) each do. That duplication is the established pattern, not a smell: `grep -rn 'func fakeConfig' internal/` finds only those two, both unexported and package-local.
- **No cancel-during-wait test here.** `TestVolumeCreateKeepsAVolumeWhenTheWaitIsCanceled` (`internal/services/blockstorage/failed_create_internal_test.go:153`) pins the shared record-before-wait mechanism once; the comment at `failed_create_internal_test.go:149-152` says so, and Task 0's promote commit updates it to name `tfstate.RecordCreated`, which makes that sentence true provider-wide.

#### Docs (belongs to the changelog/doc task, listed here so it is not lost)

- `docs/superpowers/specs/2026-09-19-create-state-before-wait-design.md:197` lists `pcd_keymanager_secret — internal/services/keymanager/secret_resource.go:157` under "Other resources with the same gap". That citation is accurate **today**; after this edit the wait sits at line 165. **Delete the entry** rather than renumbering it, and move `pcd_keymanager_secret` into the doc's Scope section.
- `CHANGELOG.md` — one `### Fixed` bullet under `## [Unreleased]`, in the existing style, American English, no ticket reference. Draft:

  > - `pcd_keymanager_secret`: a secret Barbican accepts but never brings to `ACTIVE` now stays in state, and so does one whose apply is interrupted while the provider waits. The apply still fails with Barbican's error and Terraform marks the secret tainted, so the next apply or a destroy deletes it. Before, the failed apply left no state: Terraform lost track of a secret Barbican kept, the next apply created a second one, and the first had to be deleted through the API. A secret created without a payload is not waited on at all, but it is now recorded as soon as Barbican returns its reference, so an apply that fails between the create and the read-back no longer loses it either. The attributes Barbican had not reported are saved empty until the next refresh.

**Interfaces:**
#### Consumes

| Symbol | Signature | Where |
|---|---|---|
| `tfstate.RecordCreated` | `func RecordCreated(ctx context.Context, resp *resource.CreateResponse, plan any) bool` | `internal/tfstate` — **produced by Task 0**; hard dependency |
| `(*secretResource).readInto` | `func (r *secretResource) readInto(ctx context.Context, client *gophercloud.ServiceClient, id string, m *secretModel) (notFound bool, diags diag.Diagnostics)` | `internal/services/keymanager/secret_resource.go:231` — unchanged; the edit only stops discarding its first return |
| `refToID` | `func refToID(ref string) string` | `internal/services/keymanager/keymanager.go:54` — `path.Base(strings.TrimRight(ref, "/"))`; there is no `secret.ID` field, only `SecretRef` |
| `waitForSecretActive` | `func waitForSecretActive(ctx context.Context, client *gophercloud.ServiceClient, uuid string, timeout time.Duration) error` | `internal/services/keymanager/keymanager.go:62` — unchanged |
| `defaultKeyManagerTimeout` | `const defaultKeyManagerTimeout = 5 * time.Minute` | `internal/services/keymanager/keymanager.go:34` |
| `(*clients.Config).KeyManagerV1Client` | `func (c *Config) KeyManagerV1Client() (*gophercloud.ServiceClient, error)` | `internal/clients/config.go:348` — calls `openstack.NewKeyManagerV1`, which sets `ResourceBase = Endpoint + "v1/"`; `applyOverride` at `:359-364` blanks `ResourceBase`, which is why the test must not use `EndpointOverrides` |
| `secrets.Create` / `secrets.Get` / `secrets.Delete` | gophercloud v2.13.0 `openstack/keymanager/v1/secrets` | Create OkCodes `[201]` only; Get `[200]`; Delete `[202, 204]` |
| `types.StringValue` | `func StringValue(value string) String` | already imported at `secret_resource.go:26` |

#### Produces

- **No new exported symbols.** `(*secretResource).Create` keeps its signature `func (r *secretResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse)`; only its behavior changes.
- **Test-only, package-local:** `func fakeConfig(url string) *clients.Config` in `package keymanager`. It cannot collide with anything existing — `internal/services/keymanager/keymanager_test.go` is `package keymanager_test` (external), and `grep -rn 'func fakeConfig' internal/` finds only the `blockstorage` and `images` copies, each in its own package.
- **Changed state contract:** after this change, a failed `pcd_keymanager_secret` create leaves a state row carrying `id`, `secret_ref`, and the configured `name` / `secret_type` / `payload` / `payload_content_type`, with every attribute Barbican has not reported yet set to **null** (not unknown). `region` is null in that row — it is filled only inside `readInto` at `:275-277`, which a failed wait never reaches — so nothing downstream may assume it is populated before the first refresh.

- [ ] **Step 1: Write the failing test**

Add to the test file named above:

```go
// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package keymanager

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

// fakeConfig points a clients.Config at a test server, so the Barbican client
// the resource builds resolves to it. The locator ignores the endpoint options,
// so it answers for every service type and availability. NewKeyManagerV1 appends
// "v1/" to the endpoint, so the fake serves /v1/secrets, not /secrets. An
// EndpointOverrides entry would blank that prefix (internal/clients/config.go
// applyOverride), so the locator is the only wiring this test uses.
func fakeConfig(url string) *clients.Config {
	return &clients.Config{
		Region: "region-one",
		Provider: &gophercloud.ProviderClient{
			EndpointLocator: func(gophercloud.EndpointOpts) (string, error) { return url + "/", nil },
		},
	}
}

// A secret whose payload store ends in ERROR stays in Barbican. Create used to
// return without state, so Terraform forgot the secret, the next apply created
// another, and the first had to be deleted through the API by hand. Create must
// return the error with the secret in state (Terraform then taints it), and the
// refresh and delete a destroy runs must remove the secret even though Barbican
// reports ERROR.
func TestSecretCreateKeepsASecretThatFailedToBecomeActive(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	var mu sync.Mutex
	deleteCalled, getsAfterDelete := false, 0
	// Barbican's timestamps parse only as gophercloud.RFC3339NoZ ("2006-01-02T15:04:05"):
	// a trailing "Z" or an offset makes secrets.Get(...).Extract() fail with a
	// parse error that looks like a provider bug.
	barbican := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch r.Method + " " + r.URL.Path {
		case "POST /v1/secrets":
			// secrets.Create accepts 201 only; 202 is rejected.
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{"secret_ref": "http://barbican.invalid/v1/secrets/sec-1"}`)
		case "GET /v1/secrets/sec-1":
			if deleteCalled {
				getsAfterDelete++
				w.WriteHeader(http.StatusNotFound)
				return
			}
			fmt.Fprint(w, `{"secret_ref": "http://barbican.invalid/v1/secrets/sec-1",
				"name": "tf-acc-secret", "status": "ERROR", "secret_type": "passphrase",
				"algorithm": "", "bit_length": 0, "mode": "", "creator_id": "user-1",
				"content_types": {"default": "text/plain"},
				"created": "2026-09-19T12:00:00", "updated": "2026-09-19T12:00:01"}`)
		case "DELETE /v1/secrets/sec-1":
			// secrets.Delete accepts 202 and 204 only; 200 is rejected.
			deleteCalled = true
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotImplemented)
		}
	}))
	defer barbican.Close()

	r := &secretResource{config: fakeConfig(barbican.URL)}
	var sch resource.SchemaResponse
	r.Schema(ctx, resource.SchemaRequest{}, &sch)
	s := sch.Schema

	// payload and payload_content_type are both required for this test to mean
	// anything: without payload Create skips waitForSecretActive entirely, and
	// with payload but no payload_content_type Create fails before any HTTP call.
	planned := secretModel{
		ID:                     types.StringUnknown(),
		Name:                   types.StringValue("tf-acc-secret"),
		Algorithm:              types.StringUnknown(),
		BitLength:              types.Int64Unknown(),
		Mode:                   types.StringUnknown(),
		SecretType:             types.StringValue("passphrase"),
		Expiration:             types.StringUnknown(),
		Payload:                types.StringValue("s3cr3t-passphrase"),
		PayloadContentType:     types.StringValue("text/plain"),
		PayloadContentEncoding: types.StringNull(),
		SecretRef:              types.StringUnknown(),
		Status:                 types.StringUnknown(),
		CreatorID:              types.StringUnknown(),
		ContentTypes:           types.MapUnknown(types.StringType),
		CreatedAt:              types.StringUnknown(),
		UpdatedAt:              types.StringUnknown(),
		Region:                 types.StringUnknown(),
	}
	plan := tfsdk.Plan{Schema: s, Raw: tftypes.NewValue(s.Type().TerraformType(ctx), nil)}
	if d := plan.Set(ctx, &planned); d.HasError() {
		t.Fatalf("building the plan: %v", d)
	}

	createResp := resource.CreateResponse{State: tfsdk.State{Schema: s, Raw: tftypes.NewValue(s.Type().TerraformType(ctx), nil)}}
	r.Create(ctx, resource.CreateRequest{Plan: plan}, &createResp)
	if !createResp.Diagnostics.HasError() {
		t.Fatal("create succeeded; want the ERROR status reported")
	}
	if createResp.State.Raw.IsNull() {
		t.Fatal("create returned no state: Terraform forgets sec-1 and the next apply creates a second secret")
	}
	if !createResp.State.Raw.IsFullyKnown() {
		t.Fatalf("create state holds unknown values, which Terraform refuses: %v", createResp.State.Raw)
	}
	var got secretModel
	if d := createResp.State.Get(ctx, &got); d.HasError() {
		t.Fatalf("reading the create state: %v", d)
	}
	if got.ID.ValueString() != "sec-1" {
		t.Fatalf("create state id = %s, want sec-1: Delete feeds this straight into the URL", got.ID)
	}
	if got.SecretRef.ValueString() != "http://barbican.invalid/v1/secrets/sec-1" {
		t.Fatalf("create state secret_ref = %s, want the ref Barbican returned", got.SecretRef)
	}
	if got.Name.ValueString() != "tf-acc-secret" || got.SecretType.ValueString() != "passphrase" {
		t.Fatalf("create state name=%s secret_type=%s; want tf-acc-secret, passphrase", got.Name, got.SecretType)
	}

	// terraform destroy (or the replacing apply) refreshes the tainted secret first.
	readResp := resource.ReadResponse{State: createResp.State}
	r.Read(ctx, resource.ReadRequest{State: createResp.State}, &readResp)
	if readResp.Diagnostics.HasError() {
		t.Fatalf("refresh of the failed secret: %v", readResp.Diagnostics)
	}
	if readResp.State.Raw.IsNull() {
		t.Fatal("refresh dropped the secret from state; Barbican still reports it")
	}
	if d := readResp.State.Get(ctx, &got); d.HasError() {
		t.Fatalf("reading the refreshed state: %v", d)
	}
	if got.Status.ValueString() != "ERROR" {
		t.Fatalf("refreshed status = %s, want ERROR", got.Status)
	}
	if got.CreatorID.ValueString() != "user-1" || got.CreatedAt.ValueString() == "" {
		t.Fatalf("refreshed creator_id=%s created_at=%s; want the server values", got.CreatorID, got.CreatedAt)
	}

	deleteResp := resource.DeleteResponse{State: readResp.State}
	r.Delete(ctx, resource.DeleteRequest{State: readResp.State}, &deleteResp)
	if deleteResp.Diagnostics.HasError() {
		t.Fatalf("delete of a secret in ERROR: %v", deleteResp.Diagnostics)
	}
	mu.Lock()
	defer mu.Unlock()
	if !deleteCalled {
		t.Fatal("delete never called DELETE /v1/secrets/sec-1")
	}
	if getsAfterDelete != 0 {
		t.Fatalf("delete polled the secret %d times; keymanager has no delete waiter", getsAfterDelete)
	}
}
```

- [ ] **Step 2: Run it and watch it fail**

Run the focused test for this resource. It must fail because `Create` returns no state — not for a compile error or a fake-server mismatch. If it fails for another reason, fix the test before touching the resource.

- [ ] **Step 3: Write the implementation**

Two edits, both in `internal/services/keymanager/secret_resource.go`. I applied exactly these to a scratch copy of the worktree and ran the suite; the numbers below are the real ones.

---

#### Edit 1 — the import block (lines 26–28)

**Before:**

```go
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/platform9/terraform-provider-pcd/internal/clients"
)
```

**After:**

```go
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/platform9/terraform-provider-pcd/internal/clients"
	"github.com/platform9/terraform-provider-pcd/internal/tfstate"
)
```

`types`, `fmt` and `http` are already imported; nothing else is added or removed.

---

#### Edit 2 — `Create`, lines 147–166

**Before** (verbatim `secret_resource.go:147-166`):

```go
	secret, err := secrets.Create(ctx, client, createOpts).Extract()
	if err != nil {
		resp.Diagnostics.AddError("keymanager: creating secret", err.Error())
		return
	}
	id := refToID(secret.SecretRef)

	// A secret created with a payload is briefly PENDING; wait for ACTIVE. A
	// secret created without a payload stays PENDING, so do not wait in that case.
	if payloadSet {
		if err := waitForSecretActive(ctx, client, id, defaultKeyManagerTimeout); err != nil {
			resp.Diagnostics.AddError("keymanager: waiting for secret to become active", err.Error())
			return
		}
	}

	_, readDiags := r.readInto(ctx, client, id, &plan)
	resp.Diagnostics.Append(readDiags...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}
```

**After** (lines 148–183 once applied):

```go
	secret, err := secrets.Create(ctx, client, createOpts).Extract()
	if err != nil {
		resp.Diagnostics.AddError("keymanager: creating secret", err.Error())
		return
	}
	id := refToID(secret.SecretRef)
	plan.ID = types.StringValue(id)
	plan.SecretRef = types.StringValue(secret.SecretRef)
	// Barbican holds the secret from here on, whether or not a payload was sent,
	// so record it before anything else can fail.
	if !tfstate.RecordCreated(ctx, resp, &plan) {
		return
	}

	// A secret created with a payload is briefly PENDING; wait for ACTIVE. A
	// secret created without a payload stays PENDING, so do not wait in that case.
	if payloadSet {
		if err := waitForSecretActive(ctx, client, id, defaultKeyManagerTimeout); err != nil {
			resp.Diagnostics.AddError("keymanager: waiting for secret to become active", err.Error())
			return
		}
	}

	notFound, readDiags := r.readInto(ctx, client, id, &plan)
	resp.Diagnostics.Append(readDiags...)
	if notFound {
		resp.Diagnostics.AddError("keymanager: reading secret after create",
			fmt.Sprintf("Secret %s no longer exists.", id))
		resp.State.RemoveResource(ctx)
		return
	}
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}
```

---

#### Why each line is there

1. **The record goes before `if payloadSet {`, not inside it.** Barbican holds the secret from the moment `secrets.Create` returns at line 147. With no payload there is no wait at all, but `Create` still runs `readInto` and an apply interrupted between the create and the read-back ends with the secret in Barbican and nothing in state — the same orphan, minus the wait. Placing the record before the conditional also means one record site instead of two.

2. **`plan.ID` is mandatory, not tidiness.** `secretModel.ID` is `Computed` only (`secret_resource.go:80`) and is assigned *only* inside `readInto` (`:245`), which a failed wait never reaches. Without this line `RecordCreated`'s ID guard fires, drops the row and reports a provider bug — which is the safe outcome, but it means the fix does nothing. Without both the line *and* the guard, state would carry a null id, and `Delete` at `:215` would build `.../v1/secrets/` (the collection URL with a trailing slash, via `ServiceURL("secrets", "")`), while `Read`'s GET on the same URL would get a 200 list body and never report notFound — a row that can only be removed with `terraform state rm`. **I verified this by deleting the `plan.ID` line: the test fails with "create returned no state".**

3. **`plan.SecretRef` is free and worth setting.** It is already in hand and it is the attribute `pcd_keymanager_container` consumes (`keymanager_test.go:70`), so recording it keeps the tainted row self-describing.

4. **The tail guard is a strict improvement even ignoring the record.** Before the edit, `_, readDiags :=` discarded `readInto`'s `notFound` flag and line 165 set state unconditionally. `readInto` returns at `:234-236` (404) and `:238-239` (any other error) *without assigning anything to the model*, so `plan` still holds the create plan's unknowns for `id`, `secret_ref`, `status`, `creator_id`, `content_types`, `created_at`, `updated_at` and any omitted Optional+Computed attribute. That `Set` would write those unknowns over the clean row the record just made; Terraform core answers with "Provider produced inconsistent result after apply … contains unknown values" and then nulls them, so the user gets a confusing second error and a state whose server-managed attributes are blanked. Latent today (there is no state to clobber), live the moment the record lands.

5. **notFound and "unreadable" need opposite handling.** `notFound` means the secret is genuinely gone: report it *and* `RemoveResource`, matching this resource's own `Read` (`:182-187`) and the shipped `deleteCreatedImage` precedent, which removes state only when the object is known gone. A `readInto` error means the secret exists but could not be read: return early and leave the recorded row so Terraform taints it.

6. **Rejected shortcut, recorded so a reviewer sees it was considered:** do *not* re-run `tfstate.NullUnknowns` after the final `Set` as a cheaper alternative to the guard. It would make the notFound case "succeed" by writing a row whose every computed attribute is null.

7. **`NullUnknowns` must keep operating on `resp.State`, never on `plan`.** `readInto` decides whether to populate an attribute with `unset(v) == v.IsNull() || v.IsUnknown()` (`:283-285`, plus `BitLength` at `:260` and `Region` at `:275`). Nulling the in-memory `plan` would change which of those fire on the success path. `tfstate.NullUnknowns` (`internal/tfstate/tfstate.go:18-30`) transforms `*tfsdk.State` only, so this is already correct — just do not "helpfully" add a plan-side pass.

#### What the operator gets, traced

| Abandonment | Before | After |
|---|---|---|
| Barbican sets `ERROR` (`keymanager.go:73-74`) | apply fails, no state; secret invisible; next apply makes a second one | apply fails with the same message; secret tainted; next apply or destroy issues `DELETE /v1/secrets/<id>` |
| 5-minute timeout (`keymanager.go:34/63`) | orphan in `PENDING` | tainted; destroy deletes it (`PENDING` is the normal steady state for a payload-less secret, and Delete has no status gate) |
| Ctrl-C during the wait | orphan in `PENDING` | tainted; same |
| One transient 5xx on the poll — `gophercloud.WaitFor` aborts on the first predicate error with no retry (`util.go:88-89`) | orphan, almost certainly about to go `ACTIVE` | tainted; the next refresh reads `ACTIVE` and the taint is the only cost |
| Secret vanishes between wait and read-back | unconditional `Set` writes unknowns | reported and dropped from state |
| No payload, apply dies before the read-back | orphan | recorded at line 158, tainted |

- [ ] **Step 4: Run the test and watch it pass**

Run the focused test again. Then the full suite.

- [ ] **Step 5: Verify**

```bash
cd '/Users/pushkarmulay/Documents/Claude Code Projects/Terraform Provider/terraform-provider-pcd/.claude/worktrees/inspiring-kilby-0bbd0b' && gofmt -l internal/services/keymanager internal/tfstate
```
```bash
cd '/Users/pushkarmulay/Documents/Claude Code Projects/Terraform Provider/terraform-provider-pcd/.claude/worktrees/inspiring-kilby-0bbd0b' && go build ./...
```
```bash
cd '/Users/pushkarmulay/Documents/Claude Code Projects/Terraform Provider/terraform-provider-pcd/.claude/worktrees/inspiring-kilby-0bbd0b' && go vet ./internal/services/keymanager/...
```
```bash
cd '/Users/pushkarmulay/Documents/Claude Code Projects/Terraform Provider/terraform-provider-pcd/.claude/worktrees/inspiring-kilby-0bbd0b' && go test ./internal/services/keymanager/... -run TestSecretCreate -count=1 -v
```
```bash
cd '/Users/pushkarmulay/Documents/Claude Code Projects/Terraform Provider/terraform-provider-pcd/.claude/worktrees/inspiring-kilby-0bbd0b' && go test ./... -count=1
```
```bash
RED CHECK (must fail before it passes): comment out the four added lines 154-160 (plan.ID, plan.SecretRef, the comment and the RecordCreated guard), rerun the -run TestSecretCreate command, and confirm it fails with 'create returned no state: Terraform forgets sec-1 and the next apply creates a second secret'; then restore them. I ran this: it fails on the unmodified file and passes after the edit.
```
```bash
GUARD CHECK: delete only the 'plan.ID = types.StringValue(id)' line and rerun the -run TestSecretCreate command. It must fail the same way, because tfstate.RecordCreated's ID guard calls RemoveResource and reports a provider bug. This proves the guard and the plan.ID line are both load-bearing. I ran this too.
```
```bash
cd '/Users/pushkarmulay/Documents/Claude Code Projects/Terraform Provider/terraform-provider-pcd/.claude/worktrees/inspiring-kilby-0bbd0b' && grep -n 'func waitFor' internal/services/keymanager/*.go   # must print exactly one line, keymanager.go:62 waitForSecretActive — if a second waiter ever appears, this task's 'no delete waiter' premise is void
```
```bash
cd '/Users/pushkarmulay/Documents/Claude Code Projects/Terraform Provider/terraform-provider-pcd/.claude/worktrees/inspiring-kilby-0bbd0b' && grep -rn 'Default:\|booldefault\|int64default\|stringdefault\|mapdefault' internal/services/keymanager/secret_resource.go   # must print nothing: no attribute in this schema has a static default, so every Optional+Computed attribute the config omits is unknown in the plan
```
```bash
LAB (CE), happy path unchanged: TF_ACC=1 go test ./internal/services/keymanager/ -run TestAccKeyManagerSecretAndContainer_basic -v -timeout 30m
```
```bash
LAB (CE), the one server-side claim this tree cannot prove: create a secret through the API, get it into a non-ACTIVE state, then `openstack secret delete <href>` and confirm Barbican accepts a DELETE on a secret in PENDING and in ERROR. PENDING is the normal steady state for a payload-less secret, so the PENDING half is near-certain; ERROR is the one to actually observe.
```
```bash
LAB (CE), the replan check that no in-process test can catch (validates the five already-shipped resources as much as this one): apply a pcd_keymanager_secret whose create fails, then re-apply the tainted resource and confirm no 'Provider produced inconsistent result after apply' error. 14 of this schema's 17 attributes carry UseStateForUnknown, so the tainted row's nulls are candidates to be copied into the replacement plan.
```

- [ ] **Step 6: Commit**

```
fix(keymanager): keep a secret whose wait for ACTIVE fails

Create posted the secret to Barbican, waited for it to reach ACTIVE,
and returned the wait's error without saving state. Barbican keeps the
secret, so Terraform lost track of one that exists: the next apply
created a second secret and the first had to be deleted through the API
by hand. The same happened to an apply interrupted while it waited, and
to a single transient GET failure, which gophercloud.WaitFor does not
retry.

Create now records the secret as soon as secrets.Create returns its
ref, before the wait, through the shared tfstate.RecordCreated. The
apply still fails with Barbican's error and Terraform marks the secret
tainted, so the next apply or a destroy deletes it. The record sets id
and secret_ref explicitly: readInto is the only place that assigns
them, and a failed wait never reaches readInto, so without those two
lines the row would name nothing a destroy could delete. The record
goes before the payload check, not inside it: Barbican holds the secret
whether or not a payload was sent, and a secret created without one is
never waited on at all. The attributes Barbican has not reported yet
are saved as null and the next refresh reads them.

Also guard the read-back that ends Create. It discarded readInto's
notFound flag and set state unconditionally, so a 404 or a read error
after a successful wait wrote the plan's unknown values back over the
recorded row, which Terraform answers with an inconsistent-result error
and then nulls. That was latent while Create recorded nothing; it is
live now. A secret that has vanished is reported and dropped from
state; a secret that exists but cannot be read is reported and keeps
its recorded row.

No waiter changes. The package defines one waiter, waitForSecretActive,
which is a create waiter; Delete issues secrets.Delete with no pre- or
post-wait and treats a 404 as gone, and readInto never inspects status,
so a secret abandoned in ERROR or PENDING is still destroyable.
pcd_keymanager_container has no post-create wait and is unaffected.

Adds secret_failed_create_internal_test.go, the package's first unit
test: an httptest Barbican that answers 201 on POST /v1/secrets and
then reports the secret ERROR, driving Create, Read and Delete against
the real resource and asserting the create state is non-null, fully
known and carries the id Delete needs.
```

**Open questions and traps recorded during planning:**

- The settled design's delete-path work does not apply to this resource, and the plan should say so rather than force it. There is no delete waiter in package keymanager (one waiter total, the create-side waitForSecretActive at keymanager.go:62), Delete issues secrets.Delete immediately at secret_resource.go:215 with no pre- or post-wait, and readInto never inspects status. So waitForLoadBalancerSettled, settleBeforeDelete, and the transition-aware latch for waitForZoneDeleted / waitForLoadBalancerDeleted are all no-ops here. Task 2 has no risk gate and does not need to wait on Task 3.
- The one claim not readable from code: that Barbican accepts DELETE on a secret in ERROR. Nothing in gophercloud or the provider adds a guard, and PENDING is the normal steady state for a payload-less secret so that half is near-certain, but ERROR is unverified. If Barbican did refuse, the destroy would report Barbican's own error with state preserved — the same user-visible outcome as today's orphan, with a better message — so this is a verification item, not a gate. It is in the verification commands.
- The notFound branch drops the row on a 404 from the read-back. That is right for a secret that has genuinely vanished, but it would be wrong for a Barbican eventual-consistency 404 immediately after create, which would re-introduce the orphan the fix exists to remove. On the payload path this is not reachable in practice: waitForSecretActive has already GET the secret successfully before readInto runs. On the no-payload path readInto's GET is the very first one after the POST. I found no evidence Barbican is eventually consistent here (the POST returns the ref and the reference implementation writes the secret row in the same transaction), but I could not prove it from this tree. If the lab ever shows a 404 on the first GET after a payload-less create, downgrade that branch to AddError-and-keep-the-row.
- Task 0's exact signature is assumed. This task is written against `func RecordCreated(ctx context.Context, resp *resource.CreateResponse, plan any) bool` in internal/tfstate, including the ID guard that drops the row and reports a provider bug when the recorded `id` is null or empty. If Task 0 lands a different shape, only the one call at secret_resource.go:158 changes. If Task 0 slips entirely, the fallback is the three inline lines images uses at image_resource.go:198-201 (State.Set, tfstate.NullUnknowns, HasError guard) — but then this resource loses the ID guard, and the plan.ID line at :154 becomes silently mandatory rather than enforced, which is exactly the slip the guard exists to catch.
- Deliberately no cancel-during-wait test in this package, per the settled design: TestVolumeCreateKeepsAVolumeWhenTheWaitIsCanceled (blockstorage/failed_create_internal_test.go:153) pins the shared mechanism once. That is only true once Task 0's promote commit updates the comment at failed_create_internal_test.go:149-152 to name tfstate.RecordCreated. If Task 0 skips that comment edit, either do it here or add a cancel test to this file.
- Documentation line number: the design doc cites secret_resource.go:157 for this resource (doc line 197) and that is accurate today, but the wait moves to line 165. The entry should be deleted from 'Other resources with the same gap' and the resource moved into Scope, not renumbered — otherwise the next person to touch the doc renumbers a line that is about to move again.

---
### Task 4: `pcd_lb_loadbalancer`

Depends on Task 0; independent of Task 3 (the root has no pre-delete wait). Introduces the load balancer package's first unit test and owns `fakeConfig` in `internal/services/loadbalancer/failed_create_internal_test.go` — Task 5 appends to it. Also makes `waitForLoadBalancerDeleted` transition-aware and accepts `provisioning_status == DELETED` as success.

**Files:**
All paths are relative to the worktree root `/Users/pushkarmulay/Documents/Claude Code Projects/Terraform Provider/terraform-provider-pcd/.claude/worktrees/inspiring-kilby-0bbd0b`.

**Prerequisite:** Task 0 must already have landed, so that `internal/tfstate/tfstate.go` exports `RecordCreated(ctx context.Context, resp *resource.CreateResponse, plan any) bool` with the id guard. This task calls it and does not define a local `recordCreated`.

**Modify**

- `internal/services/loadbalancer/loadbalancer.go` — replace lines **132–154** (`waitForLoadBalancerDeleted`, doc comment included) with the transition-aware version. Two independent changes land here: (a) `DELETED` counts as gone, not just a 404; (b) `ERROR` counts as a delete failure only after the load balancer has been *seen leaving* `ERROR`. The error wrap also gains the last observed status. File grows 182 → 195 lines; `waitForLoadBalancerDeleted` ends up at 132–167.
- `internal/services/loadbalancer/loadbalancer_resource.go` — two edits, **apply the body edit first so the import edit does not shift its line numbers**:
  - lines **143–151** (`Create`'s wait + tail): insert the record between the create call and the wait, and guard the tail.
  - line **28** (end of the import block): add `"github.com/platform9/terraform-provider-pcd/internal/tfstate"` after the `internal/clients` import.
  - File grows 314 → 333 lines. After the edit `tfstate.RecordCreated` sits at line 147 and `Read` begins at line 172.
- `docs/superpowers/specs/2026-09-19-create-state-before-wait-design.md` — delete line **187** (`pcd_loadbalancer_loadbalancer` — the wrong type name, which Task 0 will already have corrected to `pcd_lb_loadbalancer`) from the **Other resources with the same gap** list, and add the resource to **Scope**. Amend the **Non-goals** bullet at line **41** (“Changing which statuses the waiters treat as failures.”) — this task changes exactly that in a delete waiter, so the bullet must be narrowed to create waits, with a sentence in **Design** stating the rule: a delete waiter may treat a status as a failure only once the object has been observed leaving it. Add a **Known gap** paragraph for `PENDING_CREATE` (see openQuestions).
- `CHANGELOG.md` — one `### Fixed` bullet under `## [Unreleased]`, American English, in the voice of the existing `pcd_blockstorage_volume` bullet at lines 51–60.

**Test**

- `internal/services/loadbalancer/failed_create_internal_test.go` — **new file**, `package loadbalancer` (in-package). This is the load balancer package's first unit test and first `httptest` fake; the only existing test file, `internal/services/loadbalancer/loadbalancer_test.go`, is `package loadbalancer_test` and `TF_ACC`-gated, and the two package clauses coexist in one directory without trouble. The `fakeConfig` helper this file declares is the package's only one — Task 5 must reuse it, not redeclare it.

**Do not touch in this task**

- `waitForLoadBalancerActive` (loadbalancer.go:107–130). Building on a broken load balancer must still fail.
- The four child resources, `waitForLoadBalancerSettled` / `settleBeforeDelete`, and the package doc at loadbalancer.go:10–11. Those are Task 3. This task is genuinely independent of Task 3: `waitForLoadBalancerDeleted` has exactly one caller (`loadbalancer_resource.go:265`), the children never call it, and the root's `Delete` has no pre-delete wait.
- `Read`'s ordering quirk at loadbalancer_resource.go:166–172, where the `notFound` branch returns before appending `diags`. Harmless — `readInto` returns no diagnostics on its 404 path — and out of scope.

**Interfaces:**
**Consumes**

- `func RecordCreated(ctx context.Context, resp *resource.CreateResponse, plan any) bool` — from `internal/tfstate` (Task 0). Sets state from `plan`, nulls unknowns, checks that the root `id` attribute is non-empty (dropping the row and erroring if it is not), and reports whether `Create` should carry on. Callers must set the object's ID on `plan` first.
- `func (r *loadBalancerResource) readInto(ctx context.Context, client *gophercloud.ServiceClient, id string, m *loadBalancerModel) (notFound bool, diags diag.Diagnostics)` — unchanged, `internal/services/loadbalancer/loadbalancer_resource.go:274`. Returns `(true, nil)` on a 404 without touching `m`, and `(false, diags)` with an error on any other failure, also without touching `m`. That is why the tail must branch on `notFound` rather than discarding it.
- `func (c *Config) LoadBalancerV2Client() (*gophercloud.ServiceClient, error)` — `internal/clients/config.go:326`. Calls `openstack.NewLoadBalancerV2`, which sets `ResourceBase = endpoint + "v2.0/"`, then `applyOverride(client, "load-balancer")`, which blanks `ResourceBase` **only** when `EndpointOverrides["load-balancer"]` is set. The test fake must therefore leave `EndpointOverrides` empty and serve `/v2.0/`-prefixed paths.
- `gophercloud.WaitFor(ctx, predicate)` — v2.13.0 `util.go:87`. Runs the predicate **once immediately**, then every second. Serial, so a bool captured by the predicate closure needs no synchronization.
- `loadbalancers.Create` / `Get` / `Delete` (gophercloud v2.13.0). Default OK codes: `POST {201,202}`, `GET {200}`, `DELETE {202,204}`. `DeleteOpts{Cascade: true}` serializes as the query string `?cascade=true`, so `r.Method + " " + r.URL.Path` switching in a fake works unchanged. Both `CreateResult` and `GetResult` decode a `{"loadbalancer": {...}}` wrapper. The Terraform attribute is `loadbalancer_provider` but the JSON key is `provider`.

**Produces**

- `func waitForLoadBalancerDeleted(ctx context.Context, client *gophercloud.ServiceClient, lbID string, timeout time.Duration) error` — signature **unchanged**; only the predicate and the error wrap change. New error text on the wedged path: `waiting for load balancer <id> to delete (last status "ERROR"): context deadline exceeded`. The fast-fail text for a delete that genuinely fails is preserved verbatim: `load balancer <id> entered ERROR provisioning status during delete`.
- `func (r *loadBalancerResource) Create(...)` — same signature. New behavior: on a failed or interrupted wait it returns its error **with** a fully-known, unknown-free state row carrying `id`, so Terraform taints the resource.
- `func fakeConfig(url string) *clients.Config` — new, unexported, `package loadbalancer`. Task 5 imports nothing for it; it is already in scope.

No exported API changes. No schema changes, so no `docs/resources/lb_loadbalancer.md` regeneration.

- [ ] **Step 1: Write the failing test**

Add to the test file named above:

```go
// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package loadbalancer

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"

	"github.com/platform9/terraform-provider-pcd/internal/clients"
)

// fakeConfig points a clients.Config at a test server, so the Octavia client the
// resources build resolves to it. The locator ignores the endpoint options, so
// it answers for every service type and availability. Note that every request
// path carries a /v2.0/ prefix: openstack.NewLoadBalancerV2 sets the client's
// ResourceBase to the endpoint plus "v2.0/", unlike the Cinder and Glance
// clients the other packages' fakes serve. EndpointOverrides must stay empty,
// since an override blanks ResourceBase and drops the prefix.
func fakeConfig(url string) *clients.Config {
	return &clients.Config{
		Region: "region-one",
		Provider: &gophercloud.ProviderClient{
			EndpointLocator: func(gophercloud.EndpointOpts) (string, error) { return url + "/", nil },
		},
	}
}

// A load balancer whose build ends in ERROR stays in Octavia. Create used to
// return without state, so Terraform forgot the load balancer, the next apply
// created another, and the first had to be deleted through the API. Create must
// return the error with the load balancer in state (Terraform then taints it),
// and the refresh and delete a destroy runs must remove it even though Octavia
// reports ERROR on the first poll after the DELETE.
func TestLoadBalancerCreateKeepsALoadBalancerThatFailedToBuild(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	var mu sync.Mutex
	deleteCalled, getsAfterDelete := false, 0
	octavia := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch r.Method + " " + r.URL.Path {
		case "POST /v2.0/lbaas/loadbalancers":
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{"loadbalancer": {"id": "lb-1", "provisioning_status": "PENDING_CREATE"}}`)
		case "GET /v2.0/lbaas/loadbalancers/lb-1":
			if deleteCalled {
				getsAfterDelete++
				if getsAfterDelete > 1 {
					w.WriteHeader(http.StatusNotFound)
					return
				}
			}
			// Octavia keeps answering ERROR on the first poll after the DELETE
			// is accepted; the delete waiter must poll past it, not bail.
			fmt.Fprint(w, `{"loadbalancer": {"id": "lb-1", "name": "tf-lb", "description": "",
				"admin_state_up": true, "vip_subnet_id": "subnet-1", "vip_network_id": "net-1",
				"vip_address": "10.0.0.5", "vip_port_id": "port-1", "flavor_id": "",
				"provider": "ovn", "provisioning_status": "ERROR", "operating_status": "OFFLINE",
				"tags": []}}`)
		case "DELETE /v2.0/lbaas/loadbalancers/lb-1":
			// DeleteOpts{Cascade: true} sends ?cascade=true, which is a query
			// string and so is not part of r.URL.Path.
			if got := r.URL.Query().Get("cascade"); got != "true" {
				t.Errorf("delete sent cascade=%q, want true", got)
			}
			deleteCalled = true
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotImplemented)
		}
	}))
	defer octavia.Close()

	r := &loadBalancerResource{config: fakeConfig(octavia.URL)}
	var sch resource.SchemaResponse
	r.Schema(ctx, resource.SchemaRequest{}, &sch)
	s := sch.Schema

	// vip_subnet_id must be a known, non-empty string: isSet treats unknown as
	// unset, and a plan with neither VIP attribute set trips the "Invalid VIP"
	// guard before Create makes any request. admin_state_up must be known too,
	// since Create dereferences it unconditionally and an unknown Bool would
	// silently send admin_state_up: false.
	planned := loadBalancerModel{
		ID:                 types.StringUnknown(),
		Name:               types.StringValue("tf-lb"),
		Description:        types.StringUnknown(),
		AdminStateUp:       types.BoolValue(true),
		VipSubnetID:        types.StringValue("subnet-1"),
		VipNetworkID:       types.StringUnknown(),
		VipAddress:         types.StringUnknown(),
		VipPortID:          types.StringUnknown(),
		FlavorID:           types.StringUnknown(),
		Provider:           types.StringValue("ovn"),
		Tags:               types.SetUnknown(types.StringType),
		ProvisioningStatus: types.StringUnknown(),
		OperatingStatus:    types.StringUnknown(),
		Region:             types.StringUnknown(),
	}
	plan := tfsdk.Plan{Schema: s, Raw: tftypes.NewValue(s.Type().TerraformType(ctx), nil)}
	if d := plan.Set(ctx, &planned); d.HasError() {
		t.Fatalf("building the plan: %v", d)
	}

	createResp := resource.CreateResponse{State: tfsdk.State{Schema: s, Raw: tftypes.NewValue(s.Type().TerraformType(ctx), nil)}}
	r.Create(ctx, resource.CreateRequest{Plan: plan}, &createResp)
	if !createResp.Diagnostics.HasError() {
		t.Fatal("create succeeded; want the ERROR provisioning status reported")
	}
	if createResp.State.Raw.IsNull() {
		t.Fatal("create returned no state: Terraform forgets lb-1 and the next apply creates a second load balancer")
	}
	if !createResp.State.Raw.IsFullyKnown() {
		t.Fatalf("create state holds unknown values, which Terraform refuses: %v", createResp.State.Raw)
	}
	var got loadBalancerModel
	if d := createResp.State.Get(ctx, &got); d.HasError() {
		t.Fatalf("reading the create state: %v", d)
	}
	if got.ID.ValueString() != "lb-1" {
		t.Fatalf("create state id = %s, want lb-1: a row without an ID names nothing a destroy could delete", got.ID)
	}
	if got.Name.ValueString() != "tf-lb" || got.VipSubnetID.ValueString() != "subnet-1" {
		t.Fatalf("create state name=%s vip_subnet_id=%s; want tf-lb, subnet-1", got.Name, got.VipSubnetID)
	}
	if got.AdminStateUp.IsNull() || !got.AdminStateUp.ValueBool() {
		t.Fatalf("create state admin_state_up = %s, want true", got.AdminStateUp)
	}

	// terraform destroy (or the replacing apply) refreshes the tainted load
	// balancer first.
	readResp := resource.ReadResponse{State: createResp.State}
	r.Read(ctx, resource.ReadRequest{State: createResp.State}, &readResp)
	if readResp.Diagnostics.HasError() {
		t.Fatalf("refresh of the failed load balancer: %v", readResp.Diagnostics)
	}
	if readResp.State.Raw.IsNull() {
		t.Fatal("refresh dropped lb-1 from state; want it kept and reported in ERROR")
	}
	if d := readResp.State.Get(ctx, &got); d.HasError() {
		t.Fatalf("reading the refreshed state: %v", d)
	}
	if got.ProvisioningStatus.ValueString() != lbError {
		t.Fatalf("refreshed provisioning_status = %s, want ERROR", got.ProvisioningStatus)
	}
	if got.Region.ValueString() != "region-one" {
		t.Fatalf("refreshed region = %s, want region-one", got.Region)
	}

	deleteResp := resource.DeleteResponse{State: readResp.State}
	r.Delete(ctx, resource.DeleteRequest{State: readResp.State}, &deleteResp)
	if deleteResp.Diagnostics.HasError() {
		t.Fatalf("delete of a load balancer in ERROR: %v", deleteResp.Diagnostics)
	}
	mu.Lock()
	defer mu.Unlock()
	if !deleteCalled {
		t.Fatal("delete never called DELETE /v2.0/lbaas/loadbalancers/lb-1")
	}
	if getsAfterDelete < 2 {
		t.Fatalf("delete returned after %d polls; want it to poll past the ERROR status until Octavia answers 404", getsAfterDelete)
	}
}

// waitForLoadBalancerDeleted must take a provisioning status of DELETED as
// gone. Octavia normally answers 404 for a deleted load balancer, but the
// waiter used to end only on that 404, so a DELETED answer polled to the full
// 10-minute defaultLBTimeout. The short timeout here keeps a regression to a
// two-second failure instead of a ten-minute hang.
func TestWaitForLoadBalancerDeletedAcceptsDeletedStatus(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	octavia := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method+" "+r.URL.Path != "GET /v2.0/lbaas/loadbalancers/lb-1" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotImplemented)
			return
		}
		fmt.Fprint(w, `{"loadbalancer": {"id": "lb-1", "provisioning_status": "DELETED",
			"operating_status": "OFFLINE", "tags": []}}`)
	}))
	defer octavia.Close()

	client, err := fakeConfig(octavia.URL).LoadBalancerV2Client()
	if err != nil {
		t.Fatalf("building the client: %v", err)
	}
	if err := waitForLoadBalancerDeleted(ctx, client, "lb-1", 2*time.Second); err != nil {
		t.Fatalf("waiting on a load balancer Octavia reports DELETED: %v", err)
	}
}

// A load balancer that never leaves ERROR — the create abandoned it there and
// the backend delete failed too — must still fail the destroy, since the load
// balancer really is still present. The failure has to be the timeout, not the
// old first-poll bail, and it has to name the status so the operator knows why
// the wait ran long.
func TestWaitForLoadBalancerDeletedTimesOutOnAnErrorThatNeverClears(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	var mu sync.Mutex
	polls := 0
	octavia := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.Method+" "+r.URL.Path != "GET /v2.0/lbaas/loadbalancers/lb-1" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotImplemented)
			return
		}
		polls++
		fmt.Fprint(w, `{"loadbalancer": {"id": "lb-1", "provisioning_status": "ERROR",
			"operating_status": "OFFLINE", "tags": []}}`)
	}))
	defer octavia.Close()

	client, err := fakeConfig(octavia.URL).LoadBalancerV2Client()
	if err != nil {
		t.Fatalf("building the client: %v", err)
	}
	err = waitForLoadBalancerDeleted(ctx, client, "lb-1", 2*time.Second)
	if err == nil {
		t.Fatal("wait succeeded; want the still-present load balancer reported")
	}
	if strings.Contains(err.Error(), "entered ERROR provisioning status during delete") {
		t.Fatalf("wait bailed on the abandoned ERROR status instead of polling past it: %v", err)
	}
	if want := `last status "ERROR"`; !strings.Contains(err.Error(), want) {
		t.Fatalf("wait error = %q; want it to name the status with %q", err, want)
	}
	mu.Lock()
	defer mu.Unlock()
	if polls < 2 {
		t.Fatalf("wait polled %d times; want it to keep polling past the first ERROR", polls)
	}
}

// The opposite case must keep working: a healthy load balancer whose delete
// genuinely fails, so Octavia moves it from PENDING_DELETE into ERROR, still
// fails fast with the status message rather than waiting out the timeout.
func TestWaitForLoadBalancerDeletedFailsOnAnErrorEnteredDuringDelete(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	var mu sync.Mutex
	polls := 0
	octavia := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.Method+" "+r.URL.Path != "GET /v2.0/lbaas/loadbalancers/lb-1" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotImplemented)
			return
		}
		polls++
		status := "PENDING_DELETE"
		if polls > 1 {
			status = "ERROR"
		}
		fmt.Fprintf(w, `{"loadbalancer": {"id": "lb-1", "provisioning_status": %q,
			"operating_status": "OFFLINE", "tags": []}}`, status)
	}))
	defer octavia.Close()

	client, err := fakeConfig(octavia.URL).LoadBalancerV2Client()
	if err != nil {
		t.Fatalf("building the client: %v", err)
	}
	err = waitForLoadBalancerDeleted(ctx, client, "lb-1", defaultLBTimeout)
	if err == nil {
		t.Fatal("wait succeeded; want the ERROR entered during the delete reported")
	}
	if !strings.Contains(err.Error(), "entered ERROR provisioning status during delete") {
		t.Fatalf("wait error = %q; want the delete-failure message", err)
	}
}
```

- [ ] **Step 2: Run it and watch it fail**

Run the focused test for this resource. It must fail because `Create` returns no state — not for a compile error or a fake-server mismatch. If it fails for another reason, fix the test before touching the resource.

- [ ] **Step 3: Write the implementation**

#### Edit 1 — `internal/services/loadbalancer/loadbalancer.go`, lines 132–154

**Before:**

```go
// waitForLoadBalancerDeleted blocks until the load balancer is gone (404). Used
// after a cascade delete of the root load balancer.
func waitForLoadBalancerDeleted(ctx context.Context, client *gophercloud.ServiceClient, lbID string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	err := gophercloud.WaitFor(ctx, func(ctx context.Context) (bool, error) {
		lb, err := loadbalancers.Get(ctx, client, lbID).Extract()
		if err != nil {
			if gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
				return true, nil
			}
			return false, err
		}
		if lb.ProvisioningStatus == lbError {
			return false, fmt.Errorf("load balancer %s entered ERROR provisioning status during delete", lbID)
		}
		return false, nil
	})
	if err != nil {
		return fmt.Errorf("waiting for load balancer %s to delete: %w", lbID, err)
	}
	return nil
}
```

**After:**

```go
// waitForLoadBalancerDeleted blocks until the load balancer is gone: a GET that
// answers 404 and a provisioning status of DELETED both count as gone. A load
// balancer a failed create wait abandoned in ERROR is still deletable, and
// Octavia can still report ERROR on the first poll after the DELETE is accepted,
// so ERROR counts as a delete failure only once the load balancer has been seen
// leaving it. Used after a cascade delete of the root load balancer.
func waitForLoadBalancerDeleted(ctx context.Context, client *gophercloud.ServiceClient, lbID string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	lastStatus, seenNonError := "", false
	err := gophercloud.WaitFor(ctx, func(ctx context.Context) (bool, error) {
		lb, err := loadbalancers.Get(ctx, client, lbID).Extract()
		if err != nil {
			if gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
				return true, nil
			}
			return false, err
		}
		lastStatus = lb.ProvisioningStatus
		if lb.ProvisioningStatus == lbDeleted {
			return true, nil
		}
		if lb.ProvisioningStatus != lbError {
			seenNonError = true
			return false, nil
		}
		if seenNonError {
			return false, fmt.Errorf("load balancer %s entered ERROR provisioning status during delete", lbID)
		}
		return false, nil
	})
	if err != nil {
		return fmt.Errorf("waiting for load balancer %s to delete (last status %q): %w", lbID, lastStatus, err)
	}
	return nil
}
```

No import changes: `context`, `fmt`, `net/http`, `time`, `gophercloud` and `loadbalancers` are all already imported by this file.

**What the operator gets, branch by branch.**

| Branch | Before | After |
| --- | --- | --- |
| Healthy LB, delete genuinely fails and Octavia sets ERROR | poll 1 sees `PENDING_DELETE`, later ERROR fails fast | **unchanged** — poll 1 sets `seenNonError`, the later ERROR fails fast with the same message |
| LB abandoned in ERROR, DELETE accepted, first poll still ERROR, then `PENDING_DELETE`, then 404 | destroy fails spuriously on the first poll and the LB stays in state | **fixed** — polls past it and succeeds |
| LB abandoned in ERROR, backend delete also fails, never leaves ERROR | fails instantly every attempt; only escape is `terraform state rm` | polls to the 10-minute `defaultLBTimeout`, then fails with `... (last status "ERROR"): context deadline exceeded`. Still fails — correctly, the LB is still there — but it is no longer permanently wedged by a status |
| GET answers 200 with `provisioning_status: DELETED` | spins the full 10 minutes | **fixed** — returns immediately |

The `DELETED` line is an independent one-line fix and should be called out as such in review; it can only convert a hang into a success. It also resolves an inconsistency in the survey, which simultaneously assumed a deleted LB 404s and noted that a `DELETED` answer would hang.

---

#### Edit 2a — `internal/services/loadbalancer/loadbalancer_resource.go`, lines 143–151

Apply this **before** Edit 2b, so the line numbers above hold.

**Before** (lines 137–151, showing the create call above for context):

```go
	lb, err := loadbalancers.Create(ctx, client, createOpts).Extract()
	if err != nil {
		resp.Diagnostics.AddError("loadbalancer: creating load balancer", err.Error())
		return
	}

	if err := waitForLoadBalancerActive(ctx, client, lb.ID, defaultLBTimeout); err != nil {
		resp.Diagnostics.AddError("loadbalancer: waiting for load balancer to become active", err.Error())
		return
	}

	_, readDiags := r.readInto(ctx, client, lb.ID, &plan)
	resp.Diagnostics.Append(readDiags...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}
```

**After:**

```go
	lb, err := loadbalancers.Create(ctx, client, createOpts).Extract()
	if err != nil {
		resp.Diagnostics.AddError("loadbalancer: creating load balancer", err.Error())
		return
	}

	// Octavia keeps a load balancer whose build fails (provisioning status
	// ERROR), so record it before the wait below can return without it.
	plan.ID = types.StringValue(lb.ID)
	if !tfstate.RecordCreated(ctx, resp, &plan) {
		return
	}

	if err := waitForLoadBalancerActive(ctx, client, lb.ID, defaultLBTimeout); err != nil {
		resp.Diagnostics.AddError("loadbalancer: waiting for load balancer to become active", err.Error())
		return
	}

	notFound, readDiags := r.readInto(ctx, client, lb.ID, &plan)
	resp.Diagnostics.Append(readDiags...)
	if notFound {
		resp.Diagnostics.AddError("loadbalancer: reading load balancer after create",
			fmt.Sprintf("Load balancer %s no longer exists.", lb.ID))
		resp.State.RemoveResource(ctx)
		return
	}
	if resp.Diagnostics.HasError() {
		// The load balancer exists and is recorded; it just could not be read
		// back. Leave the row RecordCreated wrote so Terraform taints it.
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}
```

Three things about this edit that are easy to get wrong:

1. **`plan.ID` must be set explicitly.** Nothing in `Create` ever set it before: `m.ID` is assigned only inside `readInto` (line 284), which the failure path never reaches. Without this line `RecordCreated`'s id guard drops the row and reports a provider bug — better than writing an undeletable row, but still no fix.
2. **The tail's `notFound` and the final `Set` were both unguarded.** `readInto` returns before assigning anything to the model on a 404 *or* on any other error, so `plan` still carries the create plan's unknowns (`vip_port_id`, `provisioning_status`, `operating_status`, …). The old unconditional `resp.State.Set(ctx, &plan)` would then overwrite the clean, unknown-free row this fix just recorded with a state full of unknown values, which Terraform core rejects outright. Latent today because there is no state to clobber; live the moment the record lands. `notFound` and an error need opposite handling: a 404 means the object is genuinely gone, so drop the row (matching `Read`'s own branch at lines 167–172); an error means it exists but could not be read, so keep the row and let Terraform taint it.
3. **Do not "simplify" this by re-running `tfstate.NullUnknowns` after the final `Set`.** That would make the `notFound` case *succeed*, writing a row whose every computed attribute is null for a load balancer that no longer exists.

Nothing on the success path changes: `readInto` overwrites all thirteen non-region attributes plus the region fallback, so the recorded row is invisible once the wait succeeds. `NullUnknowns` operates on `resp.State`, never on the `plan` struct — that is load-bearing, because `readInto`'s region fallback at line 305 tests `m.Region.IsNull() || m.Region.IsUnknown()`.

---

#### Edit 2b — `internal/services/loadbalancer/loadbalancer_resource.go`, line 28

**Before:**

```go
	"github.com/platform9/terraform-provider-pcd/internal/clients"
)
```

**After:**

```go
	"github.com/platform9/terraform-provider-pcd/internal/clients"
	"github.com/platform9/terraform-provider-pcd/internal/tfstate"
)
```

`fmt` and `types` are already imported (lines 12 and 26).

---

#### Edit 3 — `docs/superpowers/specs/2026-09-19-create-state-before-wait-design.md`

- Remove line 187 from **Other resources with the same gap** and add `pcd_lb_loadbalancer` to **Scope**.
- Narrow the **Non-goals** bullet at line 41 from “Changing which statuses the waiters treat as failures.” to scope it to *create* waits, and add the rule this task follows to **Design**: a delete waiter may treat a status as a failure only once the object has been observed leaving it, since `ERROR` on Designate and Octavia is overloaded across both the create and the delete phase. Without this the commit silently contradicts the shipped doc.
- Add to **Known gap**: an Octavia load balancer abandoned by a timeout or a Ctrl-C sits in `PENDING_CREATE`, which Octavia treats as immutable, so `loadbalancers.Delete` at `loadbalancer_resource.go:258` returns 409 and the destroy fails until Octavia settles it. Same shape as the Cinder `creating` gap, and deliberately not fixed. Keeping state is still an improvement: Terraform at least knows the object exists.

---

#### Edit 4 — `CHANGELOG.md`, under `## [Unreleased]` / `### Fixed`

```markdown
- `pcd_lb_loadbalancer`: a load balancer Octavia accepts and then fails to build (provisioning
  status `ERROR`) now stays in state, and so does one whose create wait times out or whose apply is
  interrupted while it builds. The apply still fails with Octavia's reason, and Terraform marks the
  load balancer tainted, so the next apply deletes and recreates it and a destroy deletes it.
  Before, the failed apply left no state: Terraform lost track of a load balancer Octavia kept, the
  next apply created a second one, and the first had to be deleted through the API. The attributes
  Octavia had not reported yet are saved empty until the next refresh. Destroying such a load
  balancer also used to fail: the delete waiter treated the `ERROR` status the create had abandoned
  it in as a delete failure and gave up on its first poll, every time, so the only way out was
  `terraform state rm`. It now polls past that status, and reports a delete failure only once the
  load balancer has been seen leaving `ERROR` — the case where Octavia really did fail the delete
  still fails fast, with the same message. A load balancer Octavia reports as `DELETED` rather than
  answering 404 is now taken as deleted instead of polled for the full ten minutes. A create wait
  that times out leaves the load balancer in `PENDING_CREATE`, which Octavia refuses to delete;
  because the load balancer is now tainted, that refusal blocks the next apply, not only a destroy,
  until Octavia settles it.
```

- [ ] **Step 4: Run the test and watch it pass**

Run the focused test again. Then the full suite.

- [ ] **Step 5: Verify**

```bash
cd '/Users/pushkarmulay/Documents/Claude Code Projects/Terraform Provider/terraform-provider-pcd/.claude/worktrees/inspiring-kilby-0bbd0b' && gofmt -s -l internal/
```
```bash
cd '/Users/pushkarmulay/Documents/Claude Code Projects/Terraform Provider/terraform-provider-pcd/.claude/worktrees/inspiring-kilby-0bbd0b' && go build ./...
```
```bash
cd '/Users/pushkarmulay/Documents/Claude Code Projects/Terraform Provider/terraform-provider-pcd/.claude/worktrees/inspiring-kilby-0bbd0b' && go vet ./...
```
```bash
cd '/Users/pushkarmulay/Documents/Claude Code Projects/Terraform Provider/terraform-provider-pcd/.claude/worktrees/inspiring-kilby-0bbd0b' && go test ./internal/services/loadbalancer/ -run 'LoadBalancer' -v -count=1 -race -timeout 60s
```
```bash
cd '/Users/pushkarmulay/Documents/Claude Code Projects/Terraform Provider/terraform-provider-pcd/.claude/worktrees/inspiring-kilby-0bbd0b' && make test
```
```bash
cd '/Users/pushkarmulay/Documents/Claude Code Projects/Terraform Provider/terraform-provider-pcd/.claude/worktrees/inspiring-kilby-0bbd0b' && make lint
```
```bash
# Negative control 1 — temporarily revert Edit 1 (the waiter) and confirm three tests fail:
#   TestWaitForLoadBalancerDeletedTimesOutOnAnErrorThatNeverClears -> 'wait bailed on the abandoned ERROR status'
#   TestLoadBalancerCreateKeepsALoadBalancerThatFailedToBuild     -> 'delete of a load balancer in ERROR'
#   TestWaitForLoadBalancerDeletedAcceptsDeletedStatus            -> 'context deadline exceeded'
# Then restore. (Verified: all three fail with exactly these messages.)
```
```bash
# Negative control 2 — temporarily revert only the plan.ID + RecordCreated block from Edit 2a and confirm
#   TestLoadBalancerCreateKeepsALoadBalancerThatFailedToBuild fails with
#   'create returned no state: Terraform forgets lb-1 ...'. Then restore.
```
```bash
# CE lab, risk item A (bounded either way, see openQuestions): create a pcd_lb_loadbalancer, drive it into
# ERROR, then `terraform destroy`. Confirm the DELETE is issued, Octavia accepts it, and the destroy completes.
#   openstack loadbalancer show <id> -f value -c provisioning_status
```
```bash
# CE lab, risk item B (validates the five already-shipped resources as much as these eight): create a
# pcd_blockstorage_volume, force the create to fail so the resource is tainted, then re-apply and confirm no
# 'Provider produced inconsistent result after apply' error. The in-process unit tests provably cannot catch
# this, because they call Create/Read/Delete directly and never go through Terraform core's plan.
```

- [ ] **Step 6: Commit**

```
fix(loadbalancer): keep a load balancer whose build fails in state

Create called loadbalancers.Create, waited for the load balancer to
become ACTIVE, and returned the wait's error without saving state.
Octavia keeps a load balancer whose build fails in provisioning status
ERROR, so the failed apply left Terraform with no record of a load
balancer that exists: the next apply created a second one, and the
first had to be deleted through the API by hand. The same happened to
a create wait that timed out or an apply interrupted while the load
balancer built.

Create now records the load balancer through tfstate.RecordCreated as
soon as Octavia returns its ID, before the wait. The apply still fails
with Octavia's reason, Terraform marks the load balancer tainted, and
the next apply or a destroy deletes it. plan.ID has to be set by hand
first: it was only ever assigned inside readInto, which runs after the
wait and so never ran on this path.

Destroying such a load balancer would not have worked without a second
fix. waitForLoadBalancerDeleted treated ERROR as a delete failure and
gophercloud.WaitFor runs its predicate immediately, so a load balancer
the create had abandoned in ERROR failed the wait on its first poll,
every attempt, leaving terraform state rm as the only way out. ERROR
now counts as a delete failure only once the load balancer has been
seen leaving it, which is the difference between "was already in ERROR
before the DELETE" and "entered ERROR while being deleted". A delete
that genuinely fails still fails fast with the same message; one that
is merely slow to leave ERROR polls to the existing 10-minute timeout,
whose error now names the last status observed. While in those lines,
also treat a provisioning status of DELETED as deleted: only a 404
ended the wait before, so a load balancer Octavia reported as DELETED
would have polled the full 10 minutes.

Create's tail was unguarded and would have clobbered the new record.
It discarded readInto's notFound flag and set state unconditionally,
and readInto returns without touching the model on a 404 or an error,
so plan still held the create plan's unknown values. It now removes
the resource when the load balancer is gone and keeps the recorded row
when it merely could not be read.

Adds the load balancer package's first unit tests, with an Octavia
fake. Note that its paths carry a /v2.0/ prefix, unlike the Cinder and
Glance fakes: NewLoadBalancerV2 sets the client's ResourceBase.

A create wait that times out leaves the load balancer in
PENDING_CREATE, which Octavia refuses to delete; that gap is recorded
in the design doc rather than closed here.
```

**Open questions and traps recorded during planning:**

- SETTLED-DESIGN CONFLICT THAT MUST BE RESOLVED IN THE DOC, NOT THE CODE. The shipped design doc's Non-goals at docs/superpowers/specs/2026-09-19-create-state-before-wait-design.md:41 says "Changing which statuses the waiters treat as failures." This task changes exactly that in waitForLoadBalancerDeleted. The settled design chose the transition-aware latch knowingly, so the code is right, but the doc bullet must be narrowed to create waits and the new rule written down ("a delete waiter may treat a status as a failure only once the object has been observed leaving it"), or the commit silently contradicts the spec it cites. I have put this in Edit 3 rather than leaving it implicit.
- UNVERIFIABLE FROM THIS TREE, AND BOUNDED EITHER WAY FOR THIS RESOURCE: that Octavia accepts a DELETE on a load balancer in ERROR. Unlike the four children, this is NOT a gate. The root's Delete has no pre-delete wait — loadbalancer_resource.go:258 goes straight to loadbalancers.Delete with Cascade:true — so the request is issued regardless. If Octavia refuses with 409, Delete AddErrors at :262 and the row stays in state: a loud, retryable failure with state preserved, strictly better than today's silent orphan. Lab-verify it, but do not hold the task on it. This is the opposite of Task 3, where the same question genuinely is a ship/no-ship gate.
- The survey contained an internal contradiction I had to resolve rather than carry forward: it asserted the DELETED abandonment path "destroys cleanly because a subsequent GET 404s" while separately noting that waitForLoadBalancerDeleted never accepted provisioning_status DELETED as success. Those cannot both be assumed. Edit 1 resolves it by making DELETED a success, so the claim holds whichever way Octavia answers. Flagging it because the plan's reviewers will have read the survey.
- MERGE HAZARD BETWEEN THIS TASK AND TASK 5. This task introduces `fakeConfig` in `package loadbalancer`. Task 5 (the four LB children's Create record) will want the same helper and MUST reuse this one — a second `func fakeConfig` anywhere in `internal/services/loadbalancer/` is a duplicate-symbol compile error, not a test failure. The other packages each have their own copy (blockstorage and images), so the per-package pattern is right; it is only within this package that the two tasks collide. Worth one line in the Task 5 section.
- The error wrap is mildly redundant on one branch: when the latch fires, the message reads `waiting for load balancer lb-1 to delete (last status "ERROR"): load balancer lb-1 entered ERROR provisioning status during delete`. I kept it because the graft asks for lastStatus unconditionally and the same shape is going into waitForZoneDeleted in Task 1, and consistency between the two waiters is worth more than trimming one duplicated word. Say so if a reviewer raises it.
- The UseStateForUnknown replan check is the one risk in this change that the unit tests provably cannot cover, and this resource is a heavy user of it: id, name, description, vip_port_id, provisioning_status, operating_status and region carry UseStateForUnknown; vip_subnet_id, vip_network_id, vip_address, flavor_id and loadbalancer_provider carry RequiresReplace plus UseStateForUnknown; tags carries setplanmodifier.UseStateForUnknown. After a tainted create those are null in state. I expect the modifier to be inert, because Terraform core plans the create half of a replace with a null prior state, but I could not confirm that from this tree and the tests never go through core's plan. The already-shipped pcd_blockstorage_volume has identical exposure, so one lab run settles all thirteen call sites at once — listed as the last verification command.
- DELIBERATELY NOT DONE HERE, so it does not look like an omission: the package doc at internal/services/loadbalancer/loadbalancer.go:10-11 still claims every resource waits for the root to be ACTIVE before and after each mutation using waitForLoadBalancerActive. That sentence only becomes false when Task 3 swaps the four children's pre-delete gates to waitForLoadBalancerSettled, so the correction belongs to Task 3's commit, not this one. Nothing in this task falsifies it.

---
### Task 3: Unblock the load balancer children's delete path — LAB GATE

**This task changes no `Create`.** It exists so that Task 5 can record the children without leaving resources that cannot be destroyed.

**Files:**
- Modify: `internal/services/loadbalancer/loadbalancer.go` (add `waitForLoadBalancerSettled` and `settleBeforeDelete`; update the package doc at lines 7-11)
- Modify: `internal/services/loadbalancer/listener_resource.go:304`, `:315`
- Modify: `internal/services/loadbalancer/pool_resource.go:295`, `:306`, and the root resolution at `:290`
- Modify: `internal/services/loadbalancer/member_resource.go:274`, `:285`, and the root resolution at `:269`
- Modify: `internal/services/loadbalancer/monitor_resource.go:280`, `:291`, and the root resolution at `:275`

**Interfaces:**
- Produces: `waitForLoadBalancerSettled(ctx, client, lbID string, timeout time.Duration) (string, error)` and `settleBeforeDelete(ctx, client, lbID, child string, diags *diag.Diagnostics) bool`. Task 5 depends on both existing.

- [ ] **Step 1: Add `waitForLoadBalancerSettled`**

Next to `waitForLoadBalancerActive` in `loadbalancer.go`:

```go
// waitForLoadBalancerSettled blocks until the root load balancer leaves its
// transient PENDING_* statuses and reports the status it settled on. Octavia
// rejects changes with HTTP 409 only while the load balancer is PENDING_*
// (see the package doc), so ERROR is a settled status that still accepts a
// child delete. Delete paths use this rather than waitForLoadBalancerActive:
// a child recorded by a create whose wait gave up on an ERROR root must stay
// destroyable. A load balancer that is already gone counts as settled — its
// children went with it.
func waitForLoadBalancerSettled(ctx context.Context, client *gophercloud.ServiceClient, lbID string, timeout time.Duration) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	status := ""
	err := gophercloud.WaitFor(ctx, func(ctx context.Context) (bool, error) {
		lb, err := loadbalancers.Get(ctx, client, lbID).Extract()
		if err != nil {
			if gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
				status = lbDeleted
				return true, nil
			}
			return false, err
		}
		status = lb.ProvisioningStatus
		switch lb.ProvisioningStatus {
		case lbActive, lbError, lbDeleted:
			return true, nil
		default: // PENDING_CREATE / PENDING_UPDATE / PENDING_DELETE
			return false, nil
		}
	})
	if err != nil {
		return status, fmt.Errorf("waiting for load balancer %s to settle: %w", lbID, err)
	}
	return status, nil
}
```

Enumerate the settled set exactly as the existing switch does. Do **not** use `strings.HasPrefix("PENDING_")` — that depends on a naming convention Octavia never promised.

- [ ] **Step 2: Add `settleBeforeDelete`**

```go
// settleBeforeDelete waits for the root load balancer to accept changes again
// and reports whether the caller should carry on. A root in ERROR is warned
// about, not failed: the child is deleted anyway, and if Octavia refuses the
// request the delete call reports that itself. phase names which of a Delete's
// two waits called in, so a post-delete failure does not read as a pre-delete one.
func settleBeforeDelete(ctx context.Context, client *gophercloud.ServiceClient, lbID, child, phase string, diags *diag.Diagnostics) bool {
	status, err := waitForLoadBalancerSettled(ctx, client, lbID, defaultLBTimeout)
	if err != nil {
		diags.AddError(fmt.Sprintf("loadbalancer: waiting %s %s delete", phase, child), err.Error())
		return false
	}
	if status == lbError {
		diags.AddWarning("Load balancer in ERROR provisioning status",
			fmt.Sprintf("Load balancer %s is in ERROR provisioning status. Deleting the %s anyway. "+
				"If Octavia refuses the request, repair or delete the load balancer and retry the destroy.", lbID, child))
	}
	return true
}
```

The `phase` parameter is a correction to the settled design, which hard-coded "before" and would have reported a post-delete failure as a pre-delete one. Callers pass `"before"` and `"after"`.

- [ ] **Step 3: Swap all eight child waits**

At each of the eight sites listed under **Files**, replace `waitForLoadBalancerActive(...)` with `settleBeforeDelete(ctx, client, lbID, "<child>", "before"|"after", &resp.Diagnostics)`. The pre-delete site returns when it reports `false`; the post-delete site records diagnostics as it does today.

**Both** waits move, not just the pre-delete one. The post-delete wait exists so the next resource in the destroy does not hit a 409, and `waitForLoadBalancerSettled` serves that identically — it never returns while `PENDING_*`. Today it turns a `DELETE` that succeeded into a reported failure.

- [ ] **Step 4: Add the three 404 short-circuits**

At `member_resource.go:269`, `monitor_resource.go:275` and `pool_resource.go:290`, a 404 from the parent lookup means the parent is already gone, so the child went with it:

```go
	rootLB, err := rootLBIDFromPool(ctx, client, poolID)
	if err != nil {
		if gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
			return // the pool is gone, so the member went with it
		}
		resp.Diagnostics.AddError("loadbalancer: resolving root load balancer", err.Error())
		return
	}
```

`listener_resource.go` needs no equivalent — it reads `state.LoadbalancerID` with no API call.

- [ ] **Step 5: Update the package doc**

`loadbalancer.go:7-11` says every resource waits for the root to be ACTIVE before and after each mutation. Task 3 falsifies that for the four children's delete paths. Correct the sentence.

- [ ] **Step 6: Unit tests**

Add tests in `internal/services/loadbalancer/` that drive each child's `Delete` against a fake whose root load balancer reports `ERROR`, and assert that the child's `DELETE` **is** issued and that the delete reports no error (a warning is expected). Also test that a root in `PENDING_UPDATE` followed by `ACTIVE` still waits, and that a 404 on the parent lookup returns cleanly.

- [ ] **Step 7: Verify**

```bash
go test ./internal/... -timeout 180s && go vet ./... && golangci-lint run ./... && gofmt -l .
```

- [ ] **Step 8: LAB GATE — verify on the CE lab before Task 5**

Build a load balancer tree on the Community Edition lab, drive the root load balancer into `ERROR`, and run `terraform destroy` on a listener, pool, member and monitor. Confirm each `DELETE` is issued and that Octavia accepts it.

Nothing in this repository proves that Octavia accepts a child `DELETE` while the root is in `ERROR` — the package doc ties the 409 to `PENDING_*` only, and upstream Octavia's immutability check lists only the `PENDING_*` statuses, but that is inference.

**If Octavia refuses with 409: stop.** Task 5 is dropped, the four children move into the known-gaps section of the design document and the changelog, and this task still ships — the relaxation cannot be worse than the pre-wait failure it replaces.

- [ ] **Step 9: Commit**

```
fix(loadbalancer): let a child be deleted while its load balancer is in ERROR

A listener, pool, member or monitor waited for the root load balancer to reach
ACTIVE before issuing its own delete, and gave up when the root reported ERROR.
gophercloud's WaitFor runs its predicate once immediately, so the DELETE was
never sent: a destroy of any child of a broken load balancer failed, and because
Terraform removes children before parents, it also stopped the root's cascade
delete from running.

Delete paths now wait for the load balancer to settle instead. Octavia rejects
changes with 409 only while a load balancer is PENDING_*, so ERROR is a settled
status that still accepts a delete; the child is removed and the operator gets a
warning naming the load balancer to repair. The create path is unchanged and
still refuses to build on a load balancer in ERROR.

Resolving the parent of a member, monitor or pool now treats a 404 as "already
gone" rather than an error, so a child whose parent was cascade-deleted can
still leave state.
```

---
### Task 5a: `pcd_lb_listener`

Part of Task 5. **Do not start until Task 3 is verified on the lab.** Appends to the test file Task 4 created.

**Files:**
**Prerequisites — do not start this task until both have merged.**

- **Task 0** must have added `tfstate.RecordCreated` to `/Users/pushkarmulay/Documents/Claude Code Projects/Terraform Provider/terraform-provider-pcd/.claude/worktrees/inspiring-kilby-0bbd0b/internal/tfstate/tfstate.go` (the ID-guarded promotion of `blockstorage.recordCreated`).
- **Task 3** must have added `waitForLoadBalancerSettled` and `settleBeforeDelete` to `/Users/pushkarmulay/Documents/Claude Code Projects/Terraform Provider/terraform-provider-pcd/.claude/worktrees/inspiring-kilby-0bbd0b/internal/services/loadbalancer/loadbalancer.go`, swapped the listener's two `waitForLoadBalancerActive` calls in `Delete` (lines 304 and 315) to `settleBeforeDelete`, and been lab-verified. **Never ship this task's record without Task 3's destroy-path fix**, in this release or a later one: a recorded listener under a load balancer in `ERROR` is undeletable until that gate is relaxed, which is strictly worse than today's orphan.

#### Modify

`/Users/pushkarmulay/Documents/Claude Code Projects/Terraform Provider/terraform-provider-pcd/.claude/worktrees/inspiring-kilby-0bbd0b/internal/services/loadbalancer/listener_resource.go`

- **Line 28** (import block, pre-edit numbering) — add the `internal/tfstate` import next to `internal/clients`. `fmt` (line 12) and `types` (line 26) are already imported; nothing else in the import block changes.
- **Lines 156–169** — the `Create` tail. Insert the record between `listeners.Create` (156) and the post-create `waitForLoadBalancerActive` (161), and guard the tail at 166–168 so a `notFound` or errored `readInto` cannot overwrite the clean row with the plan's unknowns.

After the edit the file is 387 lines (was 372) and the new anchors are: record at 163–168, post-create wait at 170, guarded tail at 175–186.

#### Do NOT change

- **Line 125** — the *pre*-create `waitForLoadBalancerActive`. It runs before `listeners.Create`, so nothing exists to record. `Create` calls that waiter twice with an identical argument list; the record goes between the create call and the **second** one. This is the single easiest way to get this edit wrong.
- **Lines 272 and 280** (`Update`) — building on a broken load balancer must still fail.
- `readInto` (328–372) — unchanged.

#### Test

`/Users/pushkarmulay/Documents/Claude Code Projects/Terraform Provider/terraform-provider-pcd/.claude/worktrees/inspiring-kilby-0bbd0b/internal/services/loadbalancer/failed_create_internal_test.go` — **new file, 206 lines** (package `loadbalancer`; the existing `loadbalancer_test.go` is package `loadbalancer_test`, so there is no clash). It introduces the load balancer package's first unit test and its `fakeConfig`.

If **Task 4** (root `pcd_lb_loadbalancer`) landed first and already created this file with its own `fakeConfig`, drop the `fakeConfig` function and the `clients`/`gophercloud` imports from the block below and append only `TestListenerCreateKeepsAListenerWhoseLoadBalancerFailed` plus the imports it adds (`encoding/json`, `sync`).

#### What the operator gets, traced

| | Before | After |
|---|---|---|
| Load balancer goes `ERROR` during the listener's create wait | apply fails, **no state**. Terraform has forgotten `lsn-1`. The next apply POSTs a second listener on port 8080; Octavia rejects it as a duplicate port or silently leaves two. The first must be deleted with `openstack loadbalancer listener delete`. | apply fails with the same Octavia message, listener **tainted in state**. `terraform destroy` refreshes it (`provisioning_status = ERROR`), the pre-delete gate warns that the root is in `ERROR` and proceeds, and `DELETE /v2.0/lbaas/listeners/lsn-1` goes out. |
| Apply interrupted (Ctrl-C) during the create wait | same orphan | tainted; the root sits in `PENDING_UPDATE`, the settled gate rides that out, then the delete goes out. |
| `readInto` 404s right after a successful create | unconditional `State.Set` writes a row full of unknowns, which Terraform core rejects outright with "Provider produced inconsistent result after apply" | `AddError` + `RemoveResource`: no row for an object Octavia says is gone. |
| `readInto` errors transiently after a successful create | same unknowns row | the clean `RecordCreated` row stays; Terraform taints it and the next refresh fills it in. |

**Interfaces:**
#### Consumes (must already exist — Tasks 0 and 3)

```go
// internal/tfstate/tfstate.go
func RecordCreated(ctx context.Context, resp *resource.CreateResponse, plan any) bool

// internal/services/loadbalancer/loadbalancer.go
func waitForLoadBalancerSettled(ctx context.Context, client *gophercloud.ServiceClient, lbID string, timeout time.Duration) (string, error)
func settleBeforeDelete(ctx context.Context, client *gophercloud.ServiceClient, lbID, child string, diags *diag.Diagnostics) bool
```

Also consumed, unchanged, from this package: `waitForLoadBalancerActive(ctx, client, lbID, defaultLBTimeout) error`, `defaultLBTimeout = 10 * time.Minute` (loadbalancer.go:89), and the status constants `lbActive`/`lbError` (loadbalancer.go:83–84), which the test uses by name rather than as string literals.

#### Produces

```go
// internal/services/loadbalancer/failed_create_internal_test.go (new)
func fakeConfig(url string) *clients.Config   // shared by every LB unit test; skip if Task 4 already added it
func TestListenerCreateKeepsAListenerWhoseLoadBalancerFailed(t *testing.T)
```

No exported surface changes. `listenerModel`, the schema and `readInto`'s signature `(notFound bool, diags diag.Diagnostics)` are all untouched — `Create` simply stops discarding the `notFound` return with `_`.

#### API contract the fake must honor (verified against gophercloud v2.13.0)

- `openstack.NewLoadBalancerV2` sets `sc.ResourceBase = endpoint + "v2.0/"` (openstack/client.go:457–465), so every path carries a **`/v2.0`** prefix. Do **not** wire the fake through `clients.Config{EndpointOverrides: ...}`: `applyOverride` (internal/clients/config.go:359–364) blanks `ResourceBase` and the prefix silently disappears.
- OK codes (`provider_client.go:561–576`, and `listeners.Create/Get/Delete` all pass `nil *RequestOpts`): `POST {201,202}`, `GET {200}`, `DELETE {202,204}`.
- Paths: `POST|GET|DELETE /v2.0/lbaas/listeners[/{id}]`, `GET /v2.0/lbaas/loadbalancers/{id}`.
- Wrapper keys differ: listener bodies unwrap `{"listener": {...}}` (listeners/results.go:197–204), root load balancer bodies unwrap `{"loadbalancer": {...}}` (loadbalancers/results.go:198–204). `Listener.Loadbalancers` is `[]{"id": "..."}`.
- `gophercloud.WaitFor` (util.go:87–106) runs the predicate **immediately**, before the first tick, then once per second. `defaultLBTimeout` has no injection point, so the test must drive statuses and never rely on the timeout path.

- [ ] **Step 1: Write the failing test**

Add to the test file named above:

```go
// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package loadbalancer

import (
	"context"
	"encoding/json"
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

// fakeConfig points a clients.Config at a test server, so the Octavia client the
// resources build resolves to it. The locator ignores the endpoint options, so it
// answers for every service type and availability. openstack.NewLoadBalancerV2
// sets ResourceBase to the endpoint plus "v2.0/", so every path the fake serves
// carries that prefix; an endpoint_overrides entry would blank ResourceBase and
// drop it.
func fakeConfig(url string) *clients.Config {
	return &clients.Config{
		Region: "region-one",
		Provider: &gophercloud.ProviderClient{
			EndpointLocator: func(gophercloud.EndpointOpts) (string, error) { return url + "/", nil },
		},
	}
}

// A listener whose load balancer enters ERROR while Create waits for it stays in
// Octavia. Create used to return without state, so Terraform forgot the listener,
// the next apply created a second one on the same port, and the first had to be
// deleted by hand. Create must return the error with the listener in state
// (Terraform then taints it), the refresh must report the failure, and the delete
// a destroy runs must remove the listener even though the load balancer is still
// in ERROR - which is why Delete gates on waitForLoadBalancerSettled rather than
// waitForLoadBalancerActive.
func TestListenerCreateKeepsAListenerWhoseLoadBalancerFailed(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	var mu sync.Mutex
	listenerCreated, deleteCalled, lbGetsAfterDelete := false, false, 0
	octavia := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch r.Method + " " + r.URL.Path {
		case "GET /v2.0/lbaas/loadbalancers/lb-1":
			// ACTIVE until the listener exists, so Create clears the pre-create
			// wait and fails on the post-create one. After the DELETE, one
			// PENDING_UPDATE poll proves the settled waiter rides those out
			// instead of returning on the first answer.
			status := lbActive
			switch {
			case deleteCalled:
				lbGetsAfterDelete++
				if lbGetsAfterDelete == 1 {
					status = "PENDING_UPDATE"
				} else {
					status = lbError
				}
			case listenerCreated:
				status = lbError
			}
			fmt.Fprintf(w, `{"loadbalancer": {"id": "lb-1", "name": "web-lb", "provisioning_status": %q,
				"operating_status": "OFFLINE", "admin_state_up": true, "vip_subnet_id": "subnet-1",
				"vip_address": "10.0.0.7", "tags": []}}`, status)
		case "POST /v2.0/lbaas/listeners":
			var body struct {
				Listener struct {
					LoadbalancerID string `json:"loadbalancer_id"`
					Protocol       string `json:"protocol"`
					ProtocolPort   int    `json:"protocol_port"`
					AdminStateUp   *bool  `json:"admin_state_up"`
				} `json:"listener"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decoding the create request: %v", err)
			}
			if body.Listener.LoadbalancerID != "lb-1" || body.Listener.Protocol != "TCP" || body.Listener.ProtocolPort != 8080 {
				t.Errorf("create request sent loadbalancer_id=%q protocol=%q protocol_port=%d; want lb-1, TCP, 8080",
					body.Listener.LoadbalancerID, body.Listener.Protocol, body.Listener.ProtocolPort)
			}
			if body.Listener.AdminStateUp == nil || !*body.Listener.AdminStateUp {
				t.Errorf("create request sent admin_state_up=%v; want true, the schema default", body.Listener.AdminStateUp)
			}
			listenerCreated = true
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{"listener": {"id": "lsn-1", "name": "web", "provisioning_status": "PENDING_CREATE",
				"loadbalancers": [{"id": "lb-1"}]}}`)
		case "GET /v2.0/lbaas/listeners/lsn-1":
			if deleteCalled {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			fmt.Fprint(w, `{"listener": {"id": "lsn-1", "name": "web", "description": "",
				"protocol": "TCP", "protocol_port": 8080, "default_pool_id": "", "connection_limit": -1,
				"admin_state_up": true, "timeout_client_data": 50000, "timeout_member_connect": 5000,
				"timeout_member_data": 50000, "timeout_tcp_inspect": 0, "provisioning_status": "ERROR",
				"operating_status": "OFFLINE", "tags": [], "loadbalancers": [{"id": "lb-1"}]}}`)
		case "DELETE /v2.0/lbaas/listeners/lsn-1":
			deleteCalled = true
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotImplemented)
		}
	}))
	defer octavia.Close()

	r := &listenerResource{config: fakeConfig(octavia.URL)}
	var sch resource.SchemaResponse
	r.Schema(ctx, resource.SchemaRequest{}, &sch)
	s := sch.Schema

	planned := listenerModel{
		ID:             types.StringUnknown(),
		LoadbalancerID: types.StringValue("lb-1"),
		Protocol:       types.StringValue("TCP"),
		ProtocolPort:   types.Int64Value(8080),
		Name:           types.StringValue("web"),
		Description:    types.StringUnknown(),
		DefaultPoolID:  types.StringUnknown(),
		// admin_state_up is the one attribute with a static default, so a create
		// plan carries true rather than unknown. An unknown here serializes as
		// false and the test passes for the wrong reason.
		AdminStateUp:           types.BoolValue(true),
		ConnectionLimit:        types.Int64Unknown(),
		DefaultTLSContainerRef: types.StringNull(),
		SNIContainerRefs:       types.ListNull(types.StringType),
		TimeoutClientData:      types.Int64Unknown(),
		TimeoutMemberConnect:   types.Int64Unknown(),
		TimeoutMemberData:      types.Int64Unknown(),
		TimeoutTCPInspect:      types.Int64Unknown(),
		InsertHeaders:          types.MapNull(types.StringType),
		AllowedCIDRs:           types.ListNull(types.StringType),
		Tags:                   types.SetUnknown(types.StringType),
		ProvisioningStatus:     types.StringUnknown(),
		OperatingStatus:        types.StringUnknown(),
		Region:                 types.StringUnknown(),
	}
	plan := tfsdk.Plan{Schema: s, Raw: tftypes.NewValue(s.Type().TerraformType(ctx), nil)}
	if d := plan.Set(ctx, &planned); d.HasError() {
		t.Fatalf("building the plan: %v", d)
	}

	createResp := resource.CreateResponse{State: tfsdk.State{Schema: s, Raw: tftypes.NewValue(s.Type().TerraformType(ctx), nil)}}
	r.Create(ctx, resource.CreateRequest{Plan: plan}, &createResp)
	if !createResp.Diagnostics.HasError() {
		t.Fatal("create succeeded; want the load balancer's ERROR status reported")
	}
	if createResp.State.Raw.IsNull() {
		t.Fatal("create returned no state: Terraform forgets lsn-1 and the next apply creates a second listener on port 8080")
	}
	if !createResp.State.Raw.IsFullyKnown() {
		t.Fatalf("create state holds unknown values, which Terraform refuses: %v", createResp.State.Raw)
	}
	var got listenerModel
	if d := createResp.State.Get(ctx, &got); d.HasError() {
		t.Fatalf("reading the create state: %v", d)
	}
	if got.ID.ValueString() != "lsn-1" || got.LoadbalancerID.ValueString() != "lb-1" ||
		got.Name.ValueString() != "web" || got.ProtocolPort.ValueInt64() != 8080 {
		t.Fatalf("create state id=%s loadbalancer_id=%s name=%s protocol_port=%d; want lsn-1, lb-1, web, 8080",
			got.ID, got.LoadbalancerID, got.Name, got.ProtocolPort.ValueInt64())
	}

	// terraform destroy (or the replacing apply) refreshes the tainted listener first.
	readResp := resource.ReadResponse{State: createResp.State}
	r.Read(ctx, resource.ReadRequest{State: createResp.State}, &readResp)
	if readResp.Diagnostics.HasError() {
		t.Fatalf("refresh of the failed listener: %v", readResp.Diagnostics)
	}
	if d := readResp.State.Get(ctx, &got); d.HasError() {
		t.Fatalf("reading the refreshed state: %v", d)
	}
	if got.ProvisioningStatus.ValueString() != "ERROR" {
		t.Fatalf("refreshed provisioning_status = %s, want ERROR", got.ProvisioningStatus)
	}

	deleteResp := resource.DeleteResponse{State: readResp.State}
	r.Delete(ctx, resource.DeleteRequest{State: readResp.State}, &deleteResp)
	if deleteResp.Diagnostics.HasError() {
		t.Fatalf("delete of a listener under a load balancer in ERROR: %v", deleteResp.Diagnostics)
	}
	if deleteResp.Diagnostics.WarningsCount() == 0 {
		t.Error("delete raised no warning; want the operator told the load balancer is in ERROR")
	}
	mu.Lock()
	defer mu.Unlock()
	if !deleteCalled {
		t.Fatal("delete never issued DELETE /v2.0/lbaas/listeners/lsn-1: the wait before the delete bailed on the ERROR load balancer, so the destroy fails and the listener can only be removed by hand")
	}
	if lbGetsAfterDelete < 2 {
		t.Fatalf("the wait after the delete returned after %d polls; want it to ride out PENDING_UPDATE so the next resource in the destroy does not hit a 409", lbGetsAfterDelete)
	}
}
```

- [ ] **Step 2: Run it and watch it fail**

Run the focused test for this resource. It must fail because `Create` returns no state — not for a compile error or a fake-server mismatch. If it fails for another reason, fix the test before touching the resource.

- [ ] **Step 3: Write the implementation**

#### Edit 1 — import `internal/tfstate` (listener_resource.go, lines 26–29)

**Before:**

```go
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/platform9/terraform-provider-pcd/internal/clients"
)
```

**After:**

```go
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/platform9/terraform-provider-pcd/internal/clients"
	"github.com/platform9/terraform-provider-pcd/internal/tfstate"
)
```

#### Edit 2 — record before the post-create wait, and guard the tail (listener_resource.go, lines 156–169)

**Before** (the whole tail of `Create`, ending at the closing brace on line 169):

```go
	listener, err := listeners.Create(ctx, client, createOpts).Extract()
	if err != nil {
		resp.Diagnostics.AddError("loadbalancer: creating listener", err.Error())
		return
	}
	if err := waitForLoadBalancerActive(ctx, client, lbID, defaultLBTimeout); err != nil {
		resp.Diagnostics.AddError("loadbalancer: waiting after listener create", err.Error())
		return
	}

	_, readDiags := r.readInto(ctx, client, listener.ID, &plan)
	resp.Diagnostics.Append(readDiags...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}
```

**After:**

```go
	listener, err := listeners.Create(ctx, client, createOpts).Extract()
	if err != nil {
		resp.Diagnostics.AddError("loadbalancer: creating listener", err.Error())
		return
	}

	// Octavia holds the listener from here on, even if the load balancer never
	// returns to ACTIVE, so record it before the wait can fail.
	plan.ID = types.StringValue(listener.ID)
	if !tfstate.RecordCreated(ctx, resp, &plan) {
		return
	}

	if err := waitForLoadBalancerActive(ctx, client, lbID, defaultLBTimeout); err != nil {
		resp.Diagnostics.AddError("loadbalancer: waiting after listener create", err.Error())
		return
	}

	notFound, readDiags := r.readInto(ctx, client, listener.ID, &plan)
	resp.Diagnostics.Append(readDiags...)
	if notFound {
		resp.Diagnostics.AddError("loadbalancer: reading listener after create",
			fmt.Sprintf("Listener %s no longer exists.", listener.ID))
		resp.State.RemoveResource(ctx)
		return
	}
	if resp.Diagnostics.HasError() {
		return // leave the row RecordCreated wrote in place
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}
```

Three things this edit does, each load-bearing:

1. **`plan.ID` is set explicitly.** `Create` never assigns it today — `m.ID` is written only inside `readInto` (line 338), which the failure path by definition never reaches. Without this line `RecordCreated`'s ID guard fires, drops the row and reports a provider bug, which is the intended loud failure rather than an undeletable row.
2. **The record is placed after `listeners.Create`, before the *second* `waitForLoadBalancerActive`.** Line 125 is the *pre*-create wait on the same load balancer with the same arguments; putting the record there would record an object that does not exist.
3. **The tail's `notFound` is no longer discarded and the final `State.Set` is no longer unconditional.** `readInto` returns before assigning anything to the model on both a 404 (line 331–333) and a non-404 error (334–335), so on either path `plan` still holds the create plan's unknowns — `id`, `name`, `description`, `default_pool_id`, `connection_limit`, the four timeouts, `tags`, `provisioning_status`, `operating_status`, `region`. The old unconditional `Set` would write all of those as unknown over the clean row `RecordCreated` just saved, and Terraform core rejects unknowns in post-apply state. The two error kinds get opposite handling on purpose: a 404 means the listener is genuinely gone, so the row must go (`RemoveResource`, matching this resource's own `Read` at lines 185–190); an unreadable-but-existing listener keeps the row so Terraform taints it.

**Explicitly rejected shortcut:** do *not* call `tfstate.NullUnknowns` again after the final `State.Set` as a cheaper substitute for the `notFound` guard. It would make the 404 case "succeed" by writing a row whose every computed attribute is null, asserting an object Octavia says does not exist.

#### Edit 3 — none

`Delete` (lines 290–318) is **not** touched by this task. Its two `waitForLoadBalancerActive` calls at 304 and 315 become `settleBeforeDelete(ctx, client, lbID, "listener", &resp.Diagnostics)` in **Task 3**, which ships first and alone. Post-Task-3, the pre-delete gate returns on `false` and the post-delete gate just records diagnostics, exactly as today.

Post-Task-3 `Delete`, for reference (this is what the test below exercises):

```go
	lbID := state.LoadbalancerID.ValueString()
	if !settleBeforeDelete(ctx, client, lbID, "listener", &resp.Diagnostics) {
		return
	}
	if err := listeners.Delete(ctx, client, state.ID.ValueString()).ExtractErr(); err != nil {
		if gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
			return
		}
		resp.Diagnostics.AddError("loadbalancer: deleting listener", err.Error())
		return
	}
	settleBeforeDelete(ctx, client, lbID, "listener", &resp.Diagnostics)
}
```

- [ ] **Step 4: Run the test and watch it pass**

Run the focused test again. Then the full suite.

- [ ] **Step 5: Verify**

```bash
cd "/Users/pushkarmulay/Documents/Claude Code Projects/Terraform Provider/terraform-provider-pcd/.claude/worktrees/inspiring-kilby-0bbd0b"
```
```bash
gofmt -l ./internal/
```
```bash
go build ./...
```
```bash
go vet ./...
```
```bash
go test ./internal/services/loadbalancer/ -run TestListenerCreateKeepsAListenerWhoseLoadBalancerFailed -v
```
```bash
go test ./internal/... -timeout 120s
```
```bash
golangci-lint run ./internal/services/loadbalancer/ ./internal/tfstate/
```
```bash
# Negative check 1 - revert only the record (drop plan.ID + tfstate.RecordCreated from Create) and confirm the test fails with:
#   failed_create_internal_test.go:162: create returned no state: Terraform forgets lsn-1 and the next apply creates a second listener on port 8080
# Negative check 2 - revert only Task 3's Delete gate (settleBeforeDelete -> waitForLoadBalancerActive) and confirm the test fails with:
#   failed_create_internal_test.go:193: delete of a listener under a load balancer in ERROR: [{waiting for load balancer lb-1 to become ACTIVE: load balancer lb-1 entered ERROR provisioning status loadbalancer: waiting before listener delete}]
# Restore the fix after each check.
```
```bash
# CE lab, after Task 3 has been verified there: build an lb + listener tree, drive the root load balancer into ERROR, then
#   terraform apply   # expect: apply fails, `terraform state list` still shows pcd_lb_listener.test, `terraform show` marks it tainted
#   terraform destroy # expect: the listener DELETE is issued and accepted, no 409, no leftover in `openstack loadbalancer listener list`
```

- [ ] **Step 6: Commit**

```
fix(loadbalancer): keep a listener whose load balancer failed in state

A listener Octavia accepts is kept by Octavia even when the load
balancer then enters ERROR, or when the apply is interrupted while
Create waits for it. Create returned that wait's error without saving
state, so Terraform forgot the listener: the next apply POSTed a second
one on the same port, and the first had to be deleted through the API by
hand. Create now records the listener as soon as listeners.Create
returns, before the wait that can fail. The apply still fails with the
Octavia message, Terraform marks the listener tainted, and the next
apply or a destroy deletes it.

Create never set the listener's ID on the plan - readInto assigns it,
and the failure path never reaches readInto - so the record sets
plan.ID explicitly before calling tfstate.RecordCreated. The attributes
Octavia had not reported when the wait failed are saved as null, since
Terraform refuses unknown values in state, and the next refresh reads
them.

Also guard the read-back at the end of Create, which discarded
readInto's notFound flag and set state unconditionally. readInto returns
without touching the model on both a 404 and a transient error, so that
final Set wrote the create plan's unknowns over the row just recorded,
which Terraform core rejects outright. A 404 now removes the row, an
unreadable listener keeps it, and only a clean read sets state. This was
latent while Create saved no state; recording the listener makes it live.

Depends on the destroy-path fix for the load balancer children: a
recorded listener under a load balancer in ERROR is only destroyable
because Delete gates on waitForLoadBalancerSettled. Do not ship this
without it.

Adds the load balancer package's first unit test and its fake Octavia:
a load balancer that answers ACTIVE until the listener exists and ERROR
afterward, driving Create to fail, Read to refresh to ERROR, and Delete
to issue the listener DELETE and ride out one PENDING_UPDATE poll.
```

**Open questions and traps recorded during planning:**

- Task 3 cannot use this test, and the plan does not currently say what it uses instead. This test needs BOTH the delete gate (Task 3) and the create record (Task 5) to pass - I verified that reverting either one fails it, with the exact diagnostics quoted in the verification commands. Task 3 ships alone and before Task 5, so its own commit has no create record to build state from. Either Task 3 carries a Delete-only test that constructs a tfsdk.State from the schema by hand and calls Delete directly, or Task 3 is lab-verified only and this test is the first automated coverage of settleBeforeDelete. Pick one explicitly; the second is defensible given Task 3 is the lab risk gate anyway, but it should be a decision, not an omission.
- The one real unknown is unchanged and must be settled on the lab before this task ships: does Octavia accept DELETE /v2.0/lbaas/listeners/{id} while the root load balancer sits in ERROR? The package doc at loadbalancer.go:7-11 ties the 409 to PENDING_* only, and upstream Octavia's immutability check lists only the PENDING_* statuses, but nothing in this tree or in gophercloud proves it. My test asserts the provider issues the DELETE; it cannot assert Octavia accepts it. If Octavia refuses with 409, recording the listener would make a whole-tree terraform destroy fail at the first child and never reach the root's cascade - strictly worse than the orphan. If Octavia refuses, DO NOT SHIP this task; pcd_lb_listener goes into the design doc's Known gap section instead.
- settleBeforeDelete's error summary is hardcoded as "loadbalancer: waiting before %s delete", so the post-delete call site at listener_resource.go:315 reports a 'before delete' failure for a wait that ran after it. Harmless (it is an error string, not control flow) and it keeps each call site to one line, but a reviewer will notice. If the plan wants it right, settleBeforeDelete needs a phase argument or two thin wrappers; I did not add one because the settled design specifies the two-line call site verbatim.
- The settled design's test contract says to assert getsAfterDelete >= 2 'where a delete waiter exists'. The listener has no delete waiter on itself - its Delete polls the ROOT load balancer. I substituted lbGetsAfterDelete >= 2 against GET /v2.0/lbaas/loadbalancers/lb-1, with the fake answering PENDING_UPDATE on the first post-delete poll and ERROR after. That proves waitForLoadBalancerSettled genuinely rides out PENDING_* rather than returning on its first answer, which is the property the post-delete wait exists for. It costs about 1s of wall clock, hence t.Parallel(). If the other three children reuse this shape, say so once in the plan rather than per resource.
- The plan should name the corrected call-site count. `grep -rn 'waitForLoadBalancerActive(ctx' internal/services/loadbalancer/` returns 28 lines: 27 calls plus the function's own signature at loadbalancer.go:109. listener_resource.go holds 6 of those calls (125, 161, 272, 280, 304, 315) - the survey's complication lists only four, omitting the two in Update. Task 3 changes exactly 2 of the listener's 6; this task changes none of them.
- Not verifiable by any in-process unit test, and it applies to the five already-shipped resources as much as to this one: UseStateForUnknown is on 7 of this resource's attributes, so a tainted listener's null computed values could in principle be copied into the replacement plan and produce 'Provider produced inconsistent result after apply'. These tests call Create/Read/Delete directly and never go through Terraform core's plan, so they provably cannot catch it. The settled design's single CE lab run against pcd_blockstorage_volume covers the mechanism for all of them; make sure that run actually happens before this lands.

---
### Task 5b: `pcd_lb_pool`

Part of Task 5. **Do not start until Task 3 is verified on the lab.** Appends to the test file Task 4 created.

**Files:**
#### Files

This resource spans **two gated commits**. Commit A is the pool's share of Task 3 (destroy path). Commit B is the pool's share of Task 5 (create record). **Never land B without A** — see `openQuestions` and the commit body. Both assume Task 0 (`tfstate.RecordCreated`) is already merged.

#### Commit A — Task 3, destroy path only (no Create change)

**Modify (shared with listener/member/monitor — land once; skip if a sibling already landed it):**
- `/Users/pushkarmulay/Documents/Claude Code Projects/Terraform Provider/terraform-provider-pcd/.claude/worktrees/inspiring-kilby-0bbd0b/internal/services/loadbalancer/loadbalancer.go`
  - lines **7-11** — package doc: it currently asserts every resource waits for ACTIVE before and after each mutation. After this commit that sentence is false.
  - lines **82-86** — add `lbPending = "PENDING_"` to the existing const block.
  - insert **before line 132** (`// waitForLoadBalancerDeleted blocks until…`) — `waitForLoadBalancerSettled` and `settleBeforeDelete`.

**Modify (pool only):**
- `/Users/pushkarmulay/Documents/Claude Code Projects/Terraform Provider/terraform-provider-pcd/.claude/worktrees/inspiring-kilby-0bbd0b/internal/services/loadbalancer/pool_resource.go`
  - lines **290-294** — 404 short-circuit on `r.rootLBID`.
  - line **295** — pre-delete wait → `settleBeforeDelete(..., "pool", ...)`.
  - line **306** — post-delete wait → `settleBeforeDelete(..., "pool", ...)`.

**Modify (docs, per the settled design's "fix the names in the first commit" rule):**
- `docs/superpowers/specs/2026-09-19-create-state-before-wait-design.md` line **191** — `pcd_loadbalancer_pool` → `pcd_lb_pool` (and :188, :189, :193, :195 for the siblings, if Task 0 has not already done it). Confirmed wrong: `Metadata` at `pool_resource.go:77` registers `req.ProviderTypeName + "_lb_pool"`; docs page is `docs/resources/lb_pool.md`.

#### Commit B — Task 5, create record

**Modify:**
- `/Users/pushkarmulay/Documents/Claude Code Projects/Terraform Provider/terraform-provider-pcd/.claude/worktrees/inspiring-kilby-0bbd0b/internal/services/loadbalancer/pool_resource.go`
  - line **29** — add the `internal/tfstate` import.
  - after line **170** — `plan.ID = types.StringValue(pool.ID)` + `tfstate.RecordCreated`. **Insert between the create at :166 and the wait at :171 — never before the wait at :143, which runs before the pool exists.** Both waits have byte-identical argument lists; a naive search-and-replace hits the wrong one.
  - lines **176-178** — guarded tail (`notFound` is currently discarded with `_` and the final `State.Set` is unconditional).

**Test (new file, greenfield — the package has zero unit tests today):**
- `/Users/pushkarmulay/Documents/Claude Code Projects/Terraform Provider/terraform-provider-pcd/.claude/worktrees/inspiring-kilby-0bbd0b/internal/services/loadbalancer/failed_create_internal_test.go`, package `loadbalancer` (internal). Named for sharing with listener/member/monitor/loadbalancer, matching `internal/services/blockstorage/failed_create_internal_test.go`.
  - **Collision warning:** Task 4 (root `pcd_lb_loadbalancer`) lands in parallel and the settled design assigns `fakeConfig` to it, in this same file. Whichever commit lands second must **drop its own copy of `fakeConfig`** rather than redeclare it. `poolBody` is pool-specific and stays.

**Changelog (`CHANGELOG.md`, under `## [Unreleased]` → `### Fixed`, one bullet, American English):**
```
- `pcd_lb_pool`: a pool now stays in state when the load balancer it belongs to enters `ERROR`
  while the create is waiting for it, or when that apply is interrupted. Octavia keeps the pool
  once the create call returns, so the apply used to fail with nothing in state: Terraform forgot
  the pool, the next apply created a second one, and the first had to be deleted through the API by
  hand. The apply still fails with the load balancer's status, but Terraform marks the pool
  tainted, and the next apply or a destroy deletes it. Destroying a pool no longer requires the
  load balancer to be `ACTIVE` first: the delete now waits only for the load balancer to leave its
  transient `PENDING_*` statuses, warns when it finds it in `ERROR`, and issues the delete anyway.
```
Use `pcd_lb_pool`, not `pcd_loadbalancer_pool`.

**Interfaces:**
#### Interfaces

#### Consumes

From `internal/tfstate` (Task 0, must be merged first):
```go
func RecordCreated(ctx context.Context, resp *resource.CreateResponse, plan any) bool
```
Sets state from `plan`, nulls every unknown, verifies a non-empty root `id` attribute (dropping the row and erroring if absent), and reports whether `Create` should carry on. The caller must assign the object's ID to `plan` first — **`poolResource.Create` never sets `plan.ID` today**; it is assigned only inside `readInto` at `pool_resource.go:337`, which the failure path never reaches.

Existing, unchanged, in package `loadbalancer`:
```go
func isSet(v types.String) bool                                             // loadbalancer_resource.go:312-314; false for null, unknown AND ""
func (r *poolResource) rootLBID(ctx, client *gophercloud.ServiceClient, m *poolModel) (string, error)  // pool_resource.go:317-325
func rootLBIDFromListener(ctx, client *gophercloud.ServiceClient, listenerID string) (string, error)   // loadbalancer.go:157-166
func waitForLoadBalancerActive(ctx, client *gophercloud.ServiceClient, lbID string, timeout time.Duration) error  // loadbalancer.go:109-130
const defaultLBTimeout = 10 * time.Minute                                   // loadbalancer.go:89
```

#### Produces

New in `internal/services/loadbalancer/loadbalancer.go` (Commit A; shared with the three sibling children):
```go
const lbPending = "PENDING_"

func waitForLoadBalancerSettled(ctx context.Context, client *gophercloud.ServiceClient, lbID string, timeout time.Duration) (string, error)
func settleBeforeDelete(ctx context.Context, client *gophercloud.ServiceClient, lbID, child string, diags *diag.Diagnostics) bool
```
`waitForLoadBalancerSettled` returns the status the load balancer settled on (`ACTIVE`, `ERROR`, `DELETED`, or any unrecognized non-`PENDING_*` value; `DELETED` on a 404). `settleBeforeDelete` returns `true` when the caller should carry on, raising a warning — not an error — for `ERROR`. `diag.Diagnostics.AddWarning` has a pointer receiver and `Append` skips duplicates, so the pre- and post-delete calls emit one warning between them.

New test helpers in `internal/services/loadbalancer/failed_create_internal_test.go`:
```go
func fakeConfig(url string) *clients.Config   // shared with Tasks 4 and 5's sibling tests
func poolBody(provisioningStatus, listeners, loadbalancers string) string
```

#### Behavior contract

`poolResource.Create` still returns an error when the load balancer does not reach `ACTIVE`. What changes is that `resp.State` is non-null and fully known when it does, carrying `id`, the configured one of `loadbalancer_id`/`listener_id`, and null for every attribute Octavia has not reported. `poolResource.Delete` no longer requires `ACTIVE`.

- [ ] **Step 1: Write the failing test**

Add to the test file named above:

```go
// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package loadbalancer

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

// fakeConfig points a clients.Config at a test server, so the Octavia client the
// resources build resolves to it. The locator ignores the endpoint options, so it
// answers for every service type and availability. The client is built by
// openstack.NewLoadBalancerV2, which appends the "v2.0/" resource base, so every
// path below carries the /v2.0 prefix; wiring the fake through EndpointOverrides
// instead would blank that base (internal/clients/config.go) and every path would
// 404.
func fakeConfig(url string) *clients.Config {
	return &clients.Config{
		Region: "region-one",
		Provider: &gophercloud.ProviderClient{
			EndpointLocator: func(gophercloud.EndpointOpts) (string, error) { return url + "/", nil },
		},
	}
}

// poolBody is the fake's pool object. Octavia wraps it in a "pool" key, and
// gophercloud's Extract yields a nil *Pool if the wrapper is missing.
func poolBody(provisioningStatus, listeners, loadbalancers string) string {
	return fmt.Sprintf(`{"pool": {
		"id": "pool-1", "name": "pool-1", "description": "",
		"protocol": "HTTP", "lb_algorithm": "ROUND_ROBIN",
		"admin_state_up": true, "project_id": "proj-1", "healthmonitor_id": "",
		"provisioning_status": %q, "operating_status": "OFFLINE",
		"listeners": %s, "loadbalancers": %s, "tags": []}}`,
		provisioningStatus, listeners, loadbalancers)
}

// A pool whose load balancer goes to ERROR stays in Octavia: the create call has
// already returned it. Create used to return the wait's error without state, so
// Terraform forgot the pool, the next apply created another, and the first had to
// be deleted by hand. Create must return the error with the pool in state
// (Terraform then taints it), and the refresh and delete a destroy runs must
// remove the pool even though the load balancer is still in ERROR, which is what
// settleBeforeDelete, in place of the old waitForLoadBalancerActive gate, makes
// possible.
func TestPoolCreateKeepsAPoolWhoseLoadBalancerFailed(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	var mu sync.Mutex
	poolCreated, deleteCalled := false, false
	lbGetsAfterDelete := 0
	octavia := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch r.Method + " " + r.URL.Path {
		case "GET /v2.0/lbaas/loadbalancers/lb-1":
			// ACTIVE lets the pre-create wait through; the pool's create then
			// drives the load balancer to ERROR, which is where the post-create
			// wait gives up and where every later wait finds it.
			status := "ACTIVE"
			if poolCreated {
				status = "ERROR"
			}
			if deleteCalled {
				lbGetsAfterDelete++
			}
			fmt.Fprintf(w, `{"loadbalancer": {"id": "lb-1", "provisioning_status": %q}}`, status)
		case "POST /v2.0/lbaas/pools":
			poolCreated = true
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, poolBody("PENDING_CREATE", `[]`, `[{"id": "lb-1"}]`))
		case "GET /v2.0/lbaas/pools/pool-1":
			if deleteCalled {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			fmt.Fprint(w, poolBody("ERROR", `[]`, `[{"id": "lb-1"}]`))
		case "DELETE /v2.0/lbaas/pools/pool-1":
			deleteCalled = true
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotImplemented)
		}
	}))
	defer octavia.Close()

	r := &poolResource{config: fakeConfig(octavia.URL)}
	var sch resource.SchemaResponse
	r.Schema(ctx, resource.SchemaRequest{}, &sch)
	s := sch.Schema

	// Exactly one of loadbalancer_id / listener_id must pass isSet, which is
	// false for unknown, so the unconfigured one stays unknown and the
	// configured one is a known, non-empty string.
	planned := poolModel{
		ID:                 types.StringUnknown(),
		Name:               types.StringValue("pool-1"),
		Description:        types.StringUnknown(),
		Protocol:           types.StringValue("HTTP"),
		LBMethod:           types.StringValue("ROUND_ROBIN"),
		LoadbalancerID:     types.StringValue("lb-1"),
		ListenerID:         types.StringUnknown(),
		AdminStateUp:       types.BoolValue(true),
		Persistence:        types.ObjectNull(poolPersistenceAttrTypes),
		Tags:               types.SetUnknown(types.StringType),
		ProjectID:          types.StringUnknown(),
		MonitorID:          types.StringUnknown(),
		ProvisioningStatus: types.StringUnknown(),
		OperatingStatus:    types.StringUnknown(),
		Region:             types.StringUnknown(),
	}
	plan := tfsdk.Plan{Schema: s, Raw: tftypes.NewValue(s.Type().TerraformType(ctx), nil)}
	if d := plan.Set(ctx, &planned); d.HasError() {
		t.Fatalf("building the plan: %v", d)
	}

	createResp := resource.CreateResponse{State: tfsdk.State{Schema: s, Raw: tftypes.NewValue(s.Type().TerraformType(ctx), nil)}}
	r.Create(ctx, resource.CreateRequest{Plan: plan}, &createResp)
	if !createResp.Diagnostics.HasError() {
		t.Fatal("create succeeded; want the load balancer's ERROR status reported")
	}
	if createResp.State.Raw.IsNull() {
		t.Fatal("create returned no state: Terraform forgets pool-1 and the next apply creates a second pool")
	}
	if !createResp.State.Raw.IsFullyKnown() {
		t.Fatalf("create state holds unknown values, which Terraform refuses: %v", createResp.State.Raw)
	}
	var got poolModel
	if d := createResp.State.Get(ctx, &got); d.HasError() {
		t.Fatalf("reading the create state: %v", d)
	}
	if got.ID.ValueString() != "pool-1" {
		t.Fatalf("create state id = %s, want pool-1", got.ID)
	}
	// The configured parent has to survive the record: Delete resolves the load
	// balancer from it, and nulling both would make the pool undeletable.
	if got.LoadbalancerID.ValueString() != "lb-1" {
		t.Fatalf("create state loadbalancer_id = %s, want lb-1", got.LoadbalancerID)
	}
	if !got.ListenerID.IsNull() {
		t.Fatalf("create state listener_id = %s, want null", got.ListenerID)
	}
	if got.Name.ValueString() != "pool-1" || got.Protocol.ValueString() != "HTTP" ||
		got.LBMethod.ValueString() != "ROUND_ROBIN" || !got.AdminStateUp.ValueBool() {
		t.Fatalf("create state name=%s protocol=%s lb_method=%s admin_state_up=%v; want pool-1, HTTP, ROUND_ROBIN, true",
			got.Name, got.Protocol, got.LBMethod, got.AdminStateUp)
	}

	// terraform destroy (or the replacing apply) refreshes the tainted pool first.
	readResp := resource.ReadResponse{State: createResp.State}
	r.Read(ctx, resource.ReadRequest{State: createResp.State}, &readResp)
	if readResp.Diagnostics.HasError() {
		t.Fatalf("refresh of the failed pool: %v", readResp.Diagnostics)
	}
	if readResp.State.Raw.IsNull() {
		t.Fatal("refresh dropped pool-1 from state")
	}
	if d := readResp.State.Get(ctx, &got); d.HasError() {
		t.Fatalf("reading the refreshed state: %v", d)
	}
	if got.ProvisioningStatus.ValueString() != "ERROR" {
		t.Fatalf("refreshed provisioning_status = %s, want ERROR", got.ProvisioningStatus)
	}
	if got.Region.ValueString() != "region-one" {
		t.Fatalf("refreshed region = %s, want region-one", got.Region)
	}

	deleteResp := resource.DeleteResponse{State: readResp.State}
	r.Delete(ctx, resource.DeleteRequest{State: readResp.State}, &deleteResp)
	if deleteResp.Diagnostics.HasError() {
		t.Fatalf("delete of a pool whose load balancer is in ERROR: %v", deleteResp.Diagnostics)
	}
	if deleteResp.Diagnostics.WarningsCount() == 0 {
		t.Fatal("delete raised no warning; want the ERROR load balancer reported")
	}
	mu.Lock()
	defer mu.Unlock()
	if !deleteCalled {
		t.Fatal("delete never called DELETE /v2.0/lbaas/pools/pool-1: the pre-delete wait rejected the ERROR load balancer")
	}
	if lbGetsAfterDelete < 1 {
		t.Fatalf("delete polled the load balancer %d times after the DELETE; want the post-delete settle to run", lbGetsAfterDelete)
	}
}

// The same failure for a pool attached to a listener rather than directly to the
// load balancer. This shape costs an extra listeners.Get on both Create and
// Delete, and it is the one where the record could go wrong: NullUnknowns turns
// the unconfigured loadbalancer_id into a known null, and rootLBID must still
// resolve the load balancer through listener_id.
func TestPoolOnAListenerCreateKeepsAPoolWhoseLoadBalancerFailed(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	var mu sync.Mutex
	poolCreated, deleteCalled := false, false
	listenerGets := 0
	octavia := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch r.Method + " " + r.URL.Path {
		case "GET /v2.0/lbaas/listeners/lis-1":
			listenerGets++
			fmt.Fprint(w, `{"listener": {"id": "lis-1", "loadbalancers": [{"id": "lb-1"}]}}`)
		case "GET /v2.0/lbaas/loadbalancers/lb-1":
			status := "ACTIVE"
			if poolCreated {
				status = "ERROR"
			}
			fmt.Fprintf(w, `{"loadbalancer": {"id": "lb-1", "provisioning_status": %q}}`, status)
		case "POST /v2.0/lbaas/pools":
			poolCreated = true
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, poolBody("PENDING_CREATE", `[{"id": "lis-1"}]`, `[]`))
		case "GET /v2.0/lbaas/pools/pool-1":
			if deleteCalled {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			fmt.Fprint(w, poolBody("ERROR", `[{"id": "lis-1"}]`, `[]`))
		case "DELETE /v2.0/lbaas/pools/pool-1":
			deleteCalled = true
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotImplemented)
		}
	}))
	defer octavia.Close()

	r := &poolResource{config: fakeConfig(octavia.URL)}
	var sch resource.SchemaResponse
	r.Schema(ctx, resource.SchemaRequest{}, &sch)
	s := sch.Schema

	planned := poolModel{
		ID:                 types.StringUnknown(),
		Name:               types.StringValue("pool-1"),
		Description:        types.StringUnknown(),
		Protocol:           types.StringValue("HTTP"),
		LBMethod:           types.StringValue("ROUND_ROBIN"),
		LoadbalancerID:     types.StringUnknown(),
		ListenerID:         types.StringValue("lis-1"),
		AdminStateUp:       types.BoolValue(true),
		Persistence:        types.ObjectNull(poolPersistenceAttrTypes),
		Tags:               types.SetUnknown(types.StringType),
		ProjectID:          types.StringUnknown(),
		MonitorID:          types.StringUnknown(),
		ProvisioningStatus: types.StringUnknown(),
		OperatingStatus:    types.StringUnknown(),
		Region:             types.StringUnknown(),
	}
	plan := tfsdk.Plan{Schema: s, Raw: tftypes.NewValue(s.Type().TerraformType(ctx), nil)}
	if d := plan.Set(ctx, &planned); d.HasError() {
		t.Fatalf("building the plan: %v", d)
	}

	createResp := resource.CreateResponse{State: tfsdk.State{Schema: s, Raw: tftypes.NewValue(s.Type().TerraformType(ctx), nil)}}
	r.Create(ctx, resource.CreateRequest{Plan: plan}, &createResp)
	if !createResp.Diagnostics.HasError() {
		t.Fatal("create succeeded; want the load balancer's ERROR status reported")
	}
	if createResp.State.Raw.IsNull() || !createResp.State.Raw.IsFullyKnown() {
		t.Fatalf("create state is null or holds unknown values: %v", createResp.State.Raw)
	}
	var got poolModel
	if d := createResp.State.Get(ctx, &got); d.HasError() {
		t.Fatalf("reading the create state: %v", d)
	}
	if got.ID.ValueString() != "pool-1" {
		t.Fatalf("create state id = %s, want pool-1", got.ID)
	}
	if got.ListenerID.ValueString() != "lis-1" {
		t.Fatalf("create state listener_id = %s, want lis-1", got.ListenerID)
	}
	if !got.LoadbalancerID.IsNull() {
		t.Fatalf("create state loadbalancer_id = %s, want null", got.LoadbalancerID)
	}

	readResp := resource.ReadResponse{State: createResp.State}
	r.Read(ctx, resource.ReadRequest{State: createResp.State}, &readResp)
	if readResp.Diagnostics.HasError() {
		t.Fatalf("refresh of the failed pool: %v", readResp.Diagnostics)
	}

	mu.Lock()
	listenerGetsBeforeDelete := listenerGets
	mu.Unlock()

	deleteResp := resource.DeleteResponse{State: readResp.State}
	r.Delete(ctx, resource.DeleteRequest{State: readResp.State}, &deleteResp)
	if deleteResp.Diagnostics.HasError() {
		t.Fatalf("delete of a listener-attached pool whose load balancer is in ERROR: %v", deleteResp.Diagnostics)
	}
	mu.Lock()
	defer mu.Unlock()
	if !deleteCalled {
		t.Fatal("delete never called DELETE /v2.0/lbaas/pools/pool-1")
	}
	if listenerGets <= listenerGetsBeforeDelete {
		t.Fatal("delete never re-read the listener: rootLBID no longer resolves the load balancer through listener_id")
	}
}
```

- [ ] **Step 2: Run it and watch it fail**

Run the focused test for this resource. It must fail because `Create` returns no state — not for a compile error or a fake-server mismatch. If it fails for another reason, fix the test before touching the resource.

- [ ] **Step 3: Write the implementation**

#### Implementation

Every "Before" block below is quoted verbatim from the worktree. Line numbers are pre-edit unless stated.

---

#### Commit A, edit 1 — package doc, `internal/services/loadbalancer/loadbalancer.go:7-11`

**Before:**
```go
// Octavia serializes changes per load balancer: after any create/update/delete of
// the load balancer or one of its children, the root load balancer enters a
// PENDING_* provisioning status and the API rejects further changes with HTTP 409
// until it returns to ACTIVE. Every resource therefore waits for the root load
// balancer to be ACTIVE before and after each mutation, using waitForLoadBalancerActive.
package loadbalancer
```

**After:**
```go
// Octavia serializes changes per load balancer: after any create/update/delete of
// the load balancer or one of its children, the root load balancer enters a
// PENDING_* provisioning status and the API rejects further changes with HTTP 409
// until it leaves that status. Creates and updates wait for the root load
// balancer to be ACTIVE before and after each mutation, using
// waitForLoadBalancerActive, since building on a broken load balancer should
// fail. The children's deletes instead wait with waitForLoadBalancerSettled,
// which treats ERROR as settled, so a child of a load balancer that went to
// ERROR stays destroyable.
package loadbalancer
```

---

#### Commit A, edit 2 — the `PENDING_` prefix constant, `internal/services/loadbalancer/loadbalancer.go:82-86`

**Before:**
```go
// Octavia provisioning-status values. The gophercloud package exposes these only
// as documentation, not as exported constants, so they are declared here.
const (
	lbActive  = "ACTIVE"
	lbError   = "ERROR"
	lbDeleted = "DELETED"
)
```

**After:**
```go
// Octavia provisioning-status values. The gophercloud package exposes these only
// as documentation, not as exported constants, so they are declared here.
const (
	lbActive  = "ACTIVE"
	lbError   = "ERROR"
	lbDeleted = "DELETED"
	// lbPending prefixes the three transient statuses PENDING_CREATE,
	// PENDING_UPDATE and PENDING_DELETE, the only ones during which Octavia
	// rejects a change with HTTP 409.
	lbPending = "PENDING_"
)
```

---

#### Commit A, edit 3 — the settled waiter, inserted **before** `internal/services/loadbalancer/loadbalancer.go:132`

**Before** (line 130 is the close of `waitForLoadBalancerActive`; line 132 starts the next doc comment):
```go
	if err != nil {
		return fmt.Errorf("waiting for load balancer %s to become ACTIVE: %w", lbID, err)
	}
	return nil
}

// waitForLoadBalancerDeleted blocks until the load balancer is gone (404). Used
// after a cascade delete of the root load balancer.
```

**After:**
```go
	if err != nil {
		return fmt.Errorf("waiting for load balancer %s to become ACTIVE: %w", lbID, err)
	}
	return nil
}

// waitForLoadBalancerSettled blocks until the load balancer leaves its transient
// PENDING_* statuses and reports the status it settled on. Octavia rejects
// changes with HTTP 409 only while the load balancer is PENDING_* (see the
// package doc), so ERROR is a settled status that still accepts a child delete.
// Delete paths use this rather than waitForLoadBalancerActive: a child recorded
// by a create whose wait gave up on an ERROR root must stay destroyable. A load
// balancer that is already gone counts as settled, since its children went with
// it. An unrecognized status counts as settled too, so a destroy proceeds to a
// loud, retryable 409 instead of polling out the full timeout.
func waitForLoadBalancerSettled(ctx context.Context, client *gophercloud.ServiceClient, lbID string, timeout time.Duration) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	status := ""
	err := gophercloud.WaitFor(ctx, func(ctx context.Context) (bool, error) {
		lb, err := loadbalancers.Get(ctx, client, lbID).Extract()
		if err != nil {
			if gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
				status = lbDeleted
				return true, nil
			}
			return false, err
		}
		status = lb.ProvisioningStatus
		return !strings.HasPrefix(lb.ProvisioningStatus, lbPending), nil
	})
	if err != nil {
		return status, fmt.Errorf("waiting for load balancer %s to settle (last status %q): %w", lbID, status, err)
	}
	return status, nil
}

// settleBeforeDelete waits for the load balancer to accept changes again and
// reports whether the caller should carry on. A root in ERROR is warned about,
// not failed: the child is deleted anyway, and if Octavia refuses the request
// the delete call reports that itself. child names the resource being deleted
// ("pool", "listener", ...), for the diagnostic.
func settleBeforeDelete(ctx context.Context, client *gophercloud.ServiceClient, lbID, child string, diags *diag.Diagnostics) bool {
	status, err := waitForLoadBalancerSettled(ctx, client, lbID, defaultLBTimeout)
	if err != nil {
		diags.AddError(fmt.Sprintf("loadbalancer: waiting to delete the %s", child), err.Error())
		return false
	}
	if status == lbError {
		diags.AddWarning("Load balancer in ERROR provisioning status",
			fmt.Sprintf("Load balancer %s is in ERROR provisioning status. The %s delete proceeds anyway. "+
				"If Octavia refuses the request, repair or delete the load balancer and retry the destroy.", lbID, child))
	}
	return true
}

// waitForLoadBalancerDeleted blocks until the load balancer is gone (404). Used
// after a cascade delete of the root load balancer.
```

No new imports: `strings`, `net/http`, `time`, `fmt`, `context`, `gophercloud`, `loadbalancers` and `diag` are all already imported at `loadbalancer.go:14-29`.

---

#### Commit A, edit 4 — the pool's Delete, `internal/services/loadbalancer/pool_resource.go:290-308`

**Before:**
```go
	rootLB, err := r.rootLBID(ctx, client, &state)
	if err != nil {
		resp.Diagnostics.AddError("loadbalancer: resolving root load balancer", err.Error())
		return
	}
	if err := waitForLoadBalancerActive(ctx, client, rootLB, defaultLBTimeout); err != nil {
		resp.Diagnostics.AddError("loadbalancer: waiting before pool delete", err.Error())
		return
	}
	if err := pools.Delete(ctx, client, state.ID.ValueString()).ExtractErr(); err != nil {
		if gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
			return
		}
		resp.Diagnostics.AddError("loadbalancer: deleting pool", err.Error())
		return
	}
	if err := waitForLoadBalancerActive(ctx, client, rootLB, defaultLBTimeout); err != nil {
		resp.Diagnostics.AddError("loadbalancer: waiting after pool delete", err.Error())
	}
}
```

**After:**
```go
	rootLB, err := r.rootLBID(ctx, client, &state)
	if err != nil {
		if gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
			// The listener is gone, so the pool went with it. Returning with no
			// diagnostics drops the pool from state, which is the right outcome.
			return
		}
		resp.Diagnostics.AddError("loadbalancer: resolving root load balancer", err.Error())
		return
	}
	if !settleBeforeDelete(ctx, client, rootLB, "pool", &resp.Diagnostics) {
		return
	}
	if err := pools.Delete(ctx, client, state.ID.ValueString()).ExtractErr(); err != nil {
		if gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
			return
		}
		resp.Diagnostics.AddError("loadbalancer: deleting pool", err.Error())
		return
	}
	settleBeforeDelete(ctx, client, rootLB, "pool", &resp.Diagnostics)
}
```

Notes for the reviewer:
- **Why both waits change.** The post-delete wait exists so the next resource in the destroy does not hit a 409. `waitForLoadBalancerSettled` serves that identically: it never returns while `PENDING_*`. Today, if the load balancer lands in `ERROR` right after a `DELETE` that succeeded, line 306 turns that success into a reported failure and Terraform keeps a pool that no longer exists, recoverable only via the 404 short-circuit at :300-302 on the next destroy.
- **The 404 short-circuit is defense in depth, not an active deadlock fix.** Under default refresh, `Read` maps `notFound` to `RemoveResource` (`pool_resource.go:194-200`) and the pool is dropped before `Delete` runs. It is reachable only under `-refresh=false`, where `r.rootLBID` → `rootLBIDFromListener` → `listeners.Get` returns 404 for a cascade-deleted listener and `Delete` dies resolving the parent. `gophercloud.ResponseCodeIs` uses `errors.As` and `rootLBIDFromListener` returns the error unwrapped, so the match works.
- Nothing on the create path changes in this commit. `waitForLoadBalancerActive` keeps failing on `ERROR` at :143, :171, :259 and :267.

---

#### Commit B, edit 1 — the import, `internal/services/loadbalancer/pool_resource.go:29-30`

**Before:**
```go
	"github.com/platform9/terraform-provider-pcd/internal/clients"
)
```

**After:**
```go
	"github.com/platform9/terraform-provider-pcd/internal/clients"
	"github.com/platform9/terraform-provider-pcd/internal/tfstate"
)
```

---

#### Commit B, edit 2 — record the pool, and guard the tail, `internal/services/loadbalancer/pool_resource.go:166-179`

**Before:**
```go
	pool, err := pools.Create(ctx, client, createOpts).Extract()
	if err != nil {
		resp.Diagnostics.AddError("loadbalancer: creating pool", err.Error())
		return
	}
	if err := waitForLoadBalancerActive(ctx, client, rootLB, defaultLBTimeout); err != nil {
		resp.Diagnostics.AddError("loadbalancer: waiting after pool create", err.Error())
		return
	}

	_, readDiags := r.readInto(ctx, client, pool.ID, &plan)
	resp.Diagnostics.Append(readDiags...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}
```

**After** (post-edit lines 167-197):
```go
	pool, err := pools.Create(ctx, client, createOpts).Extract()
	if err != nil {
		resp.Diagnostics.AddError("loadbalancer: creating pool", err.Error())
		return
	}

	plan.ID = types.StringValue(pool.ID)
	// Octavia keeps the pool from here on, even when the load balancer it belongs
	// to goes to ERROR and the wait below gives up, so record it first.
	if !tfstate.RecordCreated(ctx, resp, &plan) {
		return
	}

	if err := waitForLoadBalancerActive(ctx, client, rootLB, defaultLBTimeout); err != nil {
		resp.Diagnostics.AddError("loadbalancer: waiting after pool create", err.Error())
		return
	}

	notFound, readDiags := r.readInto(ctx, client, pool.ID, &plan)
	resp.Diagnostics.Append(readDiags...)
	if notFound {
		resp.Diagnostics.AddError("loadbalancer: reading pool after create",
			fmt.Sprintf("Pool %s no longer exists.", pool.ID))
		resp.State.RemoveResource(ctx)
		return
	}
	if resp.Diagnostics.HasError() {
		return // keep the row recorded above: the pool exists but could not be read
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}
```

Notes for the reviewer:
- **`plan.ID` must be set explicitly.** `poolResource.Create` never assigns it; `m.ID` is written only inside `readInto` at :337, past the wait. Without this line `RecordCreated`'s ID guard drops the row and errors, which is loud but is not the fix.
- **The tail guard is not cosmetic.** `readInto` returns early on both 404 (:330-332) and a non-404 error (:333-334) *without assigning anything to the model*, so `plan` still carries the create plan's unknowns. The old unconditional `State.Set` at :178 would overwrite the clean, unknown-free row this change just recorded with a state full of unknowns, which Terraform core rejects outright. Latent today because there is no state to clobber; live the moment the record lands.
- **The two branches are deliberately opposite.** `notFound` means the pool is proven gone, so state is removed; a read error means the pool exists but could not be read, so the recorded row stays and Terraform taints it. This matches `Read`'s own branch at :194-200.
- **Do not** substitute a second `tfstate.NullUnknowns` after the final `State.Set` as a cheaper alternative to the `notFound` guard. It would make the `notFound` case "succeed" by writing a row whose every computed attribute is null. Considered and rejected.

---

#### What the operator gets, traced

| Branch | Today | After |
|---|---|---|
| Load balancer goes to `ERROR` during the post-create wait | Apply fails, pool exists in Octavia, nothing in state. Next apply creates a second pool on the same load balancer. The first must be deleted through the API. | Apply fails with the same message, pool is in state and tainted. `terraform destroy` or the next apply settles on the `ERROR` load balancer, issues the `DELETE`, and removes it. |
| `terraform destroy -target=pcd_lb_pool.x` with the load balancer in `ERROR` | Fails at the pre-delete wait; the `DELETE` is never issued; fails identically on every retry. | Warns that the load balancer is in `ERROR`, issues the `DELETE`, succeeds. |
| Whole-tree `terraform destroy` with the load balancer in `ERROR` | Succeeds only because no pool is in state; the root's `Cascade: true` delete (`loadbalancer_resource.go:258`, no pre-wait) clears the tree. | Still succeeds. The pool's own delete now goes out first and is accepted, and the cascade covers whatever is left. **This is exactly why Commit A must precede Commit B: with the record and without the relaxation, the destroy fails at the child and never reaches the cascade.** |
| Apply interrupted (Ctrl-C) or the 10-minute timeout | Pool exists in `PENDING_CREATE`, nothing in state. | Pool in state and tainted. The load balancer is `PENDING_UPDATE`, so the destroy's settle waits it out rather than failing. Known gap: if Octavia never settles, the destroy times out. |

- [ ] **Step 4: Run the test and watch it pass**

Run the focused test again. Then the full suite.

- [ ] **Step 5: Verify**

```bash
cd '/Users/pushkarmulay/Documents/Claude Code Projects/Terraform Provider/terraform-provider-pcd/.claude/worktrees/inspiring-kilby-0bbd0b' && gofmt -l ./internal
```
```bash
cd '/Users/pushkarmulay/Documents/Claude Code Projects/Terraform Provider/terraform-provider-pcd/.claude/worktrees/inspiring-kilby-0bbd0b' && go build ./...
```
```bash
cd '/Users/pushkarmulay/Documents/Claude Code Projects/Terraform Provider/terraform-provider-pcd/.claude/worktrees/inspiring-kilby-0bbd0b' && go vet ./internal/services/loadbalancer/...
```
```bash
cd '/Users/pushkarmulay/Documents/Claude Code Projects/Terraform Provider/terraform-provider-pcd/.claude/worktrees/inspiring-kilby-0bbd0b' && go test ./internal/services/loadbalancer/ -run 'TestPool' -v -count=1
```
```bash
cd '/Users/pushkarmulay/Documents/Claude Code Projects/Terraform Provider/terraform-provider-pcd/.claude/worktrees/inspiring-kilby-0bbd0b' && go test ./internal/services/loadbalancer/ -run 'TestPool' -race -count=1
```
```bash
cd '/Users/pushkarmulay/Documents/Claude Code Projects/Terraform Provider/terraform-provider-pcd/.claude/worktrees/inspiring-kilby-0bbd0b' && go test ./... -count=1
```
```bash
cd '/Users/pushkarmulay/Documents/Claude Code Projects/Terraform Provider/terraform-provider-pcd/.claude/worktrees/inspiring-kilby-0bbd0b' && grep -rn 'pcd_loadbalancer_' CHANGELOG.md internal/ examples/ templates/   # must print nothing. Do NOT sweep docs/: both design documents mention the wrong name on purpose, in the passages that say to correct it.
```
```bash
cd '/Users/pushkarmulay/Documents/Claude Code Projects/Terraform Provider/terraform-provider-pcd/.claude/worktrees/inspiring-kilby-0bbd0b' && grep -c 'waitForLoadBalancerActive(' internal/services/loadbalancer/pool_resource.go   # expect 4 after commit A, was 6
```
```bash
NEGATIVE CHECK 1 (run in a throwaway copy, expect FAIL 'create returned no state'): delete the plan.ID + tfstate.RecordCreated block from pool_resource.go Create, then go test ./internal/services/loadbalancer/ -run TestPool -count=1
```
```bash
NEGATIVE CHECK 2 (run in a throwaway copy, expect FAIL 'delete of a pool whose load balancer is in ERROR'): revert both settleBeforeDelete calls in pool_resource.go Delete to waitForLoadBalancerActive, then go test ./internal/services/loadbalancer/ -run TestPool -count=1
```
```bash
CE LAB GATE for commit A (risk gate for the whole load balancer half; run BEFORE commit B): build a pcd_lb_loadbalancer + pcd_lb_listener + pcd_lb_pool tree, drive the load balancer into ERROR, then 'openstack loadbalancer pool delete <id>' and a 'terraform destroy -target=pcd_lb_pool.test'. Confirm the DELETE is issued and Octavia accepts it. If Octavia answers 409, DO NOT SHIP commit B: the pool goes into the design doc's Known gap section instead.
```
```bash
CE LAB CHECK for commit B: apply a tree whose pool create fails, confirm the pool is in state and tainted, then re-apply and confirm no 'Provider produced inconsistent result after apply' error (UseStateForUnknown is on id/name/description/project_id/monitor_id/provisioning_status/operating_status/region/tags and on loadbalancer_id/listener_id via forceNewC). Finally 'terraform destroy' the whole tree and confirm it succeeds.
```

- [ ] **Step 6: Commit**

```
Two commits, in this order. Commit A must merge (and pass the lab check) before Commit B.

=== Commit A (Task 3, destroy path) ===

fix(loadbalancer): let a child delete proceed on an ERROR load balancer

Destroying a listener, pool, member or health monitor first waited for the
load balancer it belongs to to reach ACTIVE, and that wait fails on its very
first poll when the load balancer is in ERROR. The DELETE was never issued, so
`terraform destroy -target` on the child failed the same way on every retry,
and the only way out was to repair or cascade-delete the load balancer through
the API by hand.

Octavia rejects a change with HTTP 409 only while the load balancer is in one
of its transient PENDING_* statuses, so ERROR is a status that still accepts a
child delete. Add waitForLoadBalancerSettled, which blocks only for PENDING_*
and reports the status it settled on, and settleBeforeDelete, which warns about
an ERROR load balancer and carries on rather than failing. If Octavia does
refuse the request after all, the delete call reports that itself, which is no
worse than the wait failing and comes with a better message. Use them for both
the pre- and post-delete waits in the four child resources: the post-delete
wait only exists so the next resource in the destroy does not hit a 409, which
the settled wait serves identically, and today it turns a DELETE that succeeded
into a reported failure that leaves a pool that no longer exists in state.

Also handle a 404 when a child resolves its load balancer through its parent
during Delete, so a child whose parent has already been cascade-deleted is
dropped from state instead of erroring. Under the default refresh the child's
Read removes it first; this covers -refresh=false.

Nothing on the create or update path changes: building on a broken load
balancer should still fail, so those keep waiting for ACTIVE.

This stands on its own, and it is a prerequisite for recording a child in state
when its create wait gives up. Terraform destroys children before parents, so a
child in state whose delete cannot proceed also stops the load balancer's own
cascade delete, which is what cleans up the tree today. If the two changes land
in different releases, never ship the create record without this one.

=== Commit B (Task 5, create record) ===

fix(loadbalancer): keep a pool whose load balancer fails to settle

Octavia keeps a pool from the moment the create call returns. Create then
waited for the load balancer to return to ACTIVE and returned the wait's error
without saving state, so an apply that hit a load balancer in ERROR, timed out,
or was interrupted left a pool that Terraform had no record of. The next apply
created a second pool on the same load balancer and the first had to be deleted
through the API by hand.

Record the pool as soon as pools.Create returns, before the wait. The apply
still fails with the load balancer's status, but Terraform marks the pool
tainted and the next apply or a destroy deletes it. The attributes Octavia has
not reported yet are saved as null, since Terraform refuses unknown values in
state, and the next refresh reads them.

Guard the read-back at the end of Create as well. It discarded the not-found
flag and set state unconditionally, so a pool that could not be read wrote the
create plan's unknown values straight back over the recorded row. It now
reports a pool that has vanished and drops it from state, keeps the recorded
row when the pool exists but cannot be read, and sets state only on success.

Add the package's first unit tests, covering a pool attached directly to a load
balancer and one attached through a listener, since the second resolves the
load balancer through an extra API call that the recorded state must not break.
```

**Open questions and traps recorded during planning:**

- The settled design contradicts itself on the rootLBID 404 short-circuit. One graft says to keep it at pool_resource.go:290 as cheap coverage for -refresh=false; a later graft says to drop it and record it as a known gap because the children's Read maps notFound to RemoveResource before Delete ever runs. I verified the refresh-order claim (Read at pool_resource.go:194-200 removes the resource on 404), so the short-circuit really is unreachable under default refresh. I kept it anyway: it is five lines, it is correct, and -refresh=false is a real flag. If the reviewer prefers the stricter reading, drop that hunk from Commit A; nothing else in this section depends on it.
- UNVERIFIED SERVER BEHAVIOR, and it gates Commit B: nothing in this repo or in gophercloud proves that Octavia accepts a DELETE on a pool whose load balancer is in ERROR. The package doc at loadbalancer.go:7-11 only establishes the other half (409 during PENDING_*). The relaxation in Commit A cannot be worse than what it replaces either way: if Octavia refuses, the child's DELETE returns 409, Delete AddErrors and keeps state, which is the same user-visible outcome as today's pre-wait failure with a better message. But Commit B is different: recording the pool and then hitting a 409 on the child delete would make a whole-tree terraform destroy fail at the first child and never reach the root's cascade, which is strictly worse than the orphan. Run the lab gate, and if Octavia refuses, DO NOT SHIP Commit B.
- settleBeforeDelete's diagnostic wording differs from the settled design's literal text. The design specifies AddError('loadbalancer: waiting before %s delete') and a warning saying 'Deleting the %s anyway', but the same helper is called at both the pre- and post-delete sites, where 'before' and 'Deleting ... anyway' would be wrong. I made both strings phase-neutral ('waiting to delete the %s', 'The %s delete proceeds anyway'). The alternative is a phase argument, which costs a line at all eight call sites. Flagging so the four child resources use one wording.
- The design's test obligation 'getsAfterDelete >= 2' does not apply to this resource. There is no waitForPoolDeleted anywhere in the package; the pool's post-delete wait polls the LOAD BALANCER, and on an ERROR load balancer waitForLoadBalancerSettled returns on its first poll by design. The equivalent assertion here is lbGetsAfterDelete >= 1, which proves the post-delete settle ran. The >= 2 poll-past-ERROR assertion belongs to Task 1 (zone) and Task 4 (root load balancer), whose waiters do latch.
- Test-file ownership collides with Task 4. The settled design assigns fakeConfig to Task 4 (root load balancer), in this same internal/services/loadbalancer/failed_create_internal_test.go, and Tasks 4 and 5 are meant to be independent. Whichever lands second must delete its own copy of fakeConfig rather than redeclare it. Worth resolving in the plan by assigning fakeConfig to Commit A (the destroy-path commit, which lands first and could carry a settle-waiter test) instead of to either create task.
- Two factual corrections to the inputs. (a) The survey says 'four sibling resources share this shape: listener, member, monitor' and then names three; the pool has three siblings, and the four LB child resources are listener/pool/member/monitor. (b) The survey says the pre-delete-wait decision also applies to loadbalancer_resource.go:143; it does not. The root load balancer has no pre-delete wait at all (Delete at loadbalancer_resource.go:244-268 goes straight to the Cascade:true delete at :258), and line 143 is the root's own post-create wait, which is Task 4's create-record bug, not a delete guard. The pre-delete-wait relaxation applies to exactly four call sites: pool_resource.go:295, listener_resource.go:304, member_resource.go:274, monitor_resource.go:280.
- Waiter call-site count for the package doc: `grep -rn 'waitForLoadBalancerActive(' internal/services/loadbalancer/` returns 28 lines, but one of those is the function declaration at loadbalancer.go:109, so there are 27 calls, not 28 and not the 24 two designs assert. This commit takes the pool from 6 occurrences to 4; all four children together will take the package to 19 calls.
- Two claims in the survey I could not source, neither of which the fix depends on. 'Octavia typically cascades the pool to provisioning_status ERROR' is an assertion about server behavior with no backing; it does not matter here because waitForLoadBalancerActive never issues a pools.Get and the provider learns nothing about the pool's own status during the wait. And 'persistence is the one attribute NullUnknowns provably never touches' is wrong in general: an Optional non-Computed attribute can be unknown in a plan when its value derives from an unresolved reference, and NullUnknowns would null it. Harmless on an error path (Terraform skips the post-apply consistency check when the provider returns an error), but do not write 'provably never' into a comment.
- The post-delete settle can emit a second, identical ERROR warning. diag.Diagnostics.Append skips duplicates (terraform-plugin-framework v1.19.0 diag/diagnostics.go), so the operator sees one, and the test asserts WarningsCount() > 0 rather than an exact count. Noting it in case a reviewer expects one warning per call site.

---
### Task 5c: `pcd_lb_member`

Part of Task 5. **Do not start until Task 3 is verified on the lab.** Appends to the test file Task 4 created.

**Files:**
#### Files

**Prerequisites — do not start this task until both have merged**

- **Task 0** adds `RecordCreated` to `/Users/pushkarmulay/Documents/Claude Code Projects/Terraform Provider/terraform-provider-pcd/.claude/worktrees/inspiring-kilby-0bbd0b/internal/tfstate/tfstate.go` (promoted from `blockstorage.recordCreated`, plus the ID guard).
- **Task 3** adds `lbPending`, `waitForLoadBalancerSettled` and `settleBeforeDelete` to `/Users/pushkarmulay/Documents/Claude Code Projects/Terraform Provider/terraform-provider-pcd/.claude/worktrees/inspiring-kilby-0bbd0b/internal/services/loadbalancer/loadbalancer.go`, and swaps the four children's delete gates. **Task 3's member-specific edits are shown in the diff below (edits 3 and 4) because this section must be executable on its own — apply them in the Task 3 commit, not this one.**

**Modify** — `/Users/pushkarmulay/Documents/Claude Code Projects/Terraform Provider/terraform-provider-pcd/.claude/worktrees/inspiring-kilby-0bbd0b/internal/services/loadbalancer/member_resource.go`

| Edit | Task | Lines (pre-edit) | What |
|---|---|---|---|
| 1 | 5 | 28–29 | add the `internal/tfstate` import |
| 2 | 5 | 146–153 | record the member between `pools.CreateMember` (:141) and the post-create wait (:146); guard the read-back tail (:151–153) |
| 3 | 3 | 269–273 | 404 short-circuit in `rootLBIDFromPool`'s error branch |
| 4 | 3 | 274–277, 285–287 | pre-delete gate → `settleBeforeDelete`; post-delete wait → `waitForLoadBalancerSettled` |

Line numbers are against `d44f1f6` on `pushkar/create-state-before-wait` and were re-verified; the survey's `createCallLine 141`, `waitCallLine 146` and `deleteFnLine 255` are all exact.

**Modify (Task 3, shared)** — `/Users/pushkarmulay/Documents/Claude Code Projects/Terraform Provider/terraform-provider-pcd/.claude/worktrees/inspiring-kilby-0bbd0b/internal/services/loadbalancer/loadbalancer.go`: `lbPending` into the const block at 82–86, the two new functions before `waitForLoadBalancerDeleted` at :132, and the package doc at :10–11 (it currently claims *every* resource waits for ACTIVE before and after each mutation — false once the four delete gates move).

**Create (test)** — `/Users/pushkarmulay/Documents/Claude Code Projects/Terraform Provider/terraform-provider-pcd/.claude/worktrees/inspiring-kilby-0bbd0b/internal/services/loadbalancer/failed_create_internal_test.go`

This is the load balancer package's **first unit test** and its first `fakeConfig`. The only existing test file, `internal/services/loadbalancer/loadbalancer_test.go`, is `package loadbalancer_test` and `TF_ACC`-gated; the new file is `package loadbalancer` (internal) and the two coexist. If Task 4 (root load balancer) lands first it will have created `failed_create_internal_test.go` and `fakeConfig` already — then **append** the two test functions and drop the `fakeConfig` helper from this file.

**Docs** — the design doc's `pcd_loadbalancer_member` at `docs/superpowers/specs/2026-09-19-create-state-before-wait-design.md:193` is not a registered type name. `Metadata` at `member_resource.go:65` registers `pcd_lb_member`. Task 0 corrects all five LB names in the doc; never copy the wrong name into a changelog bullet.

#### What the operator gets, traced

| Branch | Today | After |
|---|---|---|
| Root LB goes to ERROR during the post-create wait | Apply fails, member invisible to Terraform. Next apply POSTs the same `address`+`protocol_port` into the same pool and Octavia rejects it; the orphan must be deleted through the API. | Apply still fails with the same message, member is in state and tainted. Next apply or a destroy deletes it. |
| `terraform destroy` of the tree while the root is in ERROR | **Already fails today**, at the pool's own pre-delete gate (`pool_resource.go:296`) — the root's `Cascade: true` delete at `loadbalancer_resource.go:258` is never reached. The survey's "recording state trades an orphan for a failed destroy" is wrong: the member's siblings block the destroy first. | The four children's gates no longer fail on ERROR, so the DELETEs go out and the destroy completes. This is a **pre-existing destroy blocker being fixed**, not regression cover. |
| Create wait ends on timeout or Ctrl-C with the root PENDING_* | Member orphaned. | Member in state; by destroy time the root has normally settled, so the destroy works. Pure improvement. |
| Read-back after a successful wait returns 404 | Unconditional `State.Set` writes a row full of unknowns, which Terraform core rejects. | `AddError` + `RemoveResource` — nothing is left pointing at an object that is gone. |
| Read-back returns 5xx | Same unknown-value rejection. | Early return; the `RecordCreated` row stays, Terraform taints it. |

**Interfaces:**
#### Interfaces

#### Consumes (all must exist before this task compiles)

From Task 0, `internal/tfstate`:

```go
func RecordCreated(ctx context.Context, resp *resource.CreateResponse, plan any) bool
```

From Task 3, `internal/services/loadbalancer/loadbalancer.go`:

```go
const lbPending = "PENDING_" // prefixes PENDING_CREATE, PENDING_UPDATE, PENDING_DELETE

func waitForLoadBalancerSettled(ctx context.Context, client *gophercloud.ServiceClient, lbID string, timeout time.Duration) (string, error)

func settleBeforeDelete(ctx context.Context, client *gophercloud.ServiceClient, lbID, child string, diags *diag.Diagnostics) bool
```

Already in the package, unchanged:

```go
func waitForLoadBalancerActive(ctx context.Context, client *gophercloud.ServiceClient, lbID string, timeout time.Duration) error
func rootLBIDFromPool(ctx context.Context, client *gophercloud.ServiceClient, poolID string) (string, error)
const defaultLBTimeout = 10 * time.Minute
func (r *memberResource) readInto(ctx context.Context, client *gophercloud.ServiceClient, poolID, memberID string, m *memberModel) (notFound bool, diags diag.Diagnostics)
```

#### Produces

```go
// internal/services/loadbalancer/failed_create_internal_test.go, package loadbalancer
func fakeConfig(url string) *clients.Config
func TestMemberCreateKeepsAMemberWhoseLoadBalancerFailed(t *testing.T)
func TestMemberCreateReadBackFailures(t *testing.T)
```

No exported provider API changes. The schema is untouched, so no state upgrade and no doc regeneration.

#### Shared-fake contract for Tasks 4 and 5 (pin these, they are the traps)

- `openstack.NewLoadBalancerV2` (gophercloud `openstack/client.go:457-465`) sets `sc.ResourceBase = endpoint + "v2.0/"`, so **every** fake path starts `/v2.0/`. Copying blockstorage's bare `POST /volumes` shape gives a 404 and the test fails for the wrong reason.
- Never build the fake through `EndpointOverrides`: `internal/clients/config.go:362` blanks `ResourceBase` and silently drops the `/v2.0/` prefix.
- Status codes (gophercloud `provider_client.go:563-572`, all member calls pass `nil` RequestOpts): GET **200 only**, POST **201 or 202**, DELETE **202 or 204**. A POST answering 200 fails.
- `pools.Member.UnmarshalJSON` (`pools/results.go:326-342`) parses `created_at`/`updated_at` with `JSONRFC3339NoZ` — layout `2006-01-02T15:04:05`, no fallback. Emit them without a trailing `Z`, or omit them entirely. `pools.Pool` embeds `Members []Member` (`pools/results.go:70`), so the same trap fires through a pool body carrying a `members` array — the fakes below omit it.
- `gophercloud.WaitFor` (`util.go:87-90`) runs the predicate once immediately before starting its 1-second ticker, so a fake that answers a terminal status on the first GET ends the wait with no sleep. Only the deliberate PENDING_DELETE poll costs a second, which is why both tests carry `t.Parallel()`.
- Wrapper keys are singular: `"member"`, `"pool"`, `"loadbalancer"`.

- [ ] **Step 1: Write the failing test**

Add to the test file named above:

```go
// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package loadbalancer

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

// fakeConfig points a clients.Config at a test server, so the Octavia client the
// resources build resolves to it. The locator ignores the endpoint options, so
// it answers for every service type and availability. openstack.NewLoadBalancerV2
// appends "v2.0/" to the endpoint, so every path the fake serves starts /v2.0/.
// The config must not go through EndpointOverrides, which blanks ResourceBase and
// drops that prefix.
func fakeConfig(url string) *clients.Config {
	return &clients.Config{
		Region: "region-one",
		Provider: &gophercloud.ProviderClient{
			EndpointLocator: func(gophercloud.EndpointOpts) (string, error) { return url + "/", nil },
		},
	}
}

// A member Octavia accepted stays in the pool even when the root load balancer
// then goes to ERROR and the create wait gives up. Create used to return without
// state, so Terraform forgot the member, the next apply POSTed the same
// address and port into the same pool, and the orphan had to be deleted by hand.
// Create must return the error with the member in state (Terraform then taints
// it), the refresh must report the failure, and the delete a destroy runs must
// issue DELETE even though the root load balancer is still in ERROR.
func TestMemberCreateKeepsAMemberWhoseLoadBalancerFailed(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	var mu sync.Mutex
	lbGets, lbGetsAfterDelete, deleteCalled := 0, 0, false
	octavia := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch r.Method + " " + r.URL.Path {
		case "GET /v2.0/lbaas/pools/pool-1":
			// Only "loadbalancers" is consumed, by rootLBIDFromPool. No
			// "members" array: pools.Member parses created_at/updated_at only
			// as RFC3339 with no zone, so a nested member would be a trap.
			fmt.Fprint(w, `{"pool": {"id": "pool-1", "name": "p", "protocol": "TCP",
				"lb_algorithm": "ROUND_ROBIN", "admin_state_up": true,
				"loadbalancers": [{"id": "lb-1"}], "listeners": []}}`)
		case "GET /v2.0/lbaas/loadbalancers/lb-1":
			// Poll 1 is the wait before the create, and answers ACTIVE so the
			// create goes out. Every later poll answers ERROR, which is what
			// the wait after the create gives up on. After the DELETE, the
			// first poll answers PENDING_DELETE, so the post-delete wait has
			// to poll again rather than return on the first answer.
			status := lbActive
			if lbGets > 0 {
				status = lbError
			}
			lbGets++
			if deleteCalled {
				lbGetsAfterDelete++
				if lbGetsAfterDelete == 1 {
					status = "PENDING_DELETE"
				}
			}
			fmt.Fprintf(w, `{"loadbalancer": {"id": "lb-1", "name": "lb", "admin_state_up": true,
				"provisioning_status": %q, "operating_status": "OFFLINE"}}`, status)
		case "POST /v2.0/lbaas/pools/pool-1/members":
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{"member": {"id": "mem-1", "address": "10.120.0.10", "protocol_port": 8080,
				"name": "", "weight": 1, "subnet_id": "sub-1", "admin_state_up": true, "backup": false,
				"monitor_address": "", "monitor_port": 0, "provisioning_status": "PENDING_CREATE",
				"operating_status": "OFFLINE", "tags": []}}`)
		case "GET /v2.0/lbaas/pools/pool-1/members/mem-1":
			fmt.Fprint(w, `{"member": {"id": "mem-1", "address": "10.120.0.10", "protocol_port": 8080,
				"name": "", "weight": 1, "subnet_id": "sub-1", "admin_state_up": true, "backup": false,
				"monitor_address": "", "monitor_port": 0, "provisioning_status": "ERROR",
				"operating_status": "NO_MONITOR", "tags": []}}`)
		case "DELETE /v2.0/lbaas/pools/pool-1/members/mem-1":
			deleteCalled = true
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotImplemented)
		}
	}))
	defer octavia.Close()

	r := &memberResource{config: fakeConfig(octavia.URL)}
	var sch resource.SchemaResponse
	r.Schema(ctx, resource.SchemaRequest{}, &sch)
	s := sch.Schema

	// admin_state_up is the only attribute with a Default, so it is the only
	// Optional+Computed one Terraform knows in a first-apply plan.
	planned := memberModel{
		ID:                 types.StringUnknown(),
		PoolID:             types.StringValue("pool-1"),
		Address:            types.StringValue("10.120.0.10"),
		ProtocolPort:       types.Int64Value(8080),
		Name:               types.StringUnknown(),
		Weight:             types.Int64Unknown(),
		SubnetID:           types.StringValue("sub-1"),
		AdminStateUp:       types.BoolValue(true),
		Backup:             types.BoolUnknown(),
		MonitorAddress:     types.StringUnknown(),
		MonitorPort:        types.Int64Unknown(),
		Tags:               types.SetUnknown(types.StringType),
		ProvisioningStatus: types.StringUnknown(),
		OperatingStatus:    types.StringUnknown(),
		Region:             types.StringUnknown(),
	}
	plan := tfsdk.Plan{Schema: s, Raw: tftypes.NewValue(s.Type().TerraformType(ctx), nil)}
	if d := plan.Set(ctx, &planned); d.HasError() {
		t.Fatalf("building the plan: %v", d)
	}

	createResp := resource.CreateResponse{State: tfsdk.State{Schema: s, Raw: tftypes.NewValue(s.Type().TerraformType(ctx), nil)}}
	r.Create(ctx, resource.CreateRequest{Plan: plan}, &createResp)
	if !createResp.Diagnostics.HasError() {
		t.Fatal("create succeeded; want the load balancer's ERROR status reported")
	}
	if createResp.State.Raw.IsNull() {
		t.Fatal("create returned no state: Terraform forgets mem-1 and the next apply creates a second member")
	}
	if !createResp.State.Raw.IsFullyKnown() {
		t.Fatalf("create state holds unknown values, which Terraform refuses: %v", createResp.State.Raw)
	}
	var got memberModel
	if d := createResp.State.Get(ctx, &got); d.HasError() {
		t.Fatalf("reading the create state: %v", d)
	}
	if got.ID.ValueString() != "mem-1" {
		t.Fatalf("create state id = %s, want mem-1; a row without one names nothing Delete could remove", got.ID)
	}
	// pool_id and id are what Delete resolves the root load balancer and the
	// member from, so both have to survive the null-out of unknown values.
	if got.PoolID.ValueString() != "pool-1" || got.Address.ValueString() != "10.120.0.10" || got.ProtocolPort.ValueInt64() != 8080 {
		t.Fatalf("create state pool_id=%s address=%s protocol_port=%d; want pool-1, 10.120.0.10, 8080",
			got.PoolID, got.Address, got.ProtocolPort.ValueInt64())
	}

	// terraform destroy (or the replacing apply) refreshes the tainted member first.
	readResp := resource.ReadResponse{State: createResp.State}
	r.Read(ctx, resource.ReadRequest{State: createResp.State}, &readResp)
	if readResp.Diagnostics.HasError() {
		t.Fatalf("refresh of the failed member: %v", readResp.Diagnostics)
	}
	if d := readResp.State.Get(ctx, &got); d.HasError() {
		t.Fatalf("reading the refreshed state: %v", d)
	}
	if got.ProvisioningStatus.ValueString() != "ERROR" {
		t.Fatalf("refreshed provisioning_status = %s, want ERROR", got.ProvisioningStatus)
	}

	deleteResp := resource.DeleteResponse{State: readResp.State}
	r.Delete(ctx, resource.DeleteRequest{State: readResp.State}, &deleteResp)
	if deleteResp.Diagnostics.HasError() {
		t.Fatalf("delete of a member whose load balancer is in ERROR: %v", deleteResp.Diagnostics)
	}
	if deleteResp.Diagnostics.WarningsCount() != 1 {
		t.Fatalf("delete diagnostics = %v; want one warning naming the load balancer in ERROR", deleteResp.Diagnostics)
	}
	mu.Lock()
	defer mu.Unlock()
	if !deleteCalled {
		t.Fatal("delete never called DELETE /v2.0/lbaas/pools/pool-1/members/mem-1: the gate before it must not fail on an ERROR root")
	}
	if lbGetsAfterDelete < 2 {
		t.Fatalf("the wait after the delete returned after %d polls; want it to wait out PENDING_DELETE so the next resource does not get a 409", lbGetsAfterDelete)
	}
}

// Create reads the member back after the wait succeeds. That read can still
// fail, and what Create does then decides what is left in state. If the member
// is gone the recorded row has to go with it; if it is only unreadable the row
// has to stay, and the unknown values the read left on the plan must never
// reach state, which Terraform rejects outright.
func TestMemberCreateReadBackFailures(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		readStatus int
		wantState  bool
	}{
		{name: "the member is gone", readStatus: http.StatusNotFound, wantState: false},
		{name: "the member cannot be read", readStatus: http.StatusInternalServerError, wantState: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			var mu sync.Mutex
			octavia := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				switch r.Method + " " + r.URL.Path {
				case "GET /v2.0/lbaas/pools/pool-1":
					fmt.Fprint(w, `{"pool": {"id": "pool-1", "name": "p", "protocol": "TCP",
						"lb_algorithm": "ROUND_ROBIN", "admin_state_up": true,
						"loadbalancers": [{"id": "lb-1"}], "listeners": []}}`)
				case "GET /v2.0/lbaas/loadbalancers/lb-1":
					// Both waits pass: this test is about the read after them.
					fmt.Fprint(w, `{"loadbalancer": {"id": "lb-1", "name": "lb", "admin_state_up": true,
						"provisioning_status": "ACTIVE", "operating_status": "ONLINE"}}`)
				case "POST /v2.0/lbaas/pools/pool-1/members":
					w.WriteHeader(http.StatusCreated)
					fmt.Fprint(w, `{"member": {"id": "mem-1", "address": "10.120.0.10", "protocol_port": 8080,
						"name": "", "weight": 1, "subnet_id": "sub-1", "admin_state_up": true, "backup": false,
						"monitor_address": "", "monitor_port": 0, "provisioning_status": "PENDING_CREATE",
						"operating_status": "OFFLINE", "tags": []}}`)
				case "GET /v2.0/lbaas/pools/pool-1/members/mem-1":
					w.WriteHeader(tc.readStatus)
				default:
					t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
					w.WriteHeader(http.StatusNotImplemented)
				}
			}))
			defer octavia.Close()

			r := &memberResource{config: fakeConfig(octavia.URL)}
			var sch resource.SchemaResponse
			r.Schema(ctx, resource.SchemaRequest{}, &sch)
			s := sch.Schema

			planned := memberModel{
				ID:                 types.StringUnknown(),
				PoolID:             types.StringValue("pool-1"),
				Address:            types.StringValue("10.120.0.10"),
				ProtocolPort:       types.Int64Value(8080),
				Name:               types.StringUnknown(),
				Weight:             types.Int64Unknown(),
				SubnetID:           types.StringValue("sub-1"),
				AdminStateUp:       types.BoolValue(true),
				Backup:             types.BoolUnknown(),
				MonitorAddress:     types.StringUnknown(),
				MonitorPort:        types.Int64Unknown(),
				Tags:               types.SetUnknown(types.StringType),
				ProvisioningStatus: types.StringUnknown(),
				OperatingStatus:    types.StringUnknown(),
				Region:             types.StringUnknown(),
			}
			plan := tfsdk.Plan{Schema: s, Raw: tftypes.NewValue(s.Type().TerraformType(ctx), nil)}
			if d := plan.Set(ctx, &planned); d.HasError() {
				t.Fatalf("building the plan: %v", d)
			}

			createResp := resource.CreateResponse{State: tfsdk.State{Schema: s, Raw: tftypes.NewValue(s.Type().TerraformType(ctx), nil)}}
			r.Create(ctx, resource.CreateRequest{Plan: plan}, &createResp)
			if !createResp.Diagnostics.HasError() {
				t.Fatal("create succeeded; want the failed read reported")
			}
			if !tc.wantState {
				if !createResp.State.Raw.IsNull() {
					t.Fatalf("create kept state for a member Octavia says is gone: %v", createResp.State.Raw)
				}
				return
			}
			if createResp.State.Raw.IsNull() {
				t.Fatal("create dropped the member although it still exists in Octavia")
			}
			if !createResp.State.Raw.IsFullyKnown() {
				t.Fatalf("create state holds unknown values, which Terraform refuses: %v", createResp.State.Raw)
			}
			var got memberModel
			if d := createResp.State.Get(ctx, &got); d.HasError() {
				t.Fatalf("reading the create state: %v", d)
			}
			if got.ID.ValueString() != "mem-1" {
				t.Fatalf("create state id = %s, want mem-1", got.ID)
			}
		})
	}
}
```

- [ ] **Step 2: Run it and watch it fail**

Run the focused test for this resource. It must fail because `Create` returns no state — not for a compile error or a fake-server mismatch. If it fails for another reason, fix the test before touching the resource.

- [ ] **Step 3: Write the implementation**

#### Implementation

#### Edit 1 — add the `tfstate` import (Task 5)

`internal/services/loadbalancer/member_resource.go`, lines 28–29.

**Before:**

```go
	"github.com/platform9/terraform-provider-pcd/internal/clients"
)
```

**After:**

```go
	"github.com/platform9/terraform-provider-pcd/internal/clients"
	"github.com/platform9/terraform-provider-pcd/internal/tfstate"
)
```

`types` and `fmt` are already imported (lines 26 and 12), so no other import changes.

---

#### Edit 2 — record before the wait, guard the tail (Task 5)

`internal/services/loadbalancer/member_resource.go`, lines 146–153, at the end of `Create`.

**Before** (lines 141–154, quoted in full for context — the edit replaces 146–153):

```go
	member, err := pools.CreateMember(ctx, client, poolID, createOpts).Extract()
	if err != nil {
		resp.Diagnostics.AddError("loadbalancer: creating member", err.Error())
		return
	}
	if err := waitForLoadBalancerActive(ctx, client, rootLB, defaultLBTimeout); err != nil {
		resp.Diagnostics.AddError("loadbalancer: waiting after member create", err.Error())
		return
	}

	_, readDiags := r.readInto(ctx, client, poolID, member.ID, &plan)
	resp.Diagnostics.Append(readDiags...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}
```

**After** (becomes lines 142–171):

```go
	member, err := pools.CreateMember(ctx, client, poolID, createOpts).Extract()
	if err != nil {
		resp.Diagnostics.AddError("loadbalancer: creating member", err.Error())
		return
	}
	// Octavia holds the member from here on, even if the root load balancer
	// never comes back to ACTIVE, so record it before waiting on the root.
	plan.ID = types.StringValue(member.ID)
	if !tfstate.RecordCreated(ctx, resp, &plan) {
		return
	}

	if err := waitForLoadBalancerActive(ctx, client, rootLB, defaultLBTimeout); err != nil {
		resp.Diagnostics.AddError("loadbalancer: waiting after member create", err.Error())
		return
	}

	notFound, readDiags := r.readInto(ctx, client, poolID, member.ID, &plan)
	resp.Diagnostics.Append(readDiags...)
	if notFound {
		resp.Diagnostics.AddError("loadbalancer: reading member after create",
			fmt.Sprintf("Member %s no longer exists.", member.ID))
		resp.State.RemoveResource(ctx)
		return
	}
	if resp.Diagnostics.HasError() {
		return // leave the row RecordCreated wrote in place
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}
```

Three things that are easy to get wrong here:

1. **`plan.ID` must be set explicitly.** `memberModel.ID` is assigned only inside `readInto` (`member_resource.go:310` pre-edit), which the failure path never reaches. Without this line `RecordCreated`'s ID guard drops the row and reports a provider bug — correct, but the member is still orphaned.
2. **The record goes after the *second* wait's predecessor, not the first.** `Create` calls `waitForLoadBalancerActive` twice with an identical argument list, at :117 and :146. The record belongs between `pools.CreateMember` (:141) and the **:146** call. Putting it before :117 records an object that does not exist yet.
3. **The tail's `notFound` was discarded with `_` and the final `Set` was unconditional.** `readInto` returns before assigning anything to the model on both a 404 and a non-404 error (`member_resource.go:302-308`), so `plan` still carries the create plan's unknowns — `name`, `weight`, `backup`, `monitor_address`, `monitor_port`, `tags`, `provisioning_status`, `operating_status`, `region`. Today that is latent because there is no state to clobber; once `RecordCreated` lands it is live, and Terraform core rejects unknown values in state outright. The verification section below shows the exact rejected value.

Do **not** substitute a second `tfstate.NullUnknowns` call after the final `Set` as a cheaper alternative. It would make the `notFound` case "succeed" by writing a row whose every computed attribute is null for an object that no longer exists.

Nothing on the create path's waiter changes: `waitForLoadBalancerActive` keeps failing on ERROR at :117 and :146 and in `Update`. Building on a broken load balancer should still fail.

---

#### Edit 3 — 404 short-circuit on parent resolution (Task 3)

`internal/services/loadbalancer/member_resource.go`, lines 269–273 in `Delete`.

**Before:**

```go
	poolID := state.PoolID.ValueString()
	rootLB, err := rootLBIDFromPool(ctx, client, poolID)
	if err != nil {
		resp.Diagnostics.AddError("loadbalancer: resolving root load balancer", err.Error())
		return
	}
```

**After:**

```go
	poolID := state.PoolID.ValueString()
	rootLB, err := rootLBIDFromPool(ctx, client, poolID)
	if err != nil {
		if gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
			// The pool is gone, so the member went with it. Returning with no
			// diagnostics drops the resource from state, which is the outcome
			// a destroy wants: there is nothing left to delete.
			return
		}
		resp.Diagnostics.AddError("loadbalancer: resolving root load balancer", err.Error())
		return
	}
```

`rootLBIDFromPool` returns `pools.Get`'s error unwrapped (`loadbalancer.go:171-174`) and `gophercloud.ResponseCodeIs` uses `errors.As`, so this matches. `gophercloud` and `net/http` are already imported (lines 15 and 13). Write the bare `return` deliberately: a `Delete` that returns with no diagnostics drops the resource from state, and an engineer with no context will be tempted to add an error there. Under default refresh this branch is unreachable — `Read` maps `notFound` to `RemoveResource` (`member_resource.go:169-175`) and the resource is dropped before `Delete` runs — so this is defense-in-depth for `-refresh=false`, not an active deadlock. See open question 2.

---

#### Edit 4 — the two delete waits (Task 3)

`internal/services/loadbalancer/member_resource.go`, lines 274–277 and 285–287.

**Before (274–277):**

```go
	if err := waitForLoadBalancerActive(ctx, client, rootLB, defaultLBTimeout); err != nil {
		resp.Diagnostics.AddError("loadbalancer: waiting before member delete", err.Error())
		return
	}
```

**After:**

```go
	if !settleBeforeDelete(ctx, client, rootLB, "member", &resp.Diagnostics) {
		return
	}
```

**Before (285–287):**

```go
	if err := waitForLoadBalancerActive(ctx, client, rootLB, defaultLBTimeout); err != nil {
		resp.Diagnostics.AddError("loadbalancer: waiting after member delete", err.Error())
	}
```

**After:**

```go
	if _, err := waitForLoadBalancerSettled(ctx, client, rootLB, defaultLBTimeout); err != nil {
		resp.Diagnostics.AddError("loadbalancer: waiting after member delete", err.Error())
	}
```

Both waits change, not only the pre-delete one. The post-delete wait exists so the *next* resource in the destroy does not hit a 409; `waitForLoadBalancerSettled` serves that identically, because it never returns while the root is PENDING_*. Today it turns a DELETE that succeeded into a reported failure whenever the root is in ERROR, which leaves a non-existent member in state (self-healing only via the 404 short-circuit at :279-281).

The post-delete site calls `waitForLoadBalancerSettled` directly rather than `settleBeforeDelete` for two reasons: the summary must read "waiting **after** member delete", and the pre-delete gate has already raised the ERROR warning one call earlier — a second identical warning in the same `Delete` is noise. See open question 1; whatever is decided, decide it once for all four children.

---

#### Sequencing (a release rule, not just a task order)

The destroy-path commit (edits 3 and 4, across all four children) is worth merging on its own: it repairs a `terraform destroy` that is already broken today for any tree whose root went to ERROR. **If the destroy-path fix and the create record land in different releases, never ship the record without the destroy-path fix.** And if the CE lab shows Octavia refusing a child DELETE with 409 while the root is in ERROR, stop: drop the create record for the four children and add them to the design doc's "Known gap" section instead.

#### Changelog (Task 6)

One `### Fixed` bullet under `## [Unreleased]` in `CHANGELOG.md`, American English, existing house style, no ticket reference, the type name spelled **`pcd_lb_member`**:

> - `pcd_lb_member`: a member Octavia accepted now stays in state when the load balancer then fails to return to `ACTIVE`, and so does one whose create wait times out or whose apply is interrupted. The apply still fails with Octavia's reason, and Terraform marks the member tainted, so the next apply or a destroy deletes it. Before, the failed apply left no state: Terraform lost track of a member Octavia kept, and the next apply posted the same address and port into the same pool, which Octavia rejects as a duplicate, so the first had to be deleted through the API. Deleting a listener, pool, member, or health monitor no longer waits for the load balancer to be `ACTIVE` first — it waits only for the load balancer to leave its transient `PENDING_*` statuses — so a tree whose load balancer is in `ERROR` can now be destroyed instead of failing at the first child. The attributes Octavia had not reported yet are saved empty until the next refresh.

- [ ] **Step 4: Run the test and watch it pass**

Run the focused test again. Then the full suite.

- [ ] **Step 5: Verify**

```bash
cd '/Users/pushkarmulay/Documents/Claude Code Projects/Terraform Provider/terraform-provider-pcd/.claude/worktrees/inspiring-kilby-0bbd0b' && gofmt -s -l internal/
```
```bash
cd '/Users/pushkarmulay/Documents/Claude Code Projects/Terraform Provider/terraform-provider-pcd/.claude/worktrees/inspiring-kilby-0bbd0b' && go build ./...
```
```bash
cd '/Users/pushkarmulay/Documents/Claude Code Projects/Terraform Provider/terraform-provider-pcd/.claude/worktrees/inspiring-kilby-0bbd0b' && go vet ./...
```
```bash
cd '/Users/pushkarmulay/Documents/Claude Code Projects/Terraform Provider/terraform-provider-pcd/.claude/worktrees/inspiring-kilby-0bbd0b' && go test ./internal/services/loadbalancer/ -run 'TestMemberCreate' -v -count=1
```
```bash
cd '/Users/pushkarmulay/Documents/Claude Code Projects/Terraform Provider/terraform-provider-pcd/.claude/worktrees/inspiring-kilby-0bbd0b' && go test ./internal/... -timeout 120s
```
```bash
cd '/Users/pushkarmulay/Documents/Claude Code Projects/Terraform Provider/terraform-provider-pcd/.claude/worktrees/inspiring-kilby-0bbd0b' && golangci-lint run ./internal/services/loadbalancer/ ./internal/tfstate/
```
```bash
cd '/Users/pushkarmulay/Documents/Claude Code Projects/Terraform Provider/terraform-provider-pcd/.claude/worktrees/inspiring-kilby-0bbd0b' && grep -rn 'pcd_loadbalancer_' CHANGELOG.md internal/ examples/ templates/   # must print nothing. Do NOT sweep docs/: both design documents mention the wrong name on purpose, in the passages that say to correct it.
```
```bash
NEGATIVE CHECK A (must FAIL): temporarily delete the four record lines added by edit 2 and the tfstate import, then run go test ./internal/services/loadbalancer/ -run TestMemberCreateKeepsAMemberWhoseLoadBalancerFailed
```
```bash
NEGATIVE CHECK B (must FAIL): temporarily restore waitForLoadBalancerActive at member_resource.go's pre-delete gate, then run go test ./internal/services/loadbalancer/ -run TestMemberCreateKeepsAMemberWhoseLoadBalancerFailed
```
```bash
NEGATIVE CHECK C (must FAIL): temporarily restore the unconditional tail (_, readDiags := ...; Append; State.Set), then run go test ./internal/services/loadbalancer/ -run TestMemberCreateReadBackFailures
```
```bash
LAB (gates Task 5, run during Task 3): build the acceptance tree, drive the root load balancer into ERROR, then `openstack loadbalancer member delete <pool> <member>` and confirm Octavia accepts it rather than answering 409
```
```bash
LAB (validates the already-shipped five as much as the new eight): create a pcd_blockstorage_volume, force the create to fail, re-apply the tainted resource, and confirm no 'Provider produced inconsistent result after apply' error from UseStateForUnknown
```

- [ ] **Step 6: Commit**

```
fix(loadbalancer): keep a pool member whose load balancer failed

pcd_lb_member called pools.CreateMember, then waited for the root load
balancer to come back to ACTIVE, and returned the wait's error without
saving state. Octavia keeps the member it accepted, so a wait that gave
up on an ERROR root, timed out, or was interrupted left Terraform with
no record of a member that exists. The next apply posted the same
address and protocol port into the same pool, Octavia rejected it as a
duplicate, and the orphan had to be deleted through the API by hand.

Create now sets plan.ID from the created member and records it with
tfstate.RecordCreated between the create call and the wait. The apply
still fails with Octavia's reason, and Terraform marks the member
tainted, so the next apply or a destroy deletes it. The member's ID is
assigned only inside readInto, which the failure path never reaches, so
the explicit plan.ID assignment is what gives the recorded row a name
the destroy can act on.

Create's read-back after the wait is now guarded. It discarded readInto's
notFound flag and set state unconditionally; readInto returns without
touching the model on a 404 or an error, so that final Set wrote the
create plan's unknown values, which Terraform core rejects. A member
Octavia says is gone is now dropped from state, and one that is merely
unreadable keeps the recorded row.

Delete waits for the root load balancer to leave its transient PENDING_*
statuses instead of waiting for it to be ACTIVE, and resolving a pool
that has already been cascade-deleted drops the member from state rather
than failing. The ACTIVE wait fails on the first poll for an ERROR root,
so a destroy of a tree whose load balancer went to ERROR already failed
at the first child and never reached the root's cascade delete; this
repairs that, and is what makes a recorded member destroyable.

Covered by the load balancer package's first unit tests, which drive
Create, Read and Delete against an httptest Octavia.
```

**Open questions and traps recorded during planning:**

- The post-delete wait's diagnostic shape. The settled design says both of a child's waits become `settleBeforeDelete(ctx, client, lbID, "member", &resp.Diagnostics)`, but that helper hard-codes the summary "loadbalancer: waiting before %s delete" and raises the ERROR warning, so the post-delete site would report "before" and emit a second identical warning in the same Delete. I used `waitForLoadBalancerSettled` directly at the post-delete site (member_resource.go:285) to keep the "after" wording and one warning. The alternative is to give `settleBeforeDelete` a `stage` parameter and warn only when `stage == "before"`. Either is fine; decide once in Task 3 and apply it identically to listener (:315), pool (:306), member (:285) and monitor (:291).
- The settled design contradicts itself on the rootLBIDFromPool 404 short-circuit. Task 3's body says to add it at member_resource.go:269, and one graft says to keep it as defense-in-depth for `-refresh=false`; a later graft says to keep 'Design 2's verified conclusion that 404 handling at rootLBIDFromPool / rootLBIDFromListener is unnecessary, and record it as a known gap'. I included it (edit 3, four lines, no test) because Task 3 lists it explicitly and it is cheap. If the reviewer prefers the known-gap route, drop edit 3 — nothing else in this section depends on it. I verified the refresh-order argument: under default refresh, Read maps notFound to RemoveResource at member_resource.go:169-175 and the resource is dropped before Delete runs, so this branch is reachable only under `-refresh=false`.
- The survey's central claim about this resource is wrong and the plan must not repeat it. It says recording the member converts a destroy that works today into one that fails, because the root's `Cascade: true` delete (loadbalancer_resource.go:258) would otherwise clean the member up. I re-derived the destroy order: the member's pool has the identical pre-delete ACTIVE gate at pool_resource.go:296 (as do listener_resource.go:305 and monitor_resource.go:281), Terraform destroys the pool before the root, so with the root in ERROR the destroy ALREADY fails today and the cascade is never reached. The delete-path relaxation is therefore an independently justified fix for a pre-existing destroy blocker, not regression cover. It still must land before the create record, but the PR body should say why in those terms. The survey's framing holds only in the unusual config where no managed LB child sits between the member and the root (pool_id from a data source or a literal).
- Unverified against the lab, and it gates Task 5: that Octavia/OVN accepts DELETE on a member while the root load balancer is in ERROR. The package doc at loadbalancer.go:7-11 ties the 409 to PENDING_* and upstream Octavia's immutability check lists only PENDING_*, but nothing in this tree proves it for PCD. If Octavia refuses with 409 the user-visible outcome is the same as today's pre-wait abort with a better message and state preserved — but per the settled design's own sequencing, DO NOT SHIP the four children's create record in that case; put them in the design doc's Known gap section instead.
- Also unverified: that Octavia answers 409 (rather than silently creating a second member) on a re-POST of the same address+protocol_port into the same pool. That is the user-visible symptom the changelog bullet asserts. If Octavia instead accepts the duplicate, soften the bullet to "a second member serving the same backend" — the orphan is still the bug, the failure mode is just different.
- Task 0's RecordCreated doc comment. I ran the verification against a shortened comment; the real Task 0 must ship the full service-neutral version from the settled design, and must also keep a shortened two-line version of the images comment at image_resource.go:192-197 (Glance holds the image before any data is loaded, so the record is earlier than the wait, not just before it) while dropping compute's six-line comment, which the shared doc subsumes.
- fakeConfig ownership between Tasks 4 and 5. Both create internal/services/loadbalancer/failed_create_internal_test.go. Whichever lands first owns the file and the fakeConfig helper; the second appends its test functions. If they are developed in parallel, expect a trivial conflict there.
- UseStateForUnknown is on nine of memberModel's fifteen attributes (member_resource.go:69, 71, 75, 79-89). A tainted member's computed attributes are recorded as null, and in principle core could copy those nulls into the replacement plan and then reject the apply with 'Provider produced inconsistent result after apply'. The in-process unit tests provably cannot catch this — they call Create/Read/Delete directly and never go through core's plan. The already-shipped pcd_blockstorage_volume has identical exposure, so one CE lab run against the volume settles it for all thirteen call sites; it is listed under verification commands rather than duplicated per resource.

---
### Task 5d: `pcd_lb_monitor`

Part of Task 5. **Do not start until Task 3 is verified on the lab.** Appends to the test file Task 4 created. Do not claim in the changelog that Octavia allows only one health monitor per pool — gophercloud's own docs say otherwise.

**Files:**
#### Files

**Prerequisites that must already be merged before any edit below is made**

- **Task 0** — `internal/tfstate/tfstate.go` must export `RecordCreated(ctx, resp *resource.CreateResponse, plan any) bool` (the promotion of `blockstorage.recordCreated`, plus the `path.Root("id")` guard). Edit B below will not compile until it exists.
- **Task 3** — `internal/services/loadbalancer/loadbalancer.go` must define `lbPending`, `waitForLoadBalancerSettled` and `settleBeforeDelete`, and Task 3 must have been lab-verified (Octavia accepts a child DELETE while the root is in ERROR). Edit A below is this resource's *share of the Task 3 commit*, not of this one. **If the lab check fails, Edits B and C are dropped and `pcd_lb_monitor` goes into the design doc's "Known gap" section instead.**

**Modify — `internal/services/loadbalancer/monitor_resource.go`** (absolute path: `/Users/pushkarmulay/Documents/Claude Code Projects/Terraform Provider/terraform-provider-pcd/.claude/worktrees/inspiring-kilby-0bbd0b/internal/services/loadbalancer/monitor_resource.go`)

| Lines | Edit | Ships in |
|---|---|---|
| 28–29 (import block tail) | add `internal/tfstate` | this commit (Task 5) |
| 148–156 (`monitors.Create` → post-create wait) | **Edit B**: set `plan.ID`, record before the wait | this commit (Task 5) |
| 158–160 (the `_, readDiags := r.readInto(...)` tail) | **Edit C**: guard the tail | this commit (Task 5) |
| 274–283 (`rootLBIDFromPool` + pre-delete wait) | **Edit A1**: 404 short-circuit + `settleBeforeDelete` | Task 3 commit |
| 291–293 (post-delete wait) | **Edit A2**: `settleBeforeDelete` | Task 3 commit |

Nothing else in the file changes. In particular **line 118 stays `waitForLoadBalancerActive`** — that is the *pre*-create wait, it runs before `monitors.Create`, so there is nothing yet to record and building a monitor on a broken load balancer should still fail. `Update` (lines 244–254) keeps `waitForLoadBalancerActive` at both of its waits for the same reason.

**Create — `internal/services/loadbalancer/failed_create_internal_test.go`**

Package `loadbalancer` (internal). This is the first non-acceptance test in the package: the only existing test file, `internal/services/loadbalancer/loadbalancer_test.go`, is `package loadbalancer_test` and is `TF_ACC`-gated (`TestAccLBLoadBalancer_tree`, line 23), so it cannot reach `monitorResource` or `monitorModel`.

Shared-file naming follows the design's convention (several resources share one fake → `failed_create_internal_test.go`, matching `internal/services/blockstorage/failed_create_internal_test.go`). **Task 4 (root `pcd_lb_loadbalancer`) creates this same file and defines `fakeConfig` in it.** If Task 4 has already landed, delete the `fakeConfig` function and the `gophercloud` and `clients` imports from the snippet below and append only the two test functions plus `monitorPlan`; otherwise paste the file whole.

**Not in this task**

- `docs/superpowers/specs/2026-09-19-create-state-before-wait-design.md:194–195` still names `pcd_loadbalancer_monitor`, which is not a registered type (`Metadata` at `monitor_resource.go:66` registers `pcd_lb_monitor`). Per the settled design that correction is made in **Task 0**, before any bullet can copy the wrong name.
- The `CHANGELOG.md` bullet is **Task 6**. Proposed text, for the Task 6 author, in the existing `### Fixed` voice and American English:

  > - `pcd_lb_monitor`, `pcd_lb_listener`, `pcd_lb_pool`, `pcd_lb_member`: a child of a load balancer that Octavia accepts and then fails to apply now stays in state, and so does one whose wait times out or whose apply is interrupted. These resources wait on the *root* load balancer, not on themselves, so the wait also fails when the root goes to `ERROR` for an unrelated reason or when a single `GET` on it returns a transient error — cases in which the child itself is perfectly healthy. The apply still fails with Octavia's reason, and Terraform marks the resource tainted, so the next apply deletes and recreates it and a destroy deletes it. Before, the failed apply left no state: Terraform lost track of an object Octavia kept, and it had to be deleted through the API by hand. A destroy of one of these children no longer refuses to run while the root load balancer is in `ERROR`; it warns, naming the load balancer, and issues the delete anyway. The attributes Octavia had not reported yet are saved empty until the next refresh.

**Interfaces:**
#### Interfaces

#### Consumes (must exist before this task compiles)

```go
// internal/tfstate/tfstate.go — added by Task 0.
func RecordCreated(ctx context.Context, resp *resource.CreateResponse, plan any) bool

// internal/services/loadbalancer/loadbalancer.go — added by Task 3.
const lbPending = "PENDING_" // added to the existing const block at lines 82-86
func waitForLoadBalancerSettled(ctx context.Context, client *gophercloud.ServiceClient, lbID string, timeout time.Duration) (string, error)
func settleBeforeDelete(ctx context.Context, client *gophercloud.ServiceClient, lbID, child string, diags *diag.Diagnostics) bool
```

#### Consumes (already in the tree, unchanged)

```go
// internal/services/loadbalancer/loadbalancer.go
const defaultLBTimeout = 10 * time.Minute                                     // line 89
const lbActive, lbError, lbDeleted = "ACTIVE", "ERROR", "DELETED"             // lines 83-85
func waitForLoadBalancerActive(ctx context.Context, client *gophercloud.ServiceClient, lbID string, timeout time.Duration) error // line 109
func rootLBIDFromPool(ctx context.Context, client *gophercloud.ServiceClient, poolID string) (string, error)                     // line 170
func intPtrIfSet(v types.Int64) *int                                          // line 33

// internal/services/loadbalancer/monitor_resource.go
func (r *monitorResource) readInto(ctx context.Context, client *gophercloud.ServiceClient, id string, m *monitorModel) (notFound bool, diags diag.Diagnostics) // line 300
func isHTTPMonitor(t string) bool                                             // line 337

// internal/clients/config.go
func (c *Config) LoadBalancerV2Client() (*gophercloud.ServiceClient, error)   // line 326
```

#### Produces

No new exported provider API and no schema change. The registered type name stays `pcd_lb_monitor` (`monitor_resource.go:66`).

New package-private test symbols in `internal/services/loadbalancer/failed_create_internal_test.go`:

```go
func fakeConfig(url string) *clients.Config                                   // shared with Task 4; define once
func monitorPlan(ctx context.Context, t *testing.T, s schema.Schema) tfsdk.Plan
func TestMonitorCreateKeepsAMonitorWhoseLoadBalancerFailed(t *testing.T)
func TestMonitorCreateDropsAMonitorGoneBeforeTheReadBack(t *testing.T)
```

#### Octavia wire contract the fake must honor (verified against gophercloud v2.13.0)

- **URL prefix `/v2.0/` is mandatory.** `openstack.NewLoadBalancerV2` (`openstack/client.go:457-465`) sets `sc.ResourceBase = endpoint + "v2.0/"`, and `ServiceURL` resolves through `ResourceBaseURL` (`service_client.go:38-49`). With the `EndpointLocator` returning `url + "/"`, every path the fake sees begins `/v2.0/lbaas/`. **Do not set `EndpointOverrides`** in the test: `Config.applyOverride` (`internal/clients/config.go:359-364`) blanks `ResourceBase` and the prefix silently disappears.
- **Status codes** (all three calls pass `nil` RequestOpts — `monitors/requests.go:199, 206, 308` — so gophercloud's defaults at `provider_client.go:561-576` apply): `POST` → `{201, 202}`, `GET` → `{200}`, `DELETE` → `{202, 204}`. A `200` on the DELETE fails the test for the wrong reason.
- **Wrapper keys**: `healthmonitor` (`monitors/results.go:170-176`), `pool` (`pools/results.go:195`), `loadbalancer` (`loadbalancers/results.go:200`).
- `Monitor.Pools` is `[]{"id": ...}` (`monitors/results.go:11-13, :88`); there is no flat `pool_id`. `readInto` reads `mon.Pools[0].ID` at `monitor_resource.go:311-313`.
- `http_version` must be **omitted or a JSON number** — the custom `UnmarshalJSON` at `monitors/results.go:102-120` decodes it as `float64`.
- `pools.Pool.Loadbalancers` is preferred by `rootLBIDFromPool` (`loadbalancer.go:175-177`); supplying it avoids a second `GET /v2.0/lbaas/listeners/{id}`.
- `loadbalancers.LoadBalancer` has a custom `UnmarshalJSON` (`loadbalancers/results.go:92`) that parses `created_at`/`updated_at`; omitting both fields is fine.
- `gophercloud.WaitFor` (`util.go:87-90`) evaluates the predicate **once before starting its 1s ticker**, so a settled status resolves instantly. Both tests below therefore cost no wall-clock sleep; `t.Parallel()` is still applied for consistency with `fadf2bf`.

- [ ] **Step 1: Write the failing test**

Add to the test file named above:

```go
// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package loadbalancer

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
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

// fakeConfig points a clients.Config at a test server, so the Octavia client the
// resources build resolves to it. The locator ignores the endpoint options, so it
// answers for every service type and availability. EndpointOverrides is left
// unset on purpose: Config.applyOverride blanks ResourceBase, which would drop
// the /v2.0/ prefix NewLoadBalancerV2 adds and leave every request unmatched.
//
// NOTE: if Task 4 (pcd_lb_loadbalancer) has already added this file, delete this
// function and the gophercloud/clients imports and append only what follows.
func fakeConfig(url string) *clients.Config {
	return &clients.Config{
		Region: "region-one",
		Provider: &gophercloud.ProviderClient{
			EndpointLocator: func(gophercloud.EndpointOpts) (string, error) { return url + "/", nil },
		},
	}
}

// monitorPlan builds the create plan a first apply produces for a TCP health
// monitor: the required attributes known, admin_state_up known from its schema
// default, and everything the schema computes still unknown.
func monitorPlan(ctx context.Context, t *testing.T, s schema.Schema) tfsdk.Plan {
	t.Helper()
	planned := monitorModel{
		ID:                 types.StringUnknown(),
		PoolID:             types.StringValue("pool-1"),
		Type:               types.StringValue("TCP"),
		Delay:              types.Int64Value(10),
		Timeout:            types.Int64Value(5),
		MaxRetries:         types.Int64Value(3),
		MaxRetriesDown:     types.Int64Unknown(),
		HTTPMethod:         types.StringUnknown(),
		URLPath:            types.StringUnknown(),
		ExpectedCodes:      types.StringUnknown(),
		Name:               types.StringValue("web-health"),
		AdminStateUp:       types.BoolValue(true),
		Tags:               types.SetUnknown(types.StringType),
		ProvisioningStatus: types.StringUnknown(),
		OperatingStatus:    types.StringUnknown(),
		Region:             types.StringUnknown(),
	}
	plan := tfsdk.Plan{Schema: s, Raw: tftypes.NewValue(s.Type().TerraformType(ctx), nil)}
	if d := plan.Set(ctx, &planned); d.HasError() {
		t.Fatalf("building the plan: %v", d)
	}
	return plan
}

// A health monitor Octavia accepts stays in Octavia even when the load balancer
// it belongs to then settles in ERROR. The monitor resource waits on the ROOT
// load balancer, not on itself, so that wait fails for reasons the monitor had
// no part in. Create used to return without state, so Terraform forgot a monitor
// Octavia kept and it had to be deleted through the API. Create must return the
// error with the monitor in state (Terraform then taints it), the refresh must
// report the failure status, and the delete a destroy runs must still reach
// Octavia while the load balancer is in ERROR.
func TestMonitorCreateKeepsAMonitorWhoseLoadBalancerFailed(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	const monitorBody = `{"healthmonitor": {"id": "hm-1", "name": "web-health", "type": "TCP",
		"delay": 10, "timeout": 5, "max_retries": 3, "max_retries_down": 3,
		"http_method": "", "url_path": "", "expected_codes": "", "admin_state_up": true,
		"provisioning_status": "ERROR", "operating_status": "OFFLINE",
		"pools": [{"id": "pool-1"}], "tags": []}}`

	var mu sync.Mutex
	lbStatus := lbActive
	monitorCreated, deleteCalled, lbGetsAfterDelete := false, false, 0
	octavia := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch r.Method + " " + r.URL.Path {
		case "GET /v2.0/lbaas/pools/pool-1":
			// rootLBIDFromPool prefers "loadbalancers", so no listener hop.
			fmt.Fprint(w, `{"pool": {"id": "pool-1", "loadbalancers": [{"id": "lb-1"}], "listeners": []}}`)
		case "GET /v2.0/lbaas/loadbalancers/lb-1":
			if deleteCalled {
				lbGetsAfterDelete++
			}
			fmt.Fprintf(w, `{"loadbalancer": {"id": "lb-1", "provisioning_status": %q}}`, lbStatus)
		case "POST /v2.0/lbaas/healthmonitors":
			// Octavia has the monitor from here on; applying the change then
			// fails and the load balancer settles in ERROR, not PENDING_*.
			monitorCreated = true
			lbStatus = lbError
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, monitorBody)
		case "GET /v2.0/lbaas/healthmonitors/hm-1":
			if deleteCalled {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			fmt.Fprint(w, monitorBody)
		case "DELETE /v2.0/lbaas/healthmonitors/hm-1":
			deleteCalled = true
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotImplemented)
		}
	}))
	defer octavia.Close()

	r := &monitorResource{config: fakeConfig(octavia.URL)}
	var sch resource.SchemaResponse
	r.Schema(ctx, resource.SchemaRequest{}, &sch)
	s := sch.Schema
	plan := monitorPlan(ctx, t, s)

	createResp := resource.CreateResponse{State: tfsdk.State{Schema: s, Raw: tftypes.NewValue(s.Type().TerraformType(ctx), nil)}}
	r.Create(ctx, resource.CreateRequest{Plan: plan}, &createResp)
	if !createResp.Diagnostics.HasError() {
		t.Fatal("create succeeded; want the load balancer's ERROR provisioning status reported")
	}
	if createResp.State.Raw.IsNull() {
		t.Fatal("create returned no state: Terraform forgets hm-1 and it has to be deleted through the Octavia API")
	}
	if !createResp.State.Raw.IsFullyKnown() {
		t.Fatalf("create state holds unknown values, which Terraform refuses: %v", createResp.State.Raw)
	}
	var got monitorModel
	if d := createResp.State.Get(ctx, &got); d.HasError() {
		t.Fatalf("reading the create state: %v", d)
	}
	if got.ID.ValueString() != "hm-1" {
		t.Fatalf("create state id = %s, want hm-1: a row without an ID names nothing a destroy could delete", got.ID)
	}
	if got.PoolID.ValueString() != "pool-1" {
		t.Fatalf("create state pool_id = %s, want pool-1: Delete resolves the root load balancer through it", got.PoolID)
	}
	if got.Type.ValueString() != "TCP" || got.Name.ValueString() != "web-health" {
		t.Fatalf("create state type=%s name=%s; want TCP, web-health", got.Type, got.Name)
	}
	mu.Lock()
	created := monitorCreated
	mu.Unlock()
	if !created {
		t.Fatal("create never issued POST /v2.0/lbaas/healthmonitors")
	}

	// terraform destroy (or the replacing apply) refreshes the tainted monitor first.
	readResp := resource.ReadResponse{State: createResp.State}
	r.Read(ctx, resource.ReadRequest{State: createResp.State}, &readResp)
	if readResp.Diagnostics.HasError() {
		t.Fatalf("refresh of the recorded monitor: %v", readResp.Diagnostics)
	}
	if d := readResp.State.Get(ctx, &got); d.HasError() {
		t.Fatalf("reading the refreshed state: %v", d)
	}
	if got.ProvisioningStatus.ValueString() != "ERROR" {
		t.Fatalf("refreshed provisioning_status = %s, want ERROR", got.ProvisioningStatus)
	}
	if got.Region.ValueString() != "region-one" {
		t.Fatalf("refreshed region = %s, want region-one: the null the record left must be filled by the refresh", got.Region)
	}

	deleteResp := resource.DeleteResponse{State: readResp.State}
	r.Delete(ctx, resource.DeleteRequest{State: readResp.State}, &deleteResp)
	if deleteResp.Diagnostics.HasError() {
		t.Fatalf("delete of a monitor whose load balancer is in ERROR: %v", deleteResp.Diagnostics)
	}
	if n := deleteResp.Diagnostics.WarningsCount(); n != 2 {
		t.Fatalf("delete raised %d warnings, want 2 (one before and one after the delete) naming the ERROR load balancer: %v", n, deleteResp.Diagnostics)
	}
	for _, d := range deleteResp.Diagnostics.Warnings() {
		if !strings.Contains(d.Detail(), "lb-1") {
			t.Fatalf("delete warning does not name the load balancer: %q / %q", d.Summary(), d.Detail())
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if !deleteCalled {
		t.Fatal("delete never issued DELETE /v2.0/lbaas/healthmonitors/hm-1: the pre-delete wait rejected the ERROR load balancer")
	}
	if lbGetsAfterDelete < 1 {
		t.Fatalf("delete polled the load balancer %d times after the DELETE; want the post-delete settle to run so the next resource in the destroy does not hit a 409", lbGetsAfterDelete)
	}
}

// A monitor that disappears between the create and Create's read-back must not
// leave a row behind. readInto returns before assigning anything to the model on
// a 404, so the plan still holds the create plan's unknown values: the tail must
// not write them over the row the record left, and must not keep a row for an
// object Octavia no longer has.
func TestMonitorCreateDropsAMonitorGoneBeforeTheReadBack(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	var mu sync.Mutex
	monitorGets := 0
	octavia := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch r.Method + " " + r.URL.Path {
		case "GET /v2.0/lbaas/pools/pool-1":
			fmt.Fprint(w, `{"pool": {"id": "pool-1", "loadbalancers": [{"id": "lb-1"}], "listeners": []}}`)
		case "GET /v2.0/lbaas/loadbalancers/lb-1":
			fmt.Fprint(w, `{"loadbalancer": {"id": "lb-1", "provisioning_status": "ACTIVE"}}`)
		case "POST /v2.0/lbaas/healthmonitors":
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{"healthmonitor": {"id": "hm-2", "name": "web-health", "type": "TCP",
				"delay": 10, "timeout": 5, "max_retries": 3, "admin_state_up": true,
				"pools": [{"id": "pool-1"}], "tags": []}}`)
		case "GET /v2.0/lbaas/healthmonitors/hm-2":
			// Something removed the monitor between the create and the read-back.
			monitorGets++
			w.WriteHeader(http.StatusNotFound)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotImplemented)
		}
	}))
	defer octavia.Close()

	r := &monitorResource{config: fakeConfig(octavia.URL)}
	var sch resource.SchemaResponse
	r.Schema(ctx, resource.SchemaRequest{}, &sch)
	s := sch.Schema
	plan := monitorPlan(ctx, t, s)

	createResp := resource.CreateResponse{State: tfsdk.State{Schema: s, Raw: tftypes.NewValue(s.Type().TerraformType(ctx), nil)}}
	r.Create(ctx, resource.CreateRequest{Plan: plan}, &createResp)
	if !createResp.Diagnostics.HasError() {
		t.Fatal("create succeeded; want the missing monitor reported")
	}
	if !createResp.State.Raw.IsNull() {
		t.Fatalf("create kept state for a monitor Octavia no longer has: %v", createResp.State.Raw)
	}
	mu.Lock()
	defer mu.Unlock()
	if monitorGets != 1 {
		t.Fatalf("create read the monitor back %d times, want 1", monitorGets)
	}
}
```

- [ ] **Step 2: Run it and watch it fail**

Run the focused test for this resource. It must fail because `Create` returns no state — not for a compile error or a fake-server mismatch. If it fails for another reason, fix the test before touching the resource.

- [ ] **Step 3: Write the implementation**

#### Implementation

#### Edit A — Delete path (**ships in the Task 3 commit, not this one**)

`internal/services/loadbalancer/monitor_resource.go`, lines 274–293. Both waits change. The pre-delete wait at line 280 is the blocker: `waitForLoadBalancerActive` hits `case lbError, lbDeleted:` (`loadbalancer.go:120-121`) on its very first poll, so `monitors.Delete` at line 284 is never reached and a monitor recorded by Edit B could not be destroyed while the root is in `ERROR`. The post-delete wait at line 291 changes too: its only job is to stop the *next* resource in the destroy hitting Octavia's 409, which `waitForLoadBalancerSettled` serves identically (it never returns while `PENDING_*`), and today it turns a DELETE that succeeded into a reported failure leaving a non-existent monitor in state.

**Before:**

```go
	poolID := state.PoolID.ValueString()
	rootLB, err := rootLBIDFromPool(ctx, client, poolID)
	if err != nil {
		resp.Diagnostics.AddError("loadbalancer: resolving root load balancer", err.Error())
		return
	}
	if err := waitForLoadBalancerActive(ctx, client, rootLB, defaultLBTimeout); err != nil {
		resp.Diagnostics.AddError("loadbalancer: waiting before monitor delete", err.Error())
		return
	}
	if err := monitors.Delete(ctx, client, state.ID.ValueString()).ExtractErr(); err != nil {
		if gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
			return
		}
		resp.Diagnostics.AddError("loadbalancer: deleting monitor", err.Error())
		return
	}
	if err := waitForLoadBalancerActive(ctx, client, rootLB, defaultLBTimeout); err != nil {
		resp.Diagnostics.AddError("loadbalancer: waiting after monitor delete", err.Error())
	}
}
```

**After:**

```go
	poolID := state.PoolID.ValueString()
	rootLB, err := rootLBIDFromPool(ctx, client, poolID)
	if err != nil {
		if gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
			// The pool is gone, so the monitor went with it. Returning with no
			// diagnostics drops the resource from state, which is the right
			// outcome for a child whose parent was already cascade-deleted.
			// Under the default refresh, Read removes it before Delete runs;
			// this covers a destroy with -refresh=false.
			return
		}
		resp.Diagnostics.AddError("loadbalancer: resolving root load balancer", err.Error())
		return
	}
	if !settleBeforeDelete(ctx, client, rootLB, "monitor", &resp.Diagnostics) {
		return
	}
	if err := monitors.Delete(ctx, client, state.ID.ValueString()).ExtractErr(); err != nil {
		if gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
			return
		}
		resp.Diagnostics.AddError("loadbalancer: deleting monitor", err.Error())
		return
	}
	// The next resource in the destroy must not hit Octavia's 409, so wait for
	// the load balancer to leave PENDING_* again. A root still in ERROR is
	// warned about, not failed: the monitor is already gone.
	settleBeforeDelete(ctx, client, rootLB, "monitor", &resp.Diagnostics)
}
```

`settleBeforeDelete` is a plain function-call statement at the post-delete site; discarding its `bool` needs no `_ =`.

---

#### Edit 0 — imports

`internal/services/loadbalancer/monitor_resource.go`, lines 28–29. `fmt` (line 11) and `types` (line 26) are already imported.

**Before:**

```go
	"github.com/platform9/terraform-provider-pcd/internal/clients"
)
```

**After:**

```go
	"github.com/platform9/terraform-provider-pcd/internal/clients"
	"github.com/platform9/terraform-provider-pcd/internal/tfstate"
)
```

---

#### Edit B — record the monitor before the post-create wait

`internal/services/loadbalancer/monitor_resource.go`, lines 148–156. `Create` never assigns `plan.ID` anywhere: the only assignment in the whole file is inside `readInto` at line 310, which the failure path never reaches. Without the explicit `plan.ID` line the recorded row would carry a null `id`, and `tfstate.RecordCreated`'s ID guard would (correctly) drop the row and report a provider bug.

**Before:**

```go
	mon, err := monitors.Create(ctx, client, createOpts).Extract()
	if err != nil {
		resp.Diagnostics.AddError("loadbalancer: creating monitor", err.Error())
		return
	}
	if err := waitForLoadBalancerActive(ctx, client, rootLB, defaultLBTimeout); err != nil {
		resp.Diagnostics.AddError("loadbalancer: waiting after monitor create", err.Error())
		return
	}
```

**After:**

```go
	mon, err := monitors.Create(ctx, client, createOpts).Extract()
	if err != nil {
		resp.Diagnostics.AddError("loadbalancer: creating monitor", err.Error())
		return
	}

	plan.ID = types.StringValue(mon.ID)
	// Octavia holds the monitor from here on. The wait below is on the ROOT load
	// balancer, not on the monitor, so it also fails for reasons the monitor had
	// no part in — the root going to ERROR for a sibling's sake, or one failed
	// GET on it. Record the monitor before that wait either way.
	if !tfstate.RecordCreated(ctx, resp, &plan) {
		return
	}

	if err := waitForLoadBalancerActive(ctx, client, rootLB, defaultLBTimeout); err != nil {
		resp.Diagnostics.AddError("loadbalancer: waiting after monitor create", err.Error())
		return
	}
```

`RecordCreated` nulls the unknowns in **`resp.State`**, never in the in-memory `plan`. That is load-bearing here: `readInto` decides whether to fill `pool_id` and `region` with `IsNull() || IsUnknown()` tests (`monitor_resource.go:311-313` and `:335-337`), and nulling `plan` would change which of those fire on the success path.

---

#### Edit C — guard the post-wait tail

`internal/services/loadbalancer/monitor_resource.go`, lines 158–160. Today the `notFound` return is discarded with `_` and the final `State.Set` is unconditional. When `readInto` returns `notFound` or an error it returns *before assigning anything to the model* (`monitor_resource.go:301-308`), so `plan` still carries the create plan's unknowns — `id`, `max_retries_down`, `http_method`, `url_path`, `expected_codes`, `tags`, `provisioning_status`, `operating_status`, `region` — and that `Set` would overwrite the clean, unknown-free row Edit B just recorded with a state full of unknowns. Latent today because there is no state to clobber; live the moment Edit B lands.

**Before:**

```go
	_, readDiags := r.readInto(ctx, client, mon.ID, &plan)
	resp.Diagnostics.Append(readDiags...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}
```

**After:**

```go
	notFound, readDiags := r.readInto(ctx, client, mon.ID, &plan)
	resp.Diagnostics.Append(readDiags...)
	if notFound {
		// The monitor is proven gone, so the row RecordCreated wrote would
		// assert an object that no longer exists. Drop it, the way Read does.
		resp.Diagnostics.AddError("loadbalancer: reading monitor after create",
			fmt.Sprintf("Monitor %s no longer exists.", mon.ID))
		resp.State.RemoveResource(ctx)
		return
	}
	if resp.Diagnostics.HasError() {
		// The monitor exists but could not be read: leave the row RecordCreated
		// wrote in place so Terraform taints it and a destroy removes it.
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}
```

Do **not** "simplify" this by re-running `tfstate.NullUnknowns` after the final `Set`. That would make the `notFound` case appear to succeed by writing a row whose every computed attribute is null, which is exactly the undeletable-phantom state this guard exists to prevent.

---

#### What the operator gets, traced

| Abandonment | Before | After |
|---|---|---|
| Root LB settles in `ERROR` after the POST is accepted | Apply fails, no state. The monitor is invisible to Terraform; it must be found and deleted through the Octavia API. | Apply fails, monitor recorded and tainted. `terraform destroy` warns that `lb-1` is in `ERROR`, issues the DELETE anyway, and the tree comes down. |
| One `loadbalancers.Get` returns a transient 5xx (`loadbalancer.go:113-116` aborts the wait on any Get error) | Apply fails, no state — even though the monitor and the load balancer are both healthy. | Apply fails, monitor recorded. The next apply or a destroy removes it; nothing is orphaned by a blip. This is the most common real abandonment. |
| Apply interrupted (Ctrl-C) or the 10-minute timeout with the root in `PENDING_UPDATE` | Apply fails, no state. | Monitor recorded. Once the root settles, destroy proceeds normally. |
| `readInto` 404s between the create and the read-back | Unconditional `Set` writes unknowns; Terraform core rejects the apply result. | Loud error, state removed — matching `Read` (`monitor_resource.go:177-182`) and the shipped `deleteCreatedImage` precedent. |

---

#### Test — new file `internal/services/loadbalancer/failed_create_internal_test.go`

```go
// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package loadbalancer

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
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

// fakeConfig points a clients.Config at a test server, so the Octavia client the
// resources build resolves to it. The locator ignores the endpoint options, so it
// answers for every service type and availability. EndpointOverrides is left
// unset on purpose: Config.applyOverride blanks ResourceBase, which would drop
// the /v2.0/ prefix NewLoadBalancerV2 adds and leave every request unmatched.
func fakeConfig(url string) *clients.Config {
	return &clients.Config{
		Region: "region-one",
		Provider: &gophercloud.ProviderClient{
			EndpointLocator: func(gophercloud.EndpointOpts) (string, error) { return url + "/", nil },
		},
	}
}

// monitorPlan builds the create plan a first apply produces for a TCP health
// monitor: the required attributes known, admin_state_up known from its schema
// default, and everything the schema computes still unknown.
func monitorPlan(ctx context.Context, t *testing.T, s schema.Schema) tfsdk.Plan {
	t.Helper()
	planned := monitorModel{
		ID:                 types.StringUnknown(),
		PoolID:             types.StringValue("pool-1"),
		Type:               types.StringValue("TCP"),
		Delay:              types.Int64Value(10),
		Timeout:            types.Int64Value(5),
		MaxRetries:         types.Int64Value(3),
		MaxRetriesDown:     types.Int64Unknown(),
		HTTPMethod:         types.StringUnknown(),
		URLPath:            types.StringUnknown(),
		ExpectedCodes:      types.StringUnknown(),
		Name:               types.StringValue("web-health"),
		AdminStateUp:       types.BoolValue(true),
		Tags:               types.SetUnknown(types.StringType),
		ProvisioningStatus: types.StringUnknown(),
		OperatingStatus:    types.StringUnknown(),
		Region:             types.StringUnknown(),
	}
	plan := tfsdk.Plan{Schema: s, Raw: tftypes.NewValue(s.Type().TerraformType(ctx), nil)}
	if d := plan.Set(ctx, &planned); d.HasError() {
		t.Fatalf("building the plan: %v", d)
	}
	return plan
}

// A health monitor Octavia accepts stays in Octavia even when the load balancer
// it belongs to then settles in ERROR. The monitor resource waits on the ROOT
// load balancer, not on itself, so that wait fails for reasons the monitor had
// no part in. Create used to return without state, so Terraform forgot a monitor
// Octavia kept and it had to be deleted through the API. Create must return the
// error with the monitor in state (Terraform then taints it), the refresh must
// report the failure status, and the delete a destroy runs must still reach
// Octavia while the load balancer is in ERROR.
func TestMonitorCreateKeepsAMonitorWhoseLoadBalancerFailed(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	const monitorBody = `{"healthmonitor": {"id": "hm-1", "name": "web-health", "type": "TCP",
		"delay": 10, "timeout": 5, "max_retries": 3, "max_retries_down": 3,
		"http_method": "", "url_path": "", "expected_codes": "", "admin_state_up": true,
		"provisioning_status": "ERROR", "operating_status": "OFFLINE",
		"pools": [{"id": "pool-1"}], "tags": []}}`

	var mu sync.Mutex
	lbStatus := lbActive
	monitorCreated, deleteCalled, lbGetsAfterDelete := false, false, 0
	octavia := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch r.Method + " " + r.URL.Path {
		case "GET /v2.0/lbaas/pools/pool-1":
			// rootLBIDFromPool prefers "loadbalancers", so no listener hop.
			fmt.Fprint(w, `{"pool": {"id": "pool-1", "loadbalancers": [{"id": "lb-1"}], "listeners": []}}`)
		case "GET /v2.0/lbaas/loadbalancers/lb-1":
			if deleteCalled {
				lbGetsAfterDelete++
			}
			fmt.Fprintf(w, `{"loadbalancer": {"id": "lb-1", "provisioning_status": %q}}`, lbStatus)
		case "POST /v2.0/lbaas/healthmonitors":
			// Octavia has the monitor from here on; applying the change then
			// fails and the load balancer settles in ERROR, not PENDING_*.
			monitorCreated = true
			lbStatus = lbError
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, monitorBody)
		case "GET /v2.0/lbaas/healthmonitors/hm-1":
			if deleteCalled {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			fmt.Fprint(w, monitorBody)
		case "DELETE /v2.0/lbaas/healthmonitors/hm-1":
			deleteCalled = true
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotImplemented)
		}
	}))
	defer octavia.Close()

	r := &monitorResource{config: fakeConfig(octavia.URL)}
	var sch resource.SchemaResponse
	r.Schema(ctx, resource.SchemaRequest{}, &sch)
	s := sch.Schema
	plan := monitorPlan(ctx, t, s)

	createResp := resource.CreateResponse{State: tfsdk.State{Schema: s, Raw: tftypes.NewValue(s.Type().TerraformType(ctx), nil)}}
	r.Create(ctx, resource.CreateRequest{Plan: plan}, &createResp)
	if !createResp.Diagnostics.HasError() {
		t.Fatal("create succeeded; want the load balancer's ERROR provisioning status reported")
	}
	if createResp.State.Raw.IsNull() {
		t.Fatal("create returned no state: Terraform forgets hm-1 and it has to be deleted through the Octavia API")
	}
	if !createResp.State.Raw.IsFullyKnown() {
		t.Fatalf("create state holds unknown values, which Terraform refuses: %v", createResp.State.Raw)
	}
	var got monitorModel
	if d := createResp.State.Get(ctx, &got); d.HasError() {
		t.Fatalf("reading the create state: %v", d)
	}
	if got.ID.ValueString() != "hm-1" {
		t.Fatalf("create state id = %s, want hm-1: a row without an ID names nothing a destroy could delete", got.ID)
	}
	if got.PoolID.ValueString() != "pool-1" {
		t.Fatalf("create state pool_id = %s, want pool-1: Delete resolves the root load balancer through it", got.PoolID)
	}
	if got.Type.ValueString() != "TCP" || got.Name.ValueString() != "web-health" {
		t.Fatalf("create state type=%s name=%s; want TCP, web-health", got.Type, got.Name)
	}
	mu.Lock()
	created := monitorCreated
	mu.Unlock()
	if !created {
		t.Fatal("create never issued POST /v2.0/lbaas/healthmonitors")
	}

	// terraform destroy (or the replacing apply) refreshes the tainted monitor first.
	readResp := resource.ReadResponse{State: createResp.State}
	r.Read(ctx, resource.ReadRequest{State: createResp.State}, &readResp)
	if readResp.Diagnostics.HasError() {
		t.Fatalf("refresh of the recorded monitor: %v", readResp.Diagnostics)
	}
	if d := readResp.State.Get(ctx, &got); d.HasError() {
		t.Fatalf("reading the refreshed state: %v", d)
	}
	if got.ProvisioningStatus.ValueString() != "ERROR" {
		t.Fatalf("refreshed provisioning_status = %s, want ERROR", got.ProvisioningStatus)
	}
	if got.Region.ValueString() != "region-one" {
		t.Fatalf("refreshed region = %s, want region-one: the null the record left must be filled by the refresh", got.Region)
	}

	deleteResp := resource.DeleteResponse{State: readResp.State}
	r.Delete(ctx, resource.DeleteRequest{State: readResp.State}, &deleteResp)
	if deleteResp.Diagnostics.HasError() {
		t.Fatalf("delete of a monitor whose load balancer is in ERROR: %v", deleteResp.Diagnostics)
	}
	if n := deleteResp.Diagnostics.WarningsCount(); n != 2 {
		t.Fatalf("delete raised %d warnings, want 2 (one before and one after the delete) naming the ERROR load balancer: %v", n, deleteResp.Diagnostics)
	}
	for _, d := range deleteResp.Diagnostics.Warnings() {
		if !strings.Contains(d.Detail(), "lb-1") {
			t.Fatalf("delete warning does not name the load balancer: %q / %q", d.Summary(), d.Detail())
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if !deleteCalled {
		t.Fatal("delete never issued DELETE /v2.0/lbaas/healthmonitors/hm-1: the pre-delete wait rejected the ERROR load balancer")
	}
	if lbGetsAfterDelete < 1 {
		t.Fatalf("delete polled the load balancer %d times after the DELETE; want the post-delete settle to run so the next resource in the destroy does not hit a 409", lbGetsAfterDelete)
	}
}

// A monitor that disappears between the create and Create's read-back must not
// leave a row behind. readInto returns before assigning anything to the model on
// a 404, so the plan still holds the create plan's unknown values: the tail must
// not write them over the row the record left, and must not keep a row for an
// object Octavia no longer has.
func TestMonitorCreateDropsAMonitorGoneBeforeTheReadBack(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	var mu sync.Mutex
	monitorGets := 0
	octavia := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch r.Method + " " + r.URL.Path {
		case "GET /v2.0/lbaas/pools/pool-1":
			fmt.Fprint(w, `{"pool": {"id": "pool-1", "loadbalancers": [{"id": "lb-1"}], "listeners": []}}`)
		case "GET /v2.0/lbaas/loadbalancers/lb-1":
			fmt.Fprint(w, `{"loadbalancer": {"id": "lb-1", "provisioning_status": "ACTIVE"}}`)
		case "POST /v2.0/lbaas/healthmonitors":
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{"healthmonitor": {"id": "hm-2", "name": "web-health", "type": "TCP",
				"delay": 10, "timeout": 5, "max_retries": 3, "admin_state_up": true,
				"pools": [{"id": "pool-1"}], "tags": []}}`)
		case "GET /v2.0/lbaas/healthmonitors/hm-2":
			// Something removed the monitor between the create and the read-back.
			monitorGets++
			w.WriteHeader(http.StatusNotFound)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotImplemented)
		}
	}))
	defer octavia.Close()

	r := &monitorResource{config: fakeConfig(octavia.URL)}
	var sch resource.SchemaResponse
	r.Schema(ctx, resource.SchemaRequest{}, &sch)
	s := sch.Schema
	plan := monitorPlan(ctx, t, s)

	createResp := resource.CreateResponse{State: tfsdk.State{Schema: s, Raw: tftypes.NewValue(s.Type().TerraformType(ctx), nil)}}
	r.Create(ctx, resource.CreateRequest{Plan: plan}, &createResp)
	if !createResp.Diagnostics.HasError() {
		t.Fatal("create succeeded; want the missing monitor reported")
	}
	if !createResp.State.Raw.IsNull() {
		t.Fatalf("create kept state for a monitor Octavia no longer has: %v", createResp.State.Raw)
	}
	mu.Lock()
	defer mu.Unlock()
	if monitorGets != 1 {
		t.Fatalf("create read the monitor back %d times, want 1", monitorGets)
	}
}
```

**Why no cancel-during-wait test here.** `internal/services/blockstorage/failed_create_internal_test.go:148-152` already pins that a canceled context loses only the wait, never the record. Once Task 0 makes `tfstate.RecordCreated` the single shared helper, that one test covers the mechanism for every call site, including this one; Task 0 updates that comment to name `tfstate.RecordCreated` so the claim is true provider-wide.

- [ ] **Step 4: Run the test and watch it pass**

Run the focused test again. Then the full suite.

- [ ] **Step 5: Verify**

```bash
gofmt -l internal/services/loadbalancer internal/tfstate
```
```bash
go build ./...
```
```bash
go vet ./internal/services/loadbalancer/...
```
```bash
go test ./internal/services/loadbalancer/... -run 'TestMonitorCreate' -count=1 -v
```
```bash
go test ./internal/... -timeout 120s
```
```bash
golangci-lint run ./internal/services/loadbalancer/...
```
```bash
grep -n '_, readDiags := r.readInto' internal/services/loadbalancer/monitor_resource.go  # must print nothing: the unguarded tail is gone
```
```bash
grep -n 'plan.ID = types.StringValue(mon.ID)' internal/services/loadbalancer/monitor_resource.go  # must print exactly one line, between the create and the post-create wait
```
```bash
grep -n 'waitForLoadBalancerActive' internal/services/loadbalancer/monitor_resource.go  # must print 3 lines: the pre-create wait and both Update waits, and none in Delete
```
```bash
grep -rn 'pcd_loadbalancer_monitor' docs/ CHANGELOG.md  # must print nothing: the registered name is pcd_lb_monitor
```
```bash
TF_ACC=1 go test ./internal/services/loadbalancer/... -run TestAccLBLoadBalancer_tree -v -timeout 60m  # CE lab: the happy path still builds and destroys a full tree
```
```bash
# CE lab gate for Task 3, run BEFORE this task ships: build a tree, drive the root load balancer into ERROR, then `openstack loadbalancer healthmonitor delete <id>` and confirm Octavia accepts it (not 409)
```
```bash
# CE lab replan check (covers the five already-shipped resources too): create a pcd_blockstorage_volume, force the create to fail, re-apply the tainted resource, and confirm no 'Provider returned invalid result object after apply' and no 'Provider produced inconsistent result after apply'
```

- [ ] **Step 6: Commit**

```
fix(loadbalancer): keep a health monitor whose load balancer wait fails

Create posted the health monitor, then waited for the root load balancer
to return to ACTIVE, and returned that wait's error without saving any
state. Octavia kept the monitor, Terraform did not know it existed, and
it had to be found and deleted through the API by hand.

The wait this resource abandons is not on the monitor at all: it polls
the root load balancer, which fails on ERROR or DELETED and, because
waitForLoadBalancerActive aborts on any loadbalancers.Get error, on a
single transient 5xx as well. Most of those leave a perfectly healthy
monitor behind, which is what made the orphan common.

Create now sets plan.ID from the created monitor and records it through
tfstate.RecordCreated before that wait, so the wait's error is returned
with the monitor in state. Terraform marks the resource tainted and the
next apply or a destroy removes it. Nothing about the pre-create wait
changes: building on a broken load balancer should still fail.

Also guard Create's read-back. It discarded readInto's notFound return
and set state unconditionally, so a monitor read back as 404, or a
transient failure reading it, wrote the create plan's unknown values
into state. On a 404 the monitor is proven gone, so the row is dropped
with an error, matching Read; on any other read failure the recorded row
is left in place for the destroy to clean up.

Adds the package's first unit tests: one drives a create whose load
balancer settles in ERROR and asserts the monitor is recorded with a
known ID, refreshes to ERROR, and is still deleted while the load
balancer is in ERROR; the other asserts a monitor gone before the
read-back leaves no state.

Depends on the shared tfstate.RecordCreated helper and on the relaxed
child delete path, which must both be merged first. If the destroy-path
fix and this record land in different releases, never ship this record
without it: recording a monitor whose Delete still pre-waits for ACTIVE
would turn a destroy that works today into one that fails.
```

**Open questions and traps recorded during planning:**

- settleBeforeDelete's error summary is hard-coded to "loadbalancer: waiting before %s delete", but the settled design puts it at BOTH the pre-delete (monitor_resource.go:280) and post-delete (:291) sites. The post-delete failure would then be reported as "waiting before monitor delete", losing a distinction today's code has. Recommend Task 3 add a phase parameter — settleBeforeDelete(ctx, client, lbID, child, phase string, diags *diag.Diagnostics) — called with "before"/"after", so the two summaries stay as they are. The tests above assert only on warning count and on the detail containing "lb-1", so they compile and pass under either signature.
- The settled design contradicts itself on the rootLBIDFromPool 404 short-circuit at monitor_resource.go:275. One graft says to keep the winner's short-circuit (cheap, covers -refresh=false) while dropping the 'independent deadlock' framing; a later graft says to drop the code entirely and record it as a known gap, since under the default refresh Read maps notFound to RemoveResource before Delete runs. Edit A1 above KEEPS the short-circuit, on the majority reading and because it is four lines. If the plan owner prefers the later graft, delete that branch from Edit A1 — nothing else in this task depends on it, and no test here covers it.
- The survey's claim that Octavia permits at most one health monitor per pool — and that an orphan therefore wedges every later apply with a 409 rather than creating a harmless duplicate — is NOT established anywhere in this tree, and gophercloud v2.13.0 states the opposite in its own doc comment (monitors/results.go:15-32: a pool can have several health monitors associated with it, inherited from Neutron LBaaS v2 and possibly stale for Octavia). It must not reach the CHANGELOG bullet unverified. The proposed bullet above deliberately omits it. If a CE lab run confirms the one-monitor-per-pool limit, add the stronger sentence to the pcd_lb_monitor bullet in Task 6; the case for the fix does not depend on it.
- Still unverified from this repository: that Octavia accepts a child DELETE while the root load balancer is in ERROR. This is Task 3's lab gate, not this task's. If Octavia refuses with 409, DO NOT SHIP this task — recording the monitor would make a whole-tree terraform destroy fail at the first child and never reach the root's cascade delete, which is strictly worse than the orphan. pcd_lb_monitor then goes into the design doc's Known gap section instead. Note that a 409 is bounded either way for a config where the monitor is the ONLY pcd_lb_* resource in state (pool and load balancer managed outside Terraform): today a destroy of that config is a no-op for the untracked monitor, and after the fix it would fail at the pre-delete gate until the root recovers. That narrow case is a real behavior change and is worth naming in the Task 3 PR body rather than claiming a blanket no-regression.
- The settled design makes Task 5 one commit covering all four load balancer children. The commit message above is scoped to pcd_lb_monitor alone. If all four ship together, change the subject to "fix(loadbalancer): keep a listener, pool, member, or monitor whose wait fails" and generalize the body; the monitor-specific paragraph about the wait polling the root rather than the child applies verbatim to all four.
- Task 4 (pcd_lb_loadbalancer) also creates internal/services/loadbalancer/failed_create_internal_test.go and defines fakeConfig there. Whichever task lands second must drop the duplicate definition. Because Task 4 and Task 5 are sequenced apart (0, then {1,2,4}, then 3, then 5), Task 4 lands first in the planned order and this task appends to the existing file.
- The design doc at docs/superpowers/specs/2026-09-19-create-state-before-wait-design.md:183 says 'the four resources this change fixes' while the task preamble says five are already fixed; both are consistent with the tree (compute/instance_resource.go:517-518 carries the same pattern from a separate branch, spec line 206). Whoever writes Task 6 should pick 'four' or 'five' deliberately rather than copying either number.
- loadbalancer.go:10-11 states that every resource waits for the root to be ACTIVE before and after each mutation using waitForLoadBalancerActive. Task 3 falsifies that sentence for the four children's delete paths and must update the package doc. Not this task's edit, but it will be stale if Task 3 forgets.

---
### Task 6: Changelog and corrections to the 2026-09-19 design document

**Files:**
- Modify: `CHANGELOG.md`
- Modify: `docs/superpowers/specs/2026-09-19-create-state-before-wait-design.md`

- [ ] **Step 1: Correct the load balancer resource names in the earlier design doc**

Its "Other resources with the same gap" section names them `pcd_loadbalancer_*`. The registered names are `pcd_lb_loadbalancer`, `pcd_lb_listener`, `pcd_lb_pool`, `pcd_lb_member`, `pcd_lb_monitor` (`loadbalancer_resource.go:64`, `listener_resource.go:71`, `pool_resource.go:77`, `member_resource.go:65`, `monitor_resource.go:66`).

- [ ] **Step 2: Move the resources fixed here out of that section**

Whatever shipped moves from "Other resources with the same gap" into scope. Anything dropped at the Task 3 lab gate stays, with a note saying why.

- [ ] **Step 3: Amend the earlier doc's Non-goals**

It says "Changing which statuses the waiters treat as failures." This plan does exactly that, deliberately, for two delete waiters. Record the amendment rather than leaving the two documents contradicting each other.

- [ ] **Step 4: Write the changelog entries**

One `### Fixed` bullet per resource that shipped, under `## [Unreleased]`, in the existing style: name the resource, what the service keeps, that the apply still fails, and that the resource is tainted so the next apply or a destroy removes it. Add a separate bullet for the load balancer child delete relaxation from Task 3 — it is a user-visible fix in its own right, independent of any create record.

Do **not** claim that Octavia permits at most one health monitor per pool. gophercloud's own documentation says otherwise and nothing in this work establishes it.

- [ ] **Step 5: Check**

```bash
grep -rn "pcd_loadbalancer_" CHANGELOG.md internal/ examples/ templates/   # expect no matches
grep -c "pcd_loadbalancer_" docs/superpowers/specs/2026-09-19-create-state-before-wait-design.md   # expect 0 once Step 1 has rewritten that section
grep -nE "behaviour|colour|organis|recognis|analyse|TBD|TODO" CHANGELOG.md   # expect no matches
grep -c '^## \[Unreleased\]' CHANGELOG.md   # expect 1
```

- [ ] **Step 6: Commit**

```
docs: changelog and scope corrections for the remaining create-state fixes
```

---

## Known gaps this plan deliberately leaves open

These are recorded in the design document and must appear in the PR body, not silently omitted:

- **`PENDING` / `PENDING_CREATE` abandonment.** A load balancer abandoned by a timeout or an interrupted apply sits in `PENDING_CREATE`, which Octavia treats as immutable, so the cascade delete returns 409 until it settles. Same shape as the Cinder `creating` gap already documented.
- **Waiters abort on any transient `Get` error**, including a 5xx. Recording state makes a blip taint a resource where today it orphaned one; widening the retry policy is a separate change.
- **The root load balancer's `Delete` keeps its lack of a pre-delete settle** — going straight to the cascade is what makes a whole-tree destroy work.
- **Three server-side behaviors are unverified**: that Octavia accepts a child `DELETE` with the root in `ERROR` (Task 3's lab gate settles this one), that Designate accepts a `DELETE` on a zone in `ERROR`, and that Barbican accepts one on a secret in `ERROR`. Each is safe either way — the failure mode is the same as today's, with a better message.
