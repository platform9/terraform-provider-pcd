# Clean `pcd_host_cluster_role` Plan After Import — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** An imported `pcd_host_cluster_role` plans no changes, and an update that touches only `wait_until_converged` never calls resmgr.

**Architecture:** Two small changes inside `internal/services/resmgr/host_cluster_role_resource.go`. `ImportState` writes `wait_until_converged = false` so the imported state already equals the schema default. `Update` compares the two server-side options (`host_cluster`, `backends`) between plan and prior state through a pure helper, `roleOptionsChanged`, and returns early without a PUT when nothing server-side changed. The schema is untouched.

**Tech Stack:** Go 1.26, terraform-plugin-framework v1.19.0 (`tfsdk`, `types`), terraform-plugin-go v0.31.0 (`tftypes`, already a direct dependency), standard `testing`.

Spec: `docs/superpowers/specs/2026-09-03-host-cluster-role-import-design.md`.

## Global Constraints

- Branch is `pushkar/host-cluster-role-import` (already created from `main`). Never `claude/`.
- No `Co-Authored-By` trailer and no "Generated with Claude Code" footer in commits or PR text.
- American English spelling in comments, changelog and commit messages.
- Do not change the schema: `wait_until_converged` stays `Optional + Computed + Default(false)`.
- Do not change `Create`, `Read`, `Delete`, `ValidateConfig`, `assignBody`, `putRole`, `waitConverged`.
- Every commit must pass `gofmt -l .` (empty), `go vet ./...`, `golangci-lint run ./...` (0 issues), `go test ./internal/...`.
- Run all commands from the worktree root: `/Users/pushkarmulay/Documents/Claude Code Projects/Terraform Provider/terraform-provider-pcd/.claude/worktrees/terraform-provider-registry-28784a`.

---

## File Structure

- Modify: `internal/services/resmgr/host_cluster_role_resource.go`
  - add `roleOptionsChanged`, `hostClusterOption`, `backendsOption` next to `assignBody`
  - `Update`: read prior state, short-circuit on no server-side change
  - `ImportState`: set `wait_until_converged`
- Create: `internal/services/resmgr/host_cluster_role_internal_test.go` (package `resmgr`, no lab)
  - table test for `roleOptionsChanged`
  - `Update` no-op test through the framework request types
  - `ImportState` test through a schema-built `tfsdk.State`
- Modify: `CHANGELOG.md` — new `## [Unreleased]` heading with one `Fixed` entry

The existing internal tests (`absence_internal_test.go`, `blueprint_delete_internal_test.go`) are the style reference: plain `testing`, table cases, failure messages that say what a wrong answer would do to a user.

---

### Task 1: `roleOptionsChanged` helper

**Files:**
- Create: `internal/services/resmgr/host_cluster_role_internal_test.go`
- Modify: `internal/services/resmgr/host_cluster_role_resource.go` (insert after `assignBody`, which ends around line 150)

**Interfaces:**
- Produces: `func roleOptionsChanged(plan, state *hostClusterRoleModel) bool` — true when the PUT body built from `plan` would differ from what `state` says the server holds. Used by Task 2.
- Produces: `func hostClusterOption(m *hostClusterRoleModel) string` and `func backendsOption(m *hostClusterRoleModel) types.List` — normalizers, internal to this file.

- [ ] **Step 1: Write the failing test**

Create `internal/services/resmgr/host_cluster_role_internal_test.go`:

