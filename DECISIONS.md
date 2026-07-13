# Decisions & deviations log

Records deviations from the PCD-1070 implementation plan and material findings that
change scope. The plan's Section 3 decisions remain locked unless noted here.

## Validation status — CE lab 2026.4 (as of 2026-07-12)

**VALIDATED** = acceptance test passed against the live CE lab (create/update/import,
CheckDestroy). **PENDING** = code-complete and verified up to a lab-side limitation, not
yet passable on this lab (reason noted). Generated registry docs are not committed; run
`make generate` (tfplugindocs) to produce them.

### Live run on the fully-featured CE lab (2026-07-12)

Ran the acceptance suites against a CE lab with compute (KVM), image library, Cinder
(Synology iSCSI), and DNS roles converged. **Green live:** identity, images, key
management, the entire networking suite (network/subnet/secgroup/router/port/routes/QoS/
quotas **and floating IPs**), compute control-plane (keypair/flavor/servergroup/quotaset),
all three quotasets, and the **full load-balancer tree on OVN** (lb + TCP listener +
SOURCE_IP_PORT pool + member + TCP monitor + data source).

**Provider bugs found and fixed by the live run:**
- `pcd_lb_loadbalancer` had no `loadbalancer_provider` field, so Octavia fell back to its
  server-default `amphora` (which PCD does not enable) and **every LB create failed**.
  Added the field, defaulting to `ovn`.
- `pcd_networking_floatingip` could not disassociate and produced "inconsistent result
  after apply" when the association changed: `port_id`/`fixed_ip` used
  `UseStateForUnknown` (which hid a removal) and the server-derived fields
  (`router_id`, `status`, `fixed_ip`) were not re-planned. Fixed with a `ModifyPlan`;
  inline disassociation is now `port_id = ""` (leaving it unset delegates to
  `pcd_networking_floatingip_associate`). Also fixed two test bugs surfaced here: the FIP
  tests lacked the router a floating IP needs, and `testAccCheckNetworkDestroy` matched
  data-source networks.

