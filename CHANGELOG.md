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

### Known gaps
- `cloud` (clouds.yaml) is declared but not yet implemented; it errors if set.
- `max_retries` / retry transport and per-resource `region` override are stubs pending
  Phase 1.