```go
// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package resmgr

import (
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

func backendsList(names ...string) types.List {
	vals := make([]attr.Value, 0, len(names))
	for _, n := range names {
		vals = append(vals, types.StringValue(n))
	}
	return types.ListValueMust(types.StringType, vals)
}

func roleModel(hostCluster types.String, backends types.List) *hostClusterRoleModel {
	return &hostClusterRoleModel{
		ID:          types.StringValue("host-a/hypervisor"),
		HostID:      types.StringValue("host-a"),
		Role:        types.StringValue("hypervisor"),
		HostCluster: hostCluster,
		Backends:    backends,
	}
}

// Update used to PUT the role on every change, including one that touched only
// wait_until_converged, which resmgr never sees. This helper is what decides
// whether the PUT body would differ from what the server already holds, so a
// wrong "false" skips a real change and a wrong "true" sends a needless write.
func TestRoleOptionsChanged(t *testing.T) {
	noBackends := types.ListNull(types.StringType)
	for _, tc := range []struct {
		name        string
		plan, state *hostClusterRoleModel
		want        bool
	}{
		{name: "identical", plan: roleModel(types.StringValue("c1"), noBackends), state: roleModel(types.StringValue("c1"), noBackends)},
		{name: "both unset", plan: roleModel(types.StringNull(), noBackends), state: roleModel(types.StringNull(), noBackends)},
		// assignBody omits host_cluster for both null and "", so they are the same wire body.
		{name: "null vs empty host_cluster", plan: roleModel(types.StringValue(""), noBackends), state: roleModel(types.StringNull(), noBackends)},
		{name: "host_cluster changed", plan: roleModel(types.StringValue("c2"), noBackends), state: roleModel(types.StringValue("c1"), noBackends), want: true},
		{name: "host_cluster set from null", plan: roleModel(types.StringValue("c1"), noBackends), state: roleModel(types.StringNull(), noBackends), want: true},
		{name: "same backends", plan: roleModel(types.StringNull(), backendsList("syn")), state: roleModel(types.StringNull(), backendsList("syn")), want: false},
		{name: "backends changed", plan: roleModel(types.StringNull(), backendsList("syn", "nfs")), state: roleModel(types.StringNull(), backendsList("syn")), want: true},
		{name: "backends order changed", plan: roleModel(types.StringNull(), backendsList("nfs", "syn")), state: roleModel(types.StringNull(), backendsList("syn", "nfs")), want: true},
		// An empty list is sent as [] and null is not sent at all: different bodies.
		{name: "null vs empty backends", plan: roleModel(types.StringNull(), backendsList()), state: roleModel(types.StringNull(), noBackends), want: true},
		// Unknown is never sent, exactly like null.
		{name: "unknown vs null backends", plan: roleModel(types.StringNull(), types.ListUnknown(types.StringType)), state: roleModel(types.StringNull(), noBackends)},
		{name: "backends cleared", plan: roleModel(types.StringNull(), noBackends), state: roleModel(types.StringNull(), backendsList("syn")), want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := roleOptionsChanged(tc.plan, tc.state); got != tc.want {
				if tc.want {
					t.Fatalf("roleOptionsChanged = false; Update would skip a PUT the server needs")
				}
				t.Fatalf("roleOptionsChanged = true; Update would PUT the role for a change resmgr never sees")
			}
		})
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/services/resmgr/ -run TestRoleOptionsChanged -count=1`
Expected: build failure, `undefined: roleOptionsChanged`.

- [ ] **Step 3: Write the helper**

In `host_cluster_role_resource.go`, directly after the `assignBody` function (the one ending with `return body` before the `putRole` comment), insert:

```go
// roleOptionsChanged reports whether the PUT body assignBody would build from
// plan differs from what the server already holds according to state: the
// hypervisor's host_cluster or persistent-storage's backends. Everything else
// on the resource is client-side (wait_until_converged) or ForceNew, so a false
// here means resmgr has nothing to receive — and a repeated PUT is a real write
// resmgr acts on, not a no-op. A new server-side option must be added here too,
// or changes to it would be skipped.
func roleOptionsChanged(plan, state *hostClusterRoleModel) bool {
	if hostClusterOption(plan) != hostClusterOption(state) {
		return true
	}
	return !backendsOption(plan).Equal(backendsOption(state))
}

// hostClusterOption is host_cluster as assignBody sends it: null, unknown and
// "" are all "omitted".
func hostClusterOption(m *hostClusterRoleModel) string {
	if m.HostCluster.IsNull() || m.HostCluster.IsUnknown() {
		return ""
	}
	return m.HostCluster.ValueString()
}

// backendsOption is backends as assignBody sends it: null and unknown are both
// "omitted"; an empty list is sent as [] and so is a value in its own right.
func backendsOption(m *hostClusterRoleModel) types.List {
	if m.Backends.IsNull() || m.Backends.IsUnknown() {
		return types.ListNull(types.StringType)
	}
	return m.Backends
}
```