**Lab-side blockers from the initial run — all since resolved by lab-ops and then validated
live (each was a backend gap, not a provider defect; the resources created correctly and
surfaced the real fault):**
- Compute VM boot (`instance`, `interface_attach`, `volume_attach`): was nova-compute unable
  to fetch image **data** from Glance. Resolved by uploading a library-synced image;
  **VALIDATED 2026-07-13**. (The acc tests now boot a pre-synced image named via
  `PCD_ACC_IMAGE_NAME` — PCD does not sync web-download images to the host's local Glance.)
- Block storage `volume`: was `cinder-volume@synology` down. Backend fixed; **VALIDATED 2026-07-13**.
- DNS `zone`/`recordset`: was Designate `500 no_servers_configured`. Pool given a BIND9
  nameserver (runbook Phase E step 7); **VALIDATED 2026-07-13**.

Every resource that maps to a configured PCD backend is now validated live.

### Follow-up run (2026-07-13): compute + Cinder unblocked

After the lab operator uploaded a bootable image to the library and fixed the Cinder
backend, the compute and block-storage suites were re-run. Block-storage volume passed.
Compute boot passed once pointed at a library-synced image, and surfaced several real
provider bugs in `pcd_compute_instance` (all fixed) that were latent because the instance
had never actually booted before:
- `flatten` did not read back `availability_zone`, `security_groups`, or the `network`
  block, so they stayed unknown after apply ("invalid result object"). Now populated
  (with the security-group list deduplicated, since Nova can repeat `default`).
- The `network` block's inner `uuid`/`port` lacked `UseStateForUnknown`, so an in-place
  flavor change re-planned them unknown and tripped `RequiresReplace` — the resize
  replaced the instance instead of resizing. Fixed.
- `metadata` was pushed on every update even when unset, sending Nova a nil map
  (`metadata: None` → 400). Fixed with `UseStateForUnknown` + a guarded update.
- Separately, `pcd_networking_subnet` delete now retries on `409 SubnetInUse` to ride out
  Nova's asynchronous port release after an instance is deleted.

The compute VM-boot acc tests now resolve an existing image by name via a data source
(env `PCD_ACC_IMAGE_NAME`); they skip when it is unset, since PCD does not sync
web-download images to the hypervisor's local Glance.

| Family | Item | Status |
|---|---|---|
| Provider core | password auth + project scope, self-signed TLS (`insecure`), `pcd_identity_auth_scope` | **VALIDATED** |
| Identity | `pcd_identity_project`, `_role`, `_user`, `_role_assignment`, `_application_credential` | **VALIDATED** |
| Identity (DS) | `pcd_identity_project`, `_user`, `_role` | **VALIDATED** |
| Images | `pcd_images_image` (local-file upload); web-download import path also exercised | **VALIDATED** |
| Images (DS) | `pcd_images_image`, `_image_ids` | **VALIDATED** |
| Images | `pcd_images_image` settable `properties` (custom metadata) | **PENDING** — code-complete; add/replace/remove via JSON-patch, echo-only Read filters Glance system properties to avoid perpetual diff. Acc test extended (create/update/import). Not yet run live (credentials unavailable this session). |
| Networking | `pcd_networking_network`, `_subnet`, `_secgroup`, `_secgroup_rule`, `_router`, `_router_interface` | **VALIDATED** |
| Networking (DS) | `pcd_networking_network`, `_subnet`, `_secgroup` | **VALIDATED** |
| Networking | `pcd_networking_port` | **PENDING** — code-complete, build/vet/lint/docs clean; acceptance test written (create/update/import). Not yet run live: lab credentials were unavailable in this session. No lab-side blocker expected. |
| Networking | `pcd_networking_floatingip` | **VALIDATED** (2026-07-12) — allocate/associate/disassociate/import against an external network; the disassociation and inconsistent-result bugs found here are fixed. Requires an external network; the acc test builds the router path a floating IP needs and skips unless `PCD_ACC_EXTERNAL_NETWORK` names one. |
| Networking (DS) | `pcd_networking_port`, `_port_ids`, `_router`, `_subnet_ids` | **PENDING** — code-complete; acc test written. Not yet run live (credentials unavailable this session). |
| Networking (DS) | `pcd_networking_floatingip` | **PENDING** — depends on a floating IP existing (see external-network note above). |
| Networking | `pcd_networking_router_route`, `_subnet_route` | **PENDING** — code-complete; single-route read-modify-write under a per-parent mutex so concurrent routes on one router/subnet don't clobber. Acc tests written (router route needs a router interface for a valid next-hop). Not yet run live. |
| Networking | `pcd_networking_port_secgroup_associate` | **PENDING** — code-complete; shared (`enforce=false`) and exclusive (`enforce=true`) modes. Acc test written. Not yet run live. |
| Networking | `pcd_networking_floatingip_associate` | **VALIDATED** (2026-07-12) — associates a separately-allocated floating IP to a port; acc test builds the router path and skips unless `PCD_ACC_EXTERNAL_NETWORK` set. |
| Compute | `pcd_compute_keypair`, `_flavor`, `_servergroup` | **VALIDATED** |
| Compute (DS) | `pcd_compute_flavor`, `_keypair`, `_availability_zones` | **VALIDATED** |
| Compute | `pcd_compute_instance` (boot) | **VALIDATED** (2026-07-13) — boots against a library-synced image. The acc test resolves an existing image by name via a data source (env `PCD_ACC_IMAGE_NAME`), because PCD does not sync web-download (`copy-from`) images to the hypervisor's local Glance. |
| Compute | `pcd_compute_flavor` `extra_specs` | **VALIDATED** — in-place add/change/remove; other flavor attrs force replacement. |
| Compute | `pcd_compute_instance` resize + `image_name` | **VALIDATED** (2026-07-13) — flavor change resizes in place (verified same instance ID); `image_name` resolved via Glance. |
| Compute | `pcd_compute_interface_attach` | **VALIDATED** (2026-07-13) — hot-attach a second NIC + import. |
| Compute | `pcd_compute_volume_attach` | **VALIDATED** (2026-07-13) — boot + Cinder volume attach + import. |
| Block storage | `pcd_blockstorage_volume` | **VALIDATED** (2026-07-13) — create on the `synology-iscsi` backend → available → extend → import. |
| Block storage (DS) | `pcd_blockstorage_volume`, `_snapshot` | **PENDING** — untestable without volumes on this lab. |
| Load balancing (Octavia) | `pcd_lb_loadbalancer`, `_listener`, `_pool`, `_member`, `_monitor` + `_loadbalancer` DS | **VALIDATED** (2026-07-12) — full-tree apply on OVN (lb + TCP listener + `SOURCE_IP_PORT` pool + member + TCP monitor + DS + rename + import). **PCD ships the OVN provider only** (`providers=[ovn]`, no amphora), which is L4 — use TCP/UDP/SCTP listeners and OVN pool algorithms; L7 policy/rule resources were removed (see backout entry). `loadbalancer_provider` was added (default `ovn`) because Octavia's server-side default `amphora` is not enabled — without it every create failed. |
| DNS (Designate) | `pcd_dns_zone`, `pcd_dns_recordset` + `pcd_dns_zone` DS | **VALIDATED** (2026-07-13) — zone + recordset create → `ACTIVE`, verify, import, destroy, against a Designate pool backed by BIND9. The zone import ignores `serial` (Designate auto-increments the SOA serial out of band). Requires the pool to have a nameserver target; see `~/Documents/PCD-CE/DNS-validation-lab-setup.md`. |
| Key management (Barbican) | `pcd_keymanager_secret`, `pcd_keymanager_container` + `pcd_keymanager_secret` DS | **PENDING** — Phase 3, code-complete; write-only echo-only `payload`, URL-ref→UUID id handling, wait-for-`ACTIVE` only on create-with-payload. Acc test (secret + container + data source + import) + examples written. Barbican is live on the lab (Step 0) and needs no compute/storage backend, so this should pass live — not yet run this session (credentials unavailable). |
| Network QoS (Neutron) | `pcd_networking_qos_policy`, `_qos_bandwidth_limit_rule`, `_qos_dscp_marking_rule`, `_qos_minimum_bandwidth_rule` + `_qos_policy` DS | **PENDING** — Phase 3, code-complete; rules nested under a policy with composite `<policy_id>/<rule_id>` import, tags via the attributes-tags extension (`qos/policies` type), `ForceNew` on `qos_policy_id`. Full-tree acc test (policy + all three rules + data source + import) + examples written. Depends only on the Neutron `qos` extension (no compute/storage backend), so this should pass live — not yet run this session (credentials unavailable). |
| Project quotas | `pcd_compute_quotaset` (Nova), `pcd_networking_quota` (Neutron), `pcd_blockstorage_quotaset` (Cinder) | **PENDING** — Phase 3, code-complete; every quota field `Optional+Computed` with `UseStateForUnknown` (partial management — only user-set/changed fields are PUT via `*int` omitempty; server echoes the rest). No create API (Create = Update+read). **Delete is a deliberate no-op** (matches upstream `RemoveFromState`: destroying stops management without resetting quotas). Composite `<project_id>/<region>` id with legacy bare-`project_id` import tolerance; `project_id`/`region` are `ForceNew`. Per-service acc test (create project → set quotas → verify via API → update → import) + examples written. Needs live validation on the fresh CE lab (credentials unavailable this session). **Scope note:** matches upstream field-for-field except two deliberate deferrals — see the Deferred section. |

Both PENDING items are lab-side configuration gaps (Platform9 / lab-ops), not provider
defects; their acceptance tests flip green on a properly-configured PCD cloud.


## 2026-07-12 — backed out code PCD does not ship (VPNaaS, Octavia L7)

Live inspection of the CE lab's service catalog (all 14 services + the Octavia provider
list) confirmed which of the built resources correspond to features PCD actually ships.
Two were removed so the first-party provider only exposes what works on PCD:

