# Changelog

All notable changes to this project are documented here. The format is based on
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this project adheres to
[Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

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
- Committed CE 2026.4 compatibility evidence from Step 0 preflight
  (`docs/compatibility/ce-2026.4.md`).
- Acceptance test harness (`internal/acctest`: protocol-6 provider factory + `PreCheck`)
  and the first acceptance test for `pcd_identity_auth_scope` (passes against the CE lab).
- `test.yml` CI: build, vet, gofmt, unit tests, and `terraform fmt` on examples.
- Identity (Keystone v3) resources: `pcd_identity_project`, `pcd_identity_role`,
  `pcd_identity_user`, `pcd_identity_role_assignment`, `pcd_identity_application_credential`.
- Identity data sources: `pcd_identity_project`, `pcd_identity_user`, `pcd_identity_role`.
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
  code-complete with acceptance tests; see DECISIONS.md for live-validation status.
- Networking route and association resources: `pcd_networking_router_route` and
  `pcd_networking_subnet_route` (manage a single static/host route without disturbing
  others, serialized per parent to avoid clobbering); `pcd_networking_port_secgroup_associate`
  (attach security groups to an unmanaged port, shared or exclusive via `enforce`); and
  `pcd_networking_floatingip_associate` (bind a pre-allocated floating IP to a port).
- Compute (Nova v2): resources `pcd_compute_keypair`, `pcd_compute_flavor`,
  `pcd_compute_servergroup` (acceptance-tested); `pcd_compute_instance` (code-complete —
  boot verification is blocked on a lab image-library issue, see DECISIONS.md); data
  sources `pcd_compute_flavor`, `pcd_compute_keypair`, `pcd_compute_availability_zones`.
- Compute follow-ups: `pcd_compute_flavor` gains settable `extra_specs` (added/changed/
  removed in place; the flavor's other attributes are now correctly immutable);
  `pcd_compute_instance` supports in-place **resize** on a flavor change and booting by
  `image_name` (resolved via Glance, alternative to `image_id`); new resources
  `pcd_compute_interface_attach` (attach a port/network to a server) and
  `pcd_compute_volume_attach` (attach a Cinder volume to a server).
- Block storage (Cinder v3): `pcd_blockstorage_volume` resource (create/extend/import;
  code-complete — acceptance is blocked on the CE lab having no storage backend, see
  DECISIONS.md) and `pcd_blockstorage_volume` / `pcd_blockstorage_snapshot` data sources.

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