`types.List.Equal` compares null-ness, element type and elements in order, which is exactly the wire equality the spec asks for.

- [ ] **Step 4: Run the test to verify it passes**

Run: `go test ./internal/services/resmgr/ -run TestRoleOptionsChanged -count=1 -v`
Expected: every subtest `PASS`.

- [ ] **Step 5: Static checks and commit**

Run: `gofmt -l . && go vet ./... && golangci-lint run ./...`
Expected: no files listed, no vet output, `0 issues.`

```bash
git add internal/services/resmgr/host_cluster_role_resource.go internal/services/resmgr/host_cluster_role_internal_test.go
git commit -m "resmgr: decide whether a cluster role update changes anything server-side"
```

---

### Task 2: `Update` skips the PUT when only client-side attributes changed

**Files:**
- Modify: `internal/services/resmgr/host_cluster_role_resource.go` — the `Update` method (currently lines ~308–350; starts at the comment `// Update re-PUTs the assignment`)
- Test: `internal/services/resmgr/host_cluster_role_internal_test.go`

**Interfaces:**
- Consumes: `roleOptionsChanged(plan, state *hostClusterRoleModel) bool` from Task 1.
- Produces: `func roleSchema(t *testing.T) schema.Schema` and `func roleState(t *testing.T, m *hostClusterRoleModel) tfsdk.State` test helpers, reused by Task 3.

- [ ] **Step 1: Write the failing test**

Append to `host_cluster_role_internal_test.go`. Add these imports to the file's import block: `"context"`, `"github.com/hashicorp/terraform-plugin-framework/resource"`, `"github.com/hashicorp/terraform-plugin-framework/resource/schema"`, `"github.com/hashicorp/terraform-plugin-framework/tfsdk"`, `"github.com/hashicorp/terraform-plugin-go/tftypes"`.

```go
// roleSchema is the resource's real schema, so the framework request types used
// below carry exactly what Terraform would send.
func roleSchema(t *testing.T) schema.Schema {
	t.Helper()
	var resp resource.SchemaResponse
	(&hostClusterRoleResource{}).Schema(context.Background(), resource.SchemaRequest{}, &resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("schema: %v", resp.Diagnostics)
	}
	return resp.Schema
}

// roleState builds a tfsdk.State holding m. Starting from a null root is what
// the framework itself does before Create/ImportState write into it.
func roleState(t *testing.T, m *hostClusterRoleModel) tfsdk.State {
	t.Helper()
	ctx := context.Background()
	s := roleSchema(t)
	st := tfsdk.State{Schema: s, Raw: tftypes.NewValue(s.Type().TerraformType(ctx), nil)}
	if diags := st.Set(ctx, m); diags.HasError() {
		t.Fatalf("state: %v", diags)
	}
	return st
}

// A change that touches only wait_until_converged must not reach resmgr. The
// resource is built with no client at all: if Update tries to build one the
// test fails, which is the point.
func TestUpdateSkipsResmgrForClientSideChanges(t *testing.T) {
	ctx := context.Background()
	prior := roleModel(types.StringValue("c1"), types.ListNull(types.StringType))
	prior.WaitUntilConverged = types.BoolValue(false)
	want := roleModel(types.StringValue("c1"), types.ListNull(types.StringType))
	want.WaitUntilConverged = types.BoolValue(true)

	state := roleState(t, prior)
	plan := tfsdk.Plan{Schema: state.Schema, Raw: roleState(t, want).Raw}
	resp := resource.UpdateResponse{State: roleState(t, prior)}

	r := &hostClusterRoleResource{} // config nil: any client build panics or errors
	r.Update(ctx, resource.UpdateRequest{Plan: plan, State: state}, &resp)

	if resp.Diagnostics.HasError() {
		t.Fatalf("Update errored on a client-side-only change: %v", resp.Diagnostics)
	}
	var got hostClusterRoleModel
	if diags := resp.State.Get(ctx, &got); diags.HasError() {
		t.Fatalf("state: %v", diags)
	}
	if !got.WaitUntilConverged.ValueBool() {
		t.Fatal("wait_until_converged = false after apply; the plan's value was not stored")
	}
	if got.ID.ValueString() != "host-a/hypervisor" {
		t.Fatalf("id = %q, want host-a/hypervisor", got.ID.ValueString())
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/services/resmgr/ -run TestUpdateSkipsResmgrForClientSideChanges -count=1`
Expected: FAIL. With today's `Update`, the nil `*clients.Config` is dereferenced while building the resmgr client, so the run ends in a panic (`invalid memory address or nil pointer dereference`) — that is the current behavior reaching for resmgr.

