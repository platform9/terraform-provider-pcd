# terraform-provider-pcd

A first-party Terraform provider for **Platform9 Private Cloud Director (PCD)**.

It manages the OpenStack services PCD exposes — compute (Nova), networking (Neutron),
block storage (Cinder), images (Glance), identity (Keystone) — plus PCD-specific
surfaces: host and cluster management (`resmgr`), VM leases (`mors`), and VM high
availability (`hamgr`).

> **Status: pre-release, under active development.** Not yet published to the Terraform
> Registry. Resource coverage is being built out in phases (core IaaS parity →
> PCD-specific → extended parity).

## Compatibility

| Component | Version |
|---|---|
| Minimum PCD | 2026.4 (Community Edition baseline; PCD tracks OpenStack SLURP releases) |
| Minimum Terraform | 1.0 (provider protocol 6) |
| Go | 1.25 (builds on 1.26) |
| SDK | terraform-plugin-framework, gophercloud/v2 |

## Authentication

Provider attribute names and `OS_*` environment fallbacks mirror
`terraform-provider-openstack`, so migrating a configuration is mechanical.

```hcl
provider "pcd" {
  auth_url    = "https://pcd.example.com/keystone/v3"
  region      = "Infra"
  user_name   = "admin@example.localnet"
  password    = var.pcd_password
  tenant_name = "service"

  user_domain_id    = "default"
  project_domain_id = "default"

  insecure = true # CE uses a self-signed certificate
}
```

All attributes may instead be supplied via `OS_AUTH_URL`, `OS_USERNAME`, `OS_PASSWORD`,
`OS_PROJECT_NAME`, `OS_REGION_NAME`, `OS_INSECURE`, etc. Password, token, and application
credential auth are supported.

## Development

Requires Go 1.25+.

```bash
make build      # go build ./...
make test       # unit tests (no lab required)
make testacc    # acceptance tests (TF_ACC=1; requires a reachable PCD lab, see below)
make lint       # golangci-lint
make generate   # regenerate docs/ with tfplugindocs
```

### Local dev overrides

To test an unreleased build without the registry, add to `~/.terraformrc`:

```hcl
provider_installation {
  dev_overrides {
    "platform9/pcd" = "/path/to/your/GOBIN"
  }
  direct {}
}
```

Then `make install` and run Terraform against a config that references `platform9/pcd`.

## License

Mozilla Public License 2.0 — see [LICENSE](LICENSE). Portions are ported from
[`terraform-provider-openstack`](https://github.com/terraform-provider-openstack/terraform-provider-openstack)
(MPL-2.0) and carry provenance comments.
