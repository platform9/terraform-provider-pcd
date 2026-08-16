# Changelog

All notable changes to this project are documented here. The format is based on
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this project adheres to
[Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

## [0.1.6] - 2026-08-15

### Fixed
- **`pcd_cluster_blueprint` silently accepted `vm_high_availability` and
  `auto_resource_rebalancing`.** These are cluster-scoped settings: the blueprint API does
  not store them (a POST carrying them returns an object without either key), and the PCD
  UI never sends them. A user who set them on the blueprint believed HA/DRR were enabled
  when nothing had happened. Both attributes are removed; they live on `pcd_cluster`, where
  they take effect. Not a breaking change for configurations that omitted them.
- **`networking_type` and `enable_distributed_routing` are now read-only.** No product
  surface exposes them (the PCD UI hardcodes `ovn`/`true`; `pcdctl` has no blueprint
  capability), yet the API requires both on create with no server default. The provider now
  supplies the product values itself and rejects attempts to configure them, so a Terraform
  user cannot put a region into a state the UI would never produce.

### Added
- **`pcd_cluster_blueprint.vnc_floating_ip`** (and on the data source): the floating IP
  through which VM VNC consoles are reached. Set `""` to clear. Works around a resmgr
  quirk where `POST /v2/blueprint` silently discards `vncFloatingIp` and only `PUT`
  persists it — on create the provider follows the POST with a PUT when the value is set.

### Changed
- `instance_shared_storage` documentation now matches the UI toggle it maps to ("Enable
  if this path is mounted as shared storage (e.g. NFS) across all hosts") and references
  `vm_storage`, so the flag is discoverable next to the path it qualifies.

## [0.1.5] - 2026-08-15

### Fixed
- **`pcd_cluster` failed with "inconsistent result after apply" whenever
  `auto_resource_rebalancing` was set with a `rebalancing_strategy`, and a partial block such as
  `{ enabled = false }` persisted `""`/`0` on the server.** resmgr does no server-side
  normalization: it stores exactly what it is sent and applies its defaults only to absent
  keys, and the request marshalled Go zero values for every leaf the user did not configure.
  Unconfigured optional leaves are now omitted from the request so the server applies its
  own defaults, and read-back keeps configured values while adopting server values only for
  leaves the config left unset. All three shapes — partial, full, and omitted — now apply
  cleanly and plan with no changes, including after a forced refresh and an in-place update.

## [0.1.4] - 2026-08-14

### Fixed
- **`pcd_host_role` could not assign a role at all.** Create and Delete issued
  `PUT`/`DELETE /resmgr/v2/hosts/<id>/roles/<name>`, but resmgr exposes no writable roles
  sub-resource on v2 and answers `404 RoleNotFound`; role assignment lives only on v1. Every
  `pcd_host_role` apply failed against PCD 2026.4. The resource now uses a v1 client.
  Blueprints and host configs are unaffected — they exist only on v2 (`/resmgr/v1/blueprint`
  and `/resmgr/v1/hostconfigs` both 404) and continue to use it.
- **`pcd_host_role` reported permanent drift**, which would have persisted even once the
  write path was fixed. Read compared the configured role name against `GET /v2/hosts/<id>`,
  whose `roles` are mapped "uber-roles" (`hypervisor`) rather than the granular `pf9-*` names
  a role is assigned by, so the match never succeeded and Terraform removed the resource from
  state and recreated it on every plan. Read now uses v1, which reports granular names.
- **`pcd_cluster_blueprint` showed a `storage_backends_json` diff on every plan.** resmgr
  echoes the blob with its own spacing and in insertion order, while `jsonencode()` emits
  compact output with sorted keys — semantically identical, textually different, so Terraform
  reported an in-place update that never converged. The read-back is now canonicalised
  (compact, keys sorted) in both the resource and the data source.

### Fixed (found by one-shot region bring-up validation)
- **`pcd_host_cluster_role.wait_until_converged` could return early during onboarding.**
  `role_status` aggregates only the roles assigned at that moment, so while several cluster
  roles were being assigned concurrently there was a window where it read `ok` before the
  others landed — un-gating downstream resources (an image upload against a Glance that was
  not serving yet). The wait now also requires the cluster role's own granular marker
  (e.g. `pf9-glance-role` for `image-library`) to report applied.
- **`pcd_cluster` creation failed on freshly deployed regions.** resmgr answers
  `500 Request Failed` to `POST /v2/clusters` until the compute control plane is warm
  (the PCD UI health-checks Nova before offering the dialog); the identical request
  succeeds minutes later. Create now retries 500s for a bounded window so a single apply
  can bring up a region from nothing.

### Added
- **New resource `pcd_host_cluster_role`** — assigns PCD *cluster roles* (`hypervisor`,
  `image-library`, `persistent-storage`, `dns`) via the resmgr v2 uber-role API, the same
  call the PCD UI onboards hosts with. The control plane expands a cluster role into its
  granular `pf9-*` roles and computes their settings from the cluster blueprint and the
  host's host configuration (`persistent-storage` takes a `backends` list naming entries in
  the blueprint's `storage_backends_json`; `hypervisor` takes `host_cluster`, which PCD
  2026.4 requires). An optional `wait_until_converged` blocks until the host reports
  `role_status = ok` — tolerating the transient `failed` flaps normal onboarding produces —
  so a single configuration can onboard a hypervisor and boot instances on it in one apply.
  Assignment and removal retry through resmgr's transient `409 RoleUpdateConflict` while a
  host is converging. This closes the gap that made a fresh region impossible to bring up
  with Terraform alone: `pcd_host_role` applies granular roles with *default* settings,
  which wedges the host on settings-bearing roles (see its documentation for when it is
  still appropriate).
- **New resource `pcd_cluster`** — manages PCD clusters (host clusters / host groups), the
  unit hypervisors join and the scope for VM high-availability, auto-rebalancing, GPU, and
  CPU-model settings. Required by `pcd_host_cluster_role`'s `hypervisor` role, whose
  `host_cluster` names it.
- `Config.ResmgrV1Client()` alongside `ResmgrV2Client()`. An `endpoint_overrides` entry for
  `resmgr` now names the service rather than one of its API versions: the required version is
  applied to it, replacing any version the override already carries, so a single override
  serves both clients.
- **`pcd_networking_network` gains `segments`** — provider-network attributes (admin only),
  mirroring `openstack_networking_network_v2`. A single segment creates a physical network
  (`network_type` `flat`/`vlan` on a `physical_network` label, optional `segmentation_id`),
  sent as top-level `provider:*` attributes; multiple segments use Neutron's multi-provider
  `segments` form. Create-only and not refreshed from the API, matching the upstream
  provider's behavior. Without this, provider networks — including any external network —
  could not be created by Terraform at all.

## [0.1.3] - 2026-08-14

### Changed
- Drop the stale "pre-release, not yet published to the Terraform Registry" status note from
  the README — the provider has been published at
  [`platform9/pcd`](https://registry.terraform.io/providers/platform9/pcd) since v0.1.0.
  Docs-only; no provider behavior change.

## [0.1.2] - 2026-08-11

### Changed
- Render the provider name as **PCD** (not `pcd`) in generated documentation titles — the
  overview page heading is now "PCD Provider" and each resource/data-source page title reads
  "… - PCD". Done via `tfplugindocs --rendered-provider-name PCD` (wired into `make generate`
  and the CI docs job). Resource/data-source *type* names (`pcd_*`) are unchanged, as is the
  provider's registry address `platform9/pcd`. Docs-only; no provider behavior change.

## [0.1.1] - 2026-08-11

### Changed
- Commit the generated registry documentation (`docs/`) so the Terraform Registry renders the
  provider's resource, data-source, and guide pages. v0.1.0 shipped without a committed `docs/`
  tree — the Registry builds documentation from the repository at the release tag, so its
  "Documentation" tab was empty. The docs are now generated with `tfplugindocs` (`make generate`)
  from the schema descriptions, `examples/`, and `templates/`, and committed. No provider
  behavior change.

## [0.1.0] - 2026-08-07

Initial public release: a first-party Terraform provider for Platform9 Private Cloud
Director, covering the OpenStack services PCD exposes plus PCD-native cluster/host
management (`resmgr`) that has no OpenStack-provider equivalent.

### Added
- Initial provider scaffold (`terraform-plugin-framework`, protocol 6), module
  `github.com/platform9/terraform-provider-pcd`, registry address `platform9/pcd`.
- Provider configuration schema with `OS_*` environment fallbacks: password, token, and
  application-credential auth; self-signed TLS support (`insecure`, `cacert_file`,
  `cert`/`key`); `region`, `endpoint_overrides`, and Keystone v3 domain/project scoping.
- gophercloud v2 client wiring (`internal/clients`) with a shared, authenticated
  `Config` handed to every resource and data source.
- Data source `pcd_identity_auth_scope` (ported from
  `openstack_identity_auth_scope_v3`) — reports the current token's user, project,
  domain, and roles.
- Verified CE 2026.4 compatibility from Step 0 preflight.
- Acceptance test harness (`internal/acctest`: protocol-6 provider factory + `PreCheck`)
  and the first acceptance test for `pcd_identity_auth_scope` (passes against the CE lab).
- `test.yml` CI: build, vet, gofmt, unit tests, and `terraform fmt` on examples.
- Identity (Keystone v3) resources: `pcd_identity_project`, `pcd_identity_role`,
  `pcd_identity_user`, `pcd_identity_role_assignment`, `pcd_identity_application_credential`.
- Identity data sources: `pcd_identity_project`, `pcd_identity_user`, `pcd_identity_role`.
- Identity groups (close api-docs coverage): `pcd_identity_group` resource + data source, and
  `pcd_identity_group_membership` (one user↔group pair per resource, mapping to
  `PUT/DELETE /groups/{id}/users/{user_id}`).
- Images (Glance v2): `pcd_images_image` resource (local-file upload + web-download import,
  status waiter, checksum verify, unprotect-before-delete, settable custom `properties`
  metadata) and `pcd_images_image` / `pcd_images_image_ids` data sources.
- Networking (Neutron v2): resources `pcd_networking_network`, `_subnet`, `_secgroup`,
  `_secgroup_rule`, `_router`, `_router_interface`; data sources `pcd_networking_network`,
  `_subnet`, `_secgroup`.
- Networking extras: `pcd_networking_port` (fixed IPs, security groups, allowed-address
  pairs, tags) and `pcd_networking_floatingip` (allocate from an external network by
  `pool` name, associate/disassociate to a port); data sources `pcd_networking_port`,
  `_port_ids`, `_router`, `_subnet_ids`, and `_floatingip`. Ports and floating IPs are
  code-complete with acceptance tests.
- Networking route and association resources: `pcd_networking_router_route` and
  `pcd_networking_subnet_route` (manage a single static/host route without disturbing
  others, serialized per parent to avoid clobbering); `pcd_networking_port_secgroup_associate`
  (attach security groups to an unmanaged port, shared or exclusive via `enforce`); and
  `pcd_networking_floatingip_associate` (bind a pre-allocated floating IP to a port).
- Compute (Nova v2): resources `pcd_compute_keypair`, `pcd_compute_flavor`,
  `pcd_compute_servergroup`, `pcd_compute_instance` (boot, in-place resize, import); data
  sources `pcd_compute_flavor`, `pcd_compute_keypair`, `pcd_compute_availability_zones`.
  The instance resource reads back all server-computed fields (availability zone,
  security groups, network) after apply, and only pushes metadata when the user manages it.
- Compute follow-ups: `pcd_compute_flavor` gains settable `extra_specs` (added/changed/
  removed in place; the flavor's other attributes are now correctly immutable);
  `pcd_compute_instance` supports in-place **resize** on a flavor change and booting by
  `image_name` (resolved via Glance, alternative to `image_id`); new resources
  `pcd_compute_interface_attach` (attach a port/network to a server) and
  `pcd_compute_volume_attach` (attach a Cinder volume to a server).
- Block storage (Cinder v3): `pcd_blockstorage_volume` resource (create/extend/import;
  code-complete — acceptance is blocked on the CE lab having no storage backend) and
  `pcd_blockstorage_volume` / `pcd_blockstorage_snapshot` data sources.
- Block storage gap resources (close api-docs coverage): `pcd_blockstorage_volume_type`
  (name/description/`is_public`/`extra_specs`, with in-place spec add/change/remove via the
  extra-specs sub-API — needs no storage backend), `pcd_blockstorage_snapshot` (was
  data-source-only; now a managed resource with async wait-for-`available`), and
  `pcd_blockstorage_volume_backup` (backup/restore lifecycle, async waiter).
- Load balancing (Octavia v2) — Phase 3: `pcd_lb_loadbalancer`, `pcd_lb_listener`,
  `pcd_lb_pool`, `pcd_lb_member`, `pcd_lb_monitor` resources and a `pcd_lb_loadbalancer`
  data source. Every child operation resolves the root load balancer and waits for its
  `provisioning_status` to return to `ACTIVE` before and after mutating (Octavia serializes
  changes per load balancer). PCD ships the **OVN** provider only, which is L4
  (TCP/UDP/SCTP); use L4 listener protocols and OVN-supported pool algorithms. L7
  policy/rule resources are omitted because the OVN provider does not support L7.
  `pcd_lb_loadbalancer` exposes `loadbalancer_provider` (defaults to `ovn`) — required
  because Octavia's server-side default provider is `amphora`, which PCD does not enable.
- DNS (Designate v2) — Phase 3: `pcd_dns_zone` and `pcd_dns_recordset` resources plus a
  `pcd_dns_zone` data source. Zone and recordset create/update/delete are asynchronous,
  so applies wait for the object to reach `ACTIVE` (and to disappear after delete).
- Key management (Barbican v1) — Phase 3: `pcd_keymanager_secret` (write-only, sensitive
  `payload`) and `pcd_keymanager_container` (grouped secrets) resources plus a
  `pcd_keymanager_secret` data source (optionally fetches the payload). Barbican identifies
  objects by URL refs; the resources expose the full ref and use the bare UUID as the ID.
- Network QoS (Neutron `qos` extension) — Phase 3: `pcd_networking_qos_policy` and its three
  rule types — `pcd_networking_qos_bandwidth_limit_rule`, `pcd_networking_qos_dscp_marking_rule`,
  `pcd_networking_qos_minimum_bandwidth_rule` — plus a `pcd_networking_qos_policy` data source.
  Rules are nested under a policy and imported by a composite `<qos_policy_id>/<rule_id>` ID.
- Project quotas — Phase 3: `pcd_compute_quotaset` (Nova), `pcd_networking_quota` (Neutron), and
  `pcd_blockstorage_quotaset` (Cinder). Each manages the per-project quota limits for its service;
  only the fields you set are managed (omitted fields keep their server value), and destroying the
  resource stops managing the quotas without resetting them to defaults (matching the upstream
  provider). Imported by a composite `<project_id>/<region>` ID (legacy bare `<project_id>` is
  also accepted). Cinder per-volume-type quotas (`volume_type_quota`) are not yet implemented.

- Cluster blueprint / host management (PCD `resmgr` API — the first non-OpenStack, non-gophercloud
  service): a thin `resmgr` v2 REST client (`clients.Config.ResmgrV2Client()`, endpoint resolved
  from the Keystone catalog, token via the shared ProviderClient) plus `pcd_host_config` (interface
  ↔ traffic-type mapping and physical-network labels), `pcd_host_role` (assign a role such as
  `pf9-ostackhost-neutron` to a host), `pcd_host_config_assignment` (attach a host config to a host),
  and a `pcd_cluster_blueprint` data source (read a blueprint by name).
- `pcd_cluster_blueprint` resource — manage a cluster blueprint (networking, image library, VM
  storage, HA/rebalancing, and Cinder backends). PCD supports one blueprint per region, so the
  usual workflow is to `terraform import` the existing blueprint and manage it in place (in-place
  update verified live). The write API requires the whole object, so all attributes are
  `Optional+Computed` and round-tripped; `storage_backends_json` is sensitive (driver credentials)
  and is read back so writes preserve the current backends unless you change it.
- Registry documentation generation wired via `tfplugindocs` (`make generate`) — renders
  `docs/` for every resource and data source plus the provider index from schema
  descriptions. Generated docs are produced on demand / at release and are not committed.
- CI: `golangci-lint` (v2) and a docs-generation smoke test added to `test.yml`.
- Documentation examples and guides: a self-contained Terraform `examples/` snippet for
  every resource and data source (with `import.sh` for importable resources), rendered as
  the "Example Usage" and "Import" sections; `templates/` add a **subcategory** to each
  page so the registry groups resources by service (Identity, Images, Networking, Compute,
  Block Storage); and two guides — **Authentication** and **Migrating from
  terraform-provider-openstack** (authored in `templates/guides/`).

- Provider `cloud` (clouds.yaml) support: when `cloud` (or `OS_CLOUD`) is set, auth
  defaults are sourced from a `clouds.yaml` entry (searched at `$OS_CLIENT_CONFIG_FILE`,
  `./clouds.yaml`, `~/.config/openstack/clouds.yaml`, `/etc/openstack/clouds.yaml`).
  Precedence is explicit config > `OS_*` env > `clouds.yaml`.

### Known gaps
- `max_retries` / retry transport and per-resource `region` override are stubs pending
  Phase 1.
