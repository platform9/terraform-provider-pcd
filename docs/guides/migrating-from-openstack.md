---
page_title: "Migrating from terraform-provider-openstack - PCD Provider"
subcategory: ""
description: |-
  How to move a terraform-provider-openstack configuration to the PCD provider.
---

# Migrating from terraform-provider-openstack

The PCD provider is ported from
[terraform-provider-openstack](https://github.com/terraform-provider-openstack/terraform-provider-openstack),
so migrating an existing configuration is mostly mechanical: the provider
configuration and most resource schemas are intentionally the same. This guide
covers the naming changes and the handful of behavioral differences.

## Provider block

Authentication arguments are identical — only the provider name changes. See the
[Authentication guide](authentication.md) for the full set.

```terraform
# Before
provider "openstack" {
  auth_url    = "https://pcd.example.com/keystone/v3"
  user_name   = "admin"
  password    = var.password
  tenant_name = "service"
  region      = "RegionOne"
}

# After
provider "pcd" {
  auth_url    = "https://pcd.example.com/keystone/v3"
  user_name   = "admin"
  password    = var.password
  tenant_name = "service"
  region      = "RegionOne"
}
```

## Resource and data source names

Two mechanical changes to every type name:

1. Replace the `openstack_` prefix with `pcd_`.
2. Drop the trailing API-version suffix (`_v2` / `_v3`).

So `openstack_networking_network_v2` becomes `pcd_networking_network`.

| terraform-provider-openstack | PCD |
|---|---|
| `openstack_identity_project_v3` | `pcd_identity_project` |
| `openstack_identity_user_v3` | `pcd_identity_user` |
| `openstack_identity_role_v3` | `pcd_identity_role` |
| `openstack_identity_role_assignment_v3` | `pcd_identity_role_assignment` |
| `openstack_identity_application_credential_v3` | `pcd_identity_application_credential` |
| `openstack_images_image_v2` | `pcd_images_image` |
| `openstack_networking_network_v2` | `pcd_networking_network` |
| `openstack_networking_subnet_v2` | `pcd_networking_subnet` |
| `openstack_networking_port_v2` | `pcd_networking_port` |
| `openstack_networking_secgroup_v2` | `pcd_networking_secgroup` |
| `openstack_networking_secgroup_rule_v2` | `pcd_networking_secgroup_rule` |
| `openstack_networking_router_v2` | `pcd_networking_router` |
| `openstack_networking_router_interface_v2` | `pcd_networking_router_interface` |
| `openstack_networking_router_route_v2` | `pcd_networking_router_route` |
| `openstack_networking_subnet_route_v2` | `pcd_networking_subnet_route` |
| `openstack_networking_floatingip_v2` | `pcd_networking_floatingip` |
| `openstack_networking_floatingip_associate_v2` | `pcd_networking_floatingip_associate` |
| `openstack_networking_port_secgroup_associate_v2` | `pcd_networking_port_secgroup_associate` |
| `openstack_compute_instance_v2` | `pcd_compute_instance` |
| `openstack_compute_flavor_v2` | `pcd_compute_flavor` |
| `openstack_compute_keypair_v2` | `pcd_compute_keypair` |
| `openstack_compute_servergroup_v2` | `pcd_compute_servergroup` |
| `openstack_compute_interface_attach_v2` | `pcd_compute_interface_attach` |
| `openstack_compute_volume_attach_v2` | `pcd_compute_volume_attach` |
| `openstack_blockstorage_volume_v3` | `pcd_blockstorage_volume` |

Not every OpenStack resource has a PCD equivalent yet; unported resources are
tracked on the project roadmap.

## Beyond OpenStack: PCD-native resources

The PCD provider also ships resources that have **no `openstack_*` equivalent** — they
manage Platform9's own control plane (the `resmgr` service) rather than an OpenStack
service. There is nothing to migrate *from*; these are net-new capability:

- **`pcd_cluster_blueprint`** — the region's cluster blueprint (networking type, virtual-
  network segmentation, image-library and VM storage, Cinder backends, VM HA, auto-
  rebalancing). PCD keeps one per region, so `terraform import` it and manage in place.
- **`pcd_host_config`** — a host's traffic-type-to-interface mapping and network labels.
- **`pcd_host_role`** — assign a PCD role (e.g. `pf9-ostackhost-neutron`) to a host.
- **`pcd_host_config_assignment`** — attach a host configuration to a host.

## Behavioral notes

A few resources differ slightly from their upstream counterparts:

- **`pcd_networking_port`** — `fixed_ip` and `allowed_address_pairs` capture your
  requested configuration and are not refreshed from the server (Neutron fills in
  addresses and MACs that would otherwise churn the plan). Use the computed
  `all_fixed_ips` to reference the addresses Neutron actually assigned.
- **`pcd_images_image`** — `properties` tracks only the keys you set; Glance's many
  system/read-only properties are ignored to avoid a perpetual diff.
- **`pcd_networking_floatingip`** — allocate from an external network by its name
  via `pool`, exactly as upstream.
- **`pcd_lb_loadbalancer`** and the LB tree — PCD ships only the **OVN** Octavia provider,
  which is Layer 4. `loadbalancer_provider` defaults to `ovn`; use TCP/UDP/SCTP listener
  protocols and OVN-supported pool algorithms. There are no L7 policy/rule resources
  (`openstack_lb_l7policy_v2` / `_l7rule_v2` have no PCD equivalent) because OVN does not
  do L7.

## Moving existing infrastructure

Terraform state from the OpenStack provider cannot be reused directly, because the
resource type names differ. The cleanest path is to import the live resources into
the new configuration:

1. Write the `pcd_*` resources (or copy them from your `openstack_*` config and
   rename the types).
2. Import each existing object by ID:

```shell
terraform import pcd_networking_network.web 2f0e...c1
terraform import pcd_compute_instance.app 9b1a...ff
```

3. Run `terraform plan` and reconcile any drift until the plan is empty.

Composite-ID resources (routes and the attach/associate resources) use a
`parent/child` import ID; the exact format is on each resource's documentation
page under **Import**.
