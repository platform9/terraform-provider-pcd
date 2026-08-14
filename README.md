# terraform-provider-pcd

A first-party Terraform provider for **Platform9 Private Cloud Director (PCD)**.

It manages the OpenStack services PCD exposes — identity (Keystone), compute (Nova),
networking (Neutron, including QoS and quotas), block storage (Cinder), images (Glance),
load balancing (Octavia / OVN), DNS (Designate), and key management (Barbican) — **and,
uniquely, the PCD-native infrastructure the standard OpenStack provider cannot manage:
cluster blueprints and host configuration/roles via the Platform9 `resmgr` API** (see
[PCD-native resources](#pcd-native-resources)).

## PCD-native resources

These are the value-add over [`terraform-provider-openstack`](https://github.com/terraform-provider-openstack/terraform-provider-openstack):
they manage Platform9's own control plane (the `resmgr` service), so you can declare a
PCD region's cluster topology and host onboarding as code — something no OpenStack provider
can do.

| Resource | What it manages |
|---|---|
| `pcd_cluster_blueprint` | The region's **cluster blueprint** — the shared config every virtualized cluster inherits: networking type (OVN/OVS), virtual-network segmentation range, image-library and VM storage, Cinder backends, VM HA, and auto-rebalancing. PCD keeps one per region, so the usual flow is `terraform import` then manage in place. Also available read-only as the `pcd_cluster_blueprint` data source. |
| `pcd_host_config` | A **host configuration** — the mapping of each traffic type (management, VM console, tunneling, image library, live migration) to a network interface, plus physical-network labels. |
| `pcd_host_role` | Assigns a **PCD role** (e.g. `pf9-ostackhost-neutron`) to an onboarded host. |
| `pcd_host_config_assignment` | Attaches a host configuration to a host. |

Everything else mirrors `terraform-provider-openstack` closely (attribute names, import IDs,
`OS_*` env), so migrating an existing OpenStack configuration is largely mechanical.

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