- [ ] **Step 3: Change `Update`**

Replace the opening of `Update` — from its comment through the `if resp.Diagnostics.HasError() { return }` that follows `req.Plan.Get` — with:

```go
// Update re-PUTs the assignment when a server-side option changed: backends and
// host_cluster are options on the same role. wait_until_converged is client-
// side only, and when it is the only thing that changed there is nothing to
// send — a repeated PUT is a real write resmgr acts on, not a no-op.
func (r *hostClusterRoleResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state hostClusterRoleModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	if !roleOptionsChanged(&plan, &state) {
		plan.ID = types.StringValue(plan.HostID.ValueString() + "/" + plan.Role.ValueString())
		resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
		return
	}
```

Everything after that point (client build, body, `putRole`, `plan.ID`, the convergence wait, the final `State.Set`) stays exactly as it is.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/services/resmgr/ -run 'TestRoleOptionsChanged|TestUpdateSkipsResmgr' -count=1 -v`
Expected: all `PASS`, no panic.

- [ ] **Step 5: Static checks and commit**

Run: `gofmt -l . && go vet ./... && golangci-lint run ./... && go test ./internal/services/resmgr/... -count=1`
Expected: clean, `0 issues.`, `ok`.

```bash
git add internal/services/resmgr/host_cluster_role_resource.go internal/services/resmgr/host_cluster_role_internal_test.go
git commit -m "resmgr: a cluster role update that only flips wait_until_converged sends nothing"
```

---

### Task 3: `ImportState` writes the flag's default

**Files:**
- Modify: `internal/services/resmgr/host_cluster_role_resource.go` — `ImportState` (currently lines ~391–400)
- Test: `internal/services/resmgr/host_cluster_role_internal_test.go`

**Interfaces:**
- Consumes: `roleSchema(t)` from Task 2.

- [ ] **Step 1: Write the failing test**

Append to `host_cluster_role_internal_test.go`:

```go
// Import used to set only id/host_id/role, leaving wait_until_converged null;
// the schema default then planned `null -> false` on every imported role, and
// applying it PUT the role to resmgr. Import must write the default itself.
func TestImportStateWritesTheDefault(t *testing.T) {
	ctx := context.Background()
	s := roleSchema(t)
	resp := resource.ImportStateResponse{State: tfsdk.State{Schema: s, Raw: tftypes.NewValue(s.Type().TerraformType(ctx), nil)}}

	(&hostClusterRoleResource{}).ImportState(ctx, resource.ImportStateRequest{ID: "host-a/persistent-storage"}, &resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("import: %v", resp.Diagnostics)
	}
	var got hostClusterRoleModel
	if diags := resp.State.Get(ctx, &got); diags.HasError() {
		t.Fatalf("state: %v", diags)
	}
	if got.ID.ValueString() != "host-a/persistent-storage" || got.HostID.ValueString() != "host-a" || got.Role.ValueString() != "persistent-storage" {
		t.Fatalf("id/host_id/role = %q/%q/%q", got.ID.ValueString(), got.HostID.ValueString(), got.Role.ValueString())
	}
	if got.WaitUntilConverged.IsNull() || got.WaitUntilConverged.ValueBool() {
		t.Fatalf("wait_until_converged = %v after import, want false; null plans a spurious update", got.WaitUntilConverged)
	}
	// The server-side options are not known at import and must stay null so a
	// configuration that sets them plans the update it should.
	if !got.HostCluster.IsNull() || !got.Backends.IsNull() {
		t.Fatalf("host_cluster/backends = %v/%v after import, want null", got.HostCluster, got.Backends)
	}
}

