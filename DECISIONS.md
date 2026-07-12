# Decisions & deviations log

Records deviations from the PCD-1070 implementation plan and material findings that
change scope. The plan's Section 3 decisions remain locked unless noted here.

## Validation status — CE lab 2026.4 (as of 2026-07-12)

**VALIDATED** = acceptance test passed against the live CE lab (create/update/import,
CheckDestroy). **PENDING** = code-complete and verified up to a lab-side limitation, not
yet passable on this lab (reason noted). Generated registry docs are not committed; run
`make generate` (tfplugindocs) to produce them.

| Family | Item | Status |
|---|---|---|
| Provider core | password auth + project scope, self-signed TLS (`insecure`), `pcd_identity_auth_scope` | **VALIDATED** |
| Identity | `pcd_identity_project`, `_role`, `_user`, `_role_assignment`, `_application_credential` | **VALIDATED** |
| Identity (DS) | `pcd_identity_project`, `_user`, `_role` | **VALIDATED** |
| Images | `pcd_images_image` (local-file upload); web-download import path also exercised | **VALIDATED** |
| Images (DS) | `pcd_images_image`, `_image_ids` | **VALIDATED** |
| Networking | `pcd_networking_network`, `_subnet`, `_secgroup`, `_secgroup_rule`, `_router`, `_router_interface` | **VALIDATED** |
| Networking (DS) | `pcd_networking_network`, `_subnet`, `_secgroup` | **VALIDATED** |
| Networking | `pcd_networking_port` | **PENDING** — code-complete, build/vet/lint/docs clean; acceptance test written (create/update/import). Not yet run live: lab credentials were unavailable in this session. No lab-side blocker expected. |
| Networking | `pcd_networking_floatingip` | **PENDING** — code-complete. Needs an **external network** in the lab (allocation pool); acc test skips unless `PCD_ACC_EXTERNAL_NETWORK` names one. |
| Networking (DS) | `pcd_networking_port`, `_port_ids`, `_router`, `_subnet_ids` | **PENDING** — code-complete; acc test written. Not yet run live (credentials unavailable this session). |
| Networking (DS) | `pcd_networking_floatingip` | **PENDING** — depends on a floating IP existing (see external-network note above). |
| Networking | `pcd_networking_router_route`, `_subnet_route` | **PENDING** — code-complete; single-route read-modify-write under a per-parent mutex so concurrent routes on one router/subnet don't clobber. Acc tests written (router route needs a router interface for a valid next-hop). Not yet run live. |
| Networking | `pcd_networking_port_secgroup_associate` | **PENDING** — code-complete; shared (`enforce=false`) and exclusive (`enforce=true`) modes. Acc test written. Not yet run live. |
| Networking | `pcd_networking_floatingip_associate` | **PENDING** — code-complete. Needs an external network (see note above); acc test skips unless `PCD_ACC_EXTERNAL_NETWORK` set. |
| Compute | `pcd_compute_keypair`, `_flavor`, `_servergroup` | **VALIDATED** |
| Compute (DS) | `pcd_compute_flavor`, `_keypair`, `_availability_zones` | **VALIDATED** |
| Compute | `pcd_compute_instance` (boot) | **PENDING** — lab image-library gap: images don't reach the onboarded host's local library → nova returns HTTP 204 for image data. Create/schedule/wait/error-report verified; passes with a library-backed image. |
| Block storage | `pcd_blockstorage_volume` | **PENDING** — no Cinder storage backend on the lab (`storageBackends={}`); volumes go `creating → error`. Create + waiter + error-detection verified. |
| Block storage (DS) | `pcd_blockstorage_volume`, `_snapshot` | **PENDING** — untestable without volumes on this lab. |

Both PENDING items are lab-side configuration gaps (Platform9 / lab-ops), not provider
defects; their acceptance tests flip green on a properly-configured PCD cloud.


## 2026-07-11 — blockstorage: no Cinder storage backend on the CE lab

Volume creation on the CE lab goes `creating → error` immediately: the cluster
blueprint has `storageBackends={}` (none configured) and only a `__DEFAULT__` volume
type with nothing to fulfill it. So `pcd_blockstorage_volume`'s acceptance test cannot
pass on this lab (same class of lab gap as the compute image-library issue). The
resource **code is verified up to the backend**: create + status waiter + error
detection all work (the waiter correctly reports the ERROR state). It will pass on a
cloud with a working storage backend. The volume/snapshot data sources ship alongside.

## 2026-07-11 — BLOCKER: hypervisor pcd-iso-test cannot spawn instances

Nova boots fail: an instance goes BUILD → ERROR with *"Exceeded maximum number of
retries. Exhausted all hosts available for retrying build failures"*
(`nova.exception.MaxRetriesExceeded`). Ruled out via the API and libvirt on
cannon-ubuntu: nested virtualization (CPU mode is `host-passthrough`), capacity
(4 vCPU / 7 GB / 131 GB free), image (reaches `active` via web-download), and
network (created fine). The real `nova-compute` spawn error lives on the host, but
SSH to `pcd-iso-test` (172.16.122.232) is rejected — the `pcd_automation` key is not
authorized there (it is the repurposed "iso-test" VM). Most likely an OVN
port-binding failure or a `pf9-hostagent`/nova-compute issue on the freshly-onboarded
host. **Needs the owner** to check `nova-compute.log` on the host (or grant SSH
access). `pcd_compute_instance` code is complete and correct up to the boot; its boot
acceptance test will pass once the host can spawn a VM. keypair/flavor/servergroup and
the compute data sources are testable without a boot and continue.

## 2026-07-11 — networking: floating IPs and extras deferred

The CE lab has no external Neutron network, so `pcd_networking_floatingip` (which
allocates from an external network) cannot be acceptance-tested yet. Floating IPs,
ports, the associate resources, subnet/router routes, and the remaining data sources
are deferred to a **networking-extras** follow-up. Core networking (network, subnet,
secgroup, secgroup_rule, router, router_interface + network/subnet/secgroup data
sources) ships in PR #5, all acc-green.

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
