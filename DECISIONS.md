# Decisions & deviations log

Records deviations from the PCD-1070 implementation plan and material findings that
change scope. The plan's Section 3 decisions remain locked unless noted here.

## 2026-07-11 — Gate A met; images `properties` read-only

The user onboarded hypervisor `pcd-iso-test` (the earlier `pcd-ce-jul-hyp` name was a
mistake). Nova now shows it `state=up` (4 vCPU / 7.6 GB, `nova-compute` up), so **Gate A
is met** and compute is unblocked.

`pcd_images_image` exposes `properties` as a **computed (read-only)** map for now. Glance
injects system properties (`os_hash_*`, `stores`, …) that fight a user-managed map and
cause perpetual diffs; settable custom properties with a system-property ignore-list is a
follow-up.

## 2026-07-10 — Step 0 preflight findings (live CE 2026.4)

Full evidence: [`docs/compatibility/ce-2026.4.md`](docs/compatibility/ce-2026.4.md).

### Scope reclassifications (from the live Neutron extension dump — 91 aliases)
- **BGP dynamic routing not enabled.** No `bgp`/`bgp-speaker` extension present.
  Plan §8.2 marked `networking_bgp_speaker`/`networking_bgp_peer` as *parity* — reclassed
  to **NA/conditional** for CE 2026.4.
- **Segments not enabled.** No `segment`/`network-segment-range` extension present. Plan
  §8.2 marked `networking_segment` *parity* — reclassed to **conditional**, pending
  re-probe on other PCD deployments.
- **VPNaaS is enabled** (`vpnaas`, `vpn-endpoint-groups`, `vpn-flavors`, …). Plan §8.3
  guessed "likely disabled." VPNaaS family is **shippable** (Phase 3).
- **Address groups, trunk, floating-IP port forwarding present** → the corresponding
  `[EXT]` conditional resources (§8.2/§8.3) are **confirmed shippable**.
- **FWaaS / BGPVPN / TaaS absent** → **NA confirmed** (matches plan's expectation).

### Liveness gates resolved (positive)
- **Barbican, Octavia, Designate are all live** (HTTP 200). Key-manager, load-balancer,
  and DNS families are viable; the plan's `[CAT]`-vs-docs uncertainty (§8.7–§8.9) closes
  positive.

### Still requiring discovery ([DISC], Section 7)
- **hamgr**: alive but `GET /hamgr/v1/` → 404; root returns `true` (liveness only). API
  shape unknown; VM HA continues to ship via the `pcd_cluster_blueprint` attribute.
- **mors**: alive (Flask/werkzeug 404 on probed paths); routes to be derived from
  `github.com/platform9/pf9-mors` in Phase 2.

### Lab state divergence from the handoff (§8)
- A second, experimental host `afc7bdb1…` (`pcd-host-01`, 2 vCPU) now holds the full
  compute+OVN+glance role set, but its **nova-compute is `down`** → no live capacity.
- The handoff's documented 8 vCPU hypervisor `0d7f321b…` is still `roles: []`.
- Glance images and Neutron networks are both empty; an unrelated `pcd-iso-test` libvirt
  VM is running. **Which host becomes the working hypervisor is an owner decision**
  (deferred to the compute-test phase).

## 2026-07-10 — Workplan interleave (Gate A deferred behind Gate B)

Plan §15 orders Gate A (a VM boots) before Gate B (scaffold + auth-scope data source).
Scaffolding, provider config/auth, and `pcd_identity_auth_scope` only need **read auth**,
which is verified working. Given the lab-capacity divergence above, we **start the repo
build-out through Gate B now** and resolve the hypervisor/Gate A question when compute
tests arrive. Low risk; no locked decision affected.

## 2026-07-10 — Toolchain / dependency versions

- **Go**: plan pins 1.25.x; the build host has 1.26.3. `go.mod` uses `go 1.25.8`; builds
  cleanly on 1.26. No action needed.
- **gophercloud/v2**: resolved to **v2.13.0** (plan referenced v2.12.0). Using latest;
  API is compatible.
- **terraform-plugin-framework**: **v1.19.0** as planned.

## 2026-07-10 — Gate B passed; CI scope for now

`TestAccIdentityAuthScopeDataSource_basic` passes against the live lab (terraform
v1.15.8 + terraform-plugin-testing v1.16.0). `test.yml` CI currently runs
build/vet/gofmt/unit + `terraform fmt` on examples — all verified green locally.
**golangci-lint and the `make generate` docs-drift gate are deferred** to the docs phase
(they need `tools/` + a golangci-lint v2 config); tracked so CI stays green until then.

Terraform was installed from the official `releases.hashicorp.com` prebuilt binary
(v1.15.8, darwin_arm64) because `brew install hashicorp/tap/terraform` failed on outdated
Xcode Command Line Tools on the build host.

## 2026-07-10 — `cloud` (clouds.yaml) deferred

Declared in the provider schema for config-parity with `terraform-provider-openstack`,
but not yet wired. Setting it produces a clear error rather than silently mis-configuring.
To be implemented (via `gophercloud/utils/v2` clientconfig) in Phase 1.