func TestImportStateRejectsABadID(t *testing.T) {
	ctx := context.Background()
	s := roleSchema(t)
	for _, id := range []string{"", "host-a", "/hypervisor", "host-a/"} {
		resp := resource.ImportStateResponse{State: tfsdk.State{Schema: s, Raw: tftypes.NewValue(s.Type().TerraformType(ctx), nil)}}
		(&hostClusterRoleResource{}).ImportState(ctx, resource.ImportStateRequest{ID: id}, &resp)
		if !resp.Diagnostics.HasError() {
			t.Errorf("import id %q: no error, want <host_id>/<role> to be enforced", id)
		}
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/services/resmgr/ -run TestImportState -count=1`
Expected: `TestImportStateWritesTheDefault` FAILs with `wait_until_converged = <null> after import, want false`. `TestImportStateRejectsABadID` passes already (existing validation).

- [ ] **Step 3: Change `ImportState`**

Replace the body of `ImportState` so it reads:

```go
func (r *hostClusterRoleResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	parts := strings.SplitN(req.ID, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		resp.Diagnostics.AddError("Invalid import ID", fmt.Sprintf("expected <host_id>/<role>, got %q", req.ID))
		return
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), req.ID)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("host_id"), parts[0])...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("role"), parts[1])...)
	// wait_until_converged is client-side only and has no server value to read
	// back. Left null, the schema default plans `null -> false` on every
	// imported role and applying that re-PUTs the role. Write the default now so
	// the imported state already matches what the plan will hold.
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("wait_until_converged"), false)...)
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/services/resmgr/ -run TestImportState -count=1 -v`
Expected: both `PASS`.

- [ ] **Step 5: Static checks and commit**

Run: `gofmt -l . && go vet ./... && golangci-lint run ./... && go test ./internal/... -count=1 2>&1 | grep -v "no test files"`
Expected: clean, `0 issues.`, every package `ok`.

```bash
git add internal/services/resmgr/host_cluster_role_resource.go internal/services/resmgr/host_cluster_role_internal_test.go
git commit -m "resmgr: an imported cluster role plans clean"
```

---

### Task 4: Changelog

**Files:**
- Modify: `CHANGELOG.md` — insert above the line `## [0.1.10] - 2026-09-03`

- [ ] **Step 1: Add the entry**

Insert, directly above `## [0.1.10] - 2026-09-03`:

```markdown
## [Unreleased]

### Fixed

- `pcd_host_cluster_role`: importing a role no longer leaves a permanent `wait_until_converged`
  diff. Import set only `id`, `host_id` and `role`, so the flag stayed null and the schema
  default planned `null -> false` on every imported role; applying that re-PUT the role
  assignment to resmgr for a change resmgr never sees. Import now writes the default, and an
  update whose only change is `wait_until_converged` stores the value without calling resmgr.
  `host_cluster` and `backends` are still not read back, so a configuration that sets them on
  an imported role plans the update it always did.

```

- [ ] **Step 2: Check the file still reads as one document**

Run: `sed -n 7,20p CHANGELOG.md`
Expected: the new `## [Unreleased]` block followed by a blank line and `## [0.1.10] - 2026-09-03`.

- [ ] **Step 3: Commit**

```bash
git add CHANGELOG.md
git commit -m "changelog: imported cluster roles plan clean"
```

---

### Task 5: Live check on the CE lab (optional, needs the lab)

Not a substitute for Tasks 1–3; run it when the lab is free. It reproduces the exact scenario from 2026-09-03: an existing role imported into a scratch state must plan no changes on the dev build.