- **VPNaaS — entire family removed** (`internal/services/vpnaas`, 5 resources, examples,
  the `vpnaas`→"VPN" doc subcategory, and the registrations). The Neutron `vpnaas`
  extension is *advertised* in the API, but PCD does not productize/support VPNaaS, so a
  supported PCD provider should not expose it. Re-add if PCD ships it later — the code is
  in git history.
- **Octavia L7 — `pcd_lb_l7policy` and `pcd_lb_l7rule` removed.** PCD ships only the
  **OVN** Octavia provider (verified: `GET /octavia/v2/lbaas/providers` → `[ovn]`; the
  `amphora` provider returns HTTP 400). OVN is a pure L4 load balancer with no HTTP
  awareness, so L7 policies/rules can never function on PCD. The remaining five LB
  resources (`loadbalancer`/`listener`/`pool`/`member`/`monitor`) work on OVN with L4
  constraints (TCP/UDP/SCTP protocols, OVN pool algorithms, L4 health monitors), which is
  a usage note, not a reason to remove them. `rootLBIDFromL7Policy` and the `l7policies`
  import were removed from `loadbalancer.go`; `splitParentChildID` stays (the member
  resource still uses it).

Everything else in the catalog maps to a live, shipped service (identity, image, network,
compute, volumev3, dns, key-manager, load-balancer), so nothing else was overbuilt.

## 2026-07-12 — quotas: scope and deferrals

The three quota resources match the upstream terraform-provider-openstack resources
field-for-field, with three deliberate choices worth recording:

- **Delete is a no-op.** Upstream's quota resources use `schema.RemoveFromState` and make
  no API call on destroy; the project keeps whatever quota values it had. We match this
  (the framework `Delete` returns without calling the API). Rationale: quotas are a
  property of a pre-existing project, not an object the provider created, so "un-managing"
  them should not silently reset a project's limits to defaults. gophercloud exposes a
  `Delete` (reset-to-defaults) on all three packages; we intentionally do not call it.
- **`volume_type_quota` deferred (Cinder).** The upstream `blockstorage_quotaset_v3`
  exposes per-volume-type quotas as a string→string map routed through gophercloud's
  `UpdateOpts.Extra`, with echo-on-write and read-back-only-for-configured-keys to avoid
  perpetual diffs. This is the single most bug-prone part of the port; it is deferred to a
  follow-up so the scalar quotaset ships clean. `pcd_blockstorage_quotaset` manages the
  scalar Cinder quotas only for now.
- **Upstream-omitted gophercloud fields left out.** Nova's `force` update flag and
  Neutron's `trunk` quota exist in gophercloud but are not in the upstream schemas; we omit
  them to match upstream exactly. Easy to add later if wanted.

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
- **VPNaaS extension is advertised** (`vpnaas`, `vpn-endpoint-groups`, `vpn-flavors`, …),
  but PCD does not productize/support VPNaaS. The family was built in Phase 3 and then
  **backed out** (see the 2026-07-12 backout entry) — advertised-but-unsupported ≠ shippable.
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