**Files:**
- Create (scratch, outside the repo): `<scratchpad>/import-check/main.tf`, `<scratchpad>/import-check/provider.tf`
- Uses: `~/Documents/Claude Code Projects/Terraform Provider/pcd-tf-testsuite` for credentials (its `lab.Config().tf_vars()` puts `TF_VAR_pcd_*` into the environment; never paste the password).

- [ ] **Step 1: Make sure a role exists on the lab**

The lab is empty after a rebuild. Stand the region up with the suite's fixture mode, which leaves the roles standing:

```bash
cd "/Users/pushkarmulay/Documents/Claude Code Projects/Terraform Provider/pcd-tf-testsuite"
python3 run.py lab discover
python3 run.py stage z2h --provider dev      # ~15 min; leaves blueprint/host config/cluster/roles
python3 run.py api hosts                      # note HYP1 id and its roles
```

`--provider dev` builds from `REPO_DIR` in `suite.env`, which is the main checkout. For this check point `REPO_DIR` at the worktree for the duration and restore it afterwards (`suite.env` is local, never shared).

- [ ] **Step 2: Build the dev provider and a scratch config**

```bash
S=<scratchpad>; mkdir -p "$S/import-check" "$S/bin"
cd "<worktree>" && go build -o "$S/bin/terraform-provider-pcd" .
printf 'provider_installation {\n  dev_overrides { "platform9/pcd" = "%s" }\n  direct {}\n}\n' "$S/bin" > "$S/dev.tfrc"
cp "/Users/pushkarmulay/Documents/Claude Code Projects/Terraform Provider/pcd-tf-testsuite/cases/provider.tf" "$S/import-check/provider.tf"
cat > "$S/import-check/main.tf" <<'EOF'
terraform {
  required_providers {
    pcd = { source = "platform9/pcd" }
  }
}
variable "hyp1_id" { type = string }
resource "pcd_host_cluster_role" "image_library" {
  host_id = var.hyp1_id
  role    = "image-library"
}
EOF
```

- [ ] **Step 3: Import and plan**

Run from a Python shell that exports the suite's environment, or reuse the pattern from the PCD-9783 session's `region.py` (`lab.Config().tf_vars()` + `TF_CLI_CONFIG_FILE=$S/dev.tfrc` + `TF_VAR_hyp1_id=<id>`):

```bash
terraform init -no-color
terraform import -no-color pcd_host_cluster_role.image_library "<hyp1_id>/image-library"
terraform plan -no-color
```

Expected: `No changes. Your infrastructure matches the configuration.` Before this fix the same plan reported `1 to change` with `+ wait_until_converged = false`.

- [ ] **Step 4: Tear the region back down and restore settings**

```bash
cd "/Users/pushkarmulay/Documents/Claude Code Projects/Terraform Provider/pcd-tf-testsuite"
python3 run.py stage teardown --run-dir runs/latest --provider dev
```

Restore `REPO_DIR` in `suite.env`. Nothing from this task is committed.

---

## Self-Review

- **Spec coverage.** Design §1 (import writes the default) → Task 3. Design §2 (helper + Update short-circuit, equality rules for null/""/unknown/empty/order) → Tasks 1–2; every equality rule in the spec has a table row in Task 1. Testing section: helper table test → Task 1; ImportState unit test via `tfsdk.State` → Task 3; Update no-op through framework types → Task 2; lab check as the live fallback → Task 5. Changelog → Task 4. Non-goals respected: no schema change, no read-back of `host_cluster`/`backends` (Task 3 asserts they stay null).
- **Placeholders.** None: every step carries its code or its exact command and expected output.
- **Type consistency.** `roleOptionsChanged(plan, state *hostClusterRoleModel) bool`, `hostClusterOption`, `backendsOption` are defined in Task 1 and used with the same signatures in Task 2. `roleSchema(t)` and `roleState(t, m)` are defined in Task 2 and reused in Task 3. `roleModel` and `backendsList` are defined in Task 1's test file and reused in Tasks 2–3.
