---
page_title: "Community Edition: from an empty host to a running VM - PCD Provider"
subcategory: ""
description: |-
  A complete, runnable configuration that turns a fresh PCD Community Edition and one prepared host into a working region with an instance and a persistent volume.
---

# Community Edition: from an empty host to a running VM

This guide walks through a configuration that builds a working PCD region from
nothing: a volume type and cluster blueprint with an NFS storage backend, host
networking, a cluster, the roles that turn a host into a hypervisor, image
library, and storage node, and then a network, an image, a flavor, and an
instance with a volume attached. One `terraform apply` does all of it.

The files are in the provider repository under
[`examples/complete/community-edition/`](https://github.com/platform9/terraform-provider-pcd/tree/main/examples/complete/community-edition)
and are rendered below exactly as they are there. The configuration was applied
and destroyed end to end on a Community Edition 2026.4 lab while this guide was
written.

## Prerequisites

- **A PCD Community Edition** you can log in to.
- **One host, prepared and authorized, with no roles.** Install `pcdctl` on the
  host and run `pcdctl prep-node` and `pcdctl authorize-node` (see
  [PCD CLI](https://docs.platform9.com/private-cloud-director/automation-and-cli/pcdctl-command-line)).
  The host must appear under **Infrastructure > Cluster Hosts** with no roles
  assigned. Terraform assigns them.
- **An NFS export the host can mount.** Any NFS server on the host's network
  works; the Community Edition beginner's guide on docs.platform9.com shows
  how to export a directory from an Ubuntu machine.
- **Network reachability.** The machine running Terraform must reach the PCD URL
  and the host's IP address: once the image-library role converges, image uploads
  go to the image-library host directly.
- **Terraform 1.5 or later** and the `pcdctl RC` file from **Settings > API
  Access** in the PCD UI. Source it, and because Community Edition ships a
  self-signed certificate, set `OS_INSECURE=true` as well:

  ```shell
  source pcdctlrc
  export OS_INSECURE=true
  ```

## Find the host UUID

Every host-scoped resource takes the host's resource-manager UUID. It is not the
ID that `pcdctl hypervisor list` prints, and before the host has roles there is no
hypervisor to list anyway. Read it on the host:

```shell
cat /etc/pf9/host_id.conf
# [hostagent]
# host_id = 136fc11a-ec5a-4699-b097-f75796134f8d
```

Or list every authorized host through the resource manager:

```shell
TOKEN=$(pcdctl token issue -f value -c id)
curl -sk -H "X-Auth-Token: $TOKEN" https://pcd.example.com/resmgr/v1/hosts | python3 -m json.tool
```

Put the UUID into `terraform.tfvars` as `host_id`, with the rest of the inputs:

```hcl
# Copy to terraform.tfvars and fill in your values.

# On the host: cat /etc/pf9/host_id.conf
host_id = "00000000-0000-0000-0000-000000000000"

# The host's NIC (ip -br link on the host).
host_interface = "enp1s0"

# An NFS export the host can mount.
nfs_export = "192.168.1.50:/srv/nfs/pcd"

# The network the host's NIC is on. Instances get addresses from the pool;
# keep it clear of addresses your router or other devices hand out.
network_cidr          = "192.168.1.0/24"
network_gateway       = "192.168.1.1"
allocation_pool_start = "192.168.1.200"
allocation_pool_end   = "192.168.1.220"
```

## The provider

Credentials come from the sourced RC file. The two domain arguments are set
explicitly so the configuration works whether or not the RC file exports them.

```terraform
terraform {
  required_providers {
    pcd = {
      source  = "platform9/pcd"
      version = "~> 0.1"
    }
  }
}

# Credentials come from the pcdctl RC file: in the PCD UI open Settings > API
# Access, copy it, set OS_PASSWORD, and `source pcdctlrc` before running
# Terraform. The provider reads OS_AUTH_URL, OS_USERNAME, OS_PASSWORD,
# OS_PROJECT_NAME, and OS_REGION_NAME from the environment.
provider "pcd" {
  user_domain_id    = "default"
  project_domain_id = "default"

  # Community Edition ships a self-signed certificate.
  insecure = true
}
```

## Day 1: the region

`region.tf` configures PCD's own control plane. The order is the order the
resources reference each other in, and it is the order the UI expects too: the
volume type before the blueprint that names it, the blueprint before the host
configuration that belongs to it, the host configuration assigned before the
roles that consume it.

```terraform
# Day 1: the region. Everything here configures PCD's own control plane, and
# the order matters: each resource names one created before it.

# 1. A volume type: what tenants ask for when they create a volume. Its
#    volume_backend_name must match a backend declared in the blueprint below,
#    and the blueprint's image_library_storage names this type, so it comes first.
resource "pcd_blockstorage_volume_type" "nfs" {
  name        = "nfs"
  description = "NFS-backed persistent storage"
  is_public   = true

  extra_specs = {
    volume_backend_name = "nfs"
  }
}

# 2. The region's single cluster blueprint. storage_backends_json declares the
#    Persistent Storage backends: the top-level key is the backend name, the
#    key under it names one driver configuration, and the keys inside config
#    are the ones the PCD UI offers under Add Volume Backend Configuration.
resource "pcd_cluster_blueprint" "region" {
  name            = var.blueprint_name
  dns_domain_name = var.dns_domain_name

  virtual_networking = {
    enabled       = true
    underlay_type = "vlan"
    vnid_range    = "1000:2000"
  }

  image_library_storage        = pcd_blockstorage_volume_type.nfs.name
  image_library_shared_storage = true
  instance_shared_storage      = false
  vm_storage                   = "/opt/data/instances"

  storage_backends_json = jsonencode({
    nfs = {
      nfs = {
        driver = "NFS"
        config = {
          nfs_shares_config           = "/opt/pf9/etc/pf9-cindervolume-base/conf.d/nfs_shares"
          nfs_mount_points            = var.nfs_export
          nfs_mount_point_base        = "/opt/pf9/etc/pf9-cindervolume-base/volumes/"
          nfs_snapshot_support        = "true"
          nas_secure_file_permissions = "false"
          nas_secure_file_operations  = "false"
        }
      }
    }
  })
}

# 3. A host configuration maps each traffic type to an interface. The
#    network_labels entry gives the interface a physical-network label
#    (physnet1) that the provider network in platform.tf binds to.
resource "pcd_host_config" "single_nic" {
  name         = "hc-single-nic"
  cluster_name = pcd_cluster_blueprint.region.name

  mgmt_interface           = var.host_interface
  vm_console_interface     = var.host_interface
  host_liveness_interface  = var.host_interface
  tunneling_interface      = var.host_interface
  imagelib_interface       = var.host_interface
  live_migration_interface = var.host_interface

  network_labels = {
    physnet1 = var.host_interface
  }
}

resource "pcd_host_config_assignment" "host1" {
  host_id        = var.host_id
  host_config_id = pcd_host_config.single_nic.id
}

# 4. The cluster the hypervisor joins. VM high availability and rebalancing
#    are cluster settings, so they are part of the region from the start.
resource "pcd_cluster" "main" {
  name = var.cluster_name

  vm_high_availability = {
    enabled = true
  }

  auto_resource_rebalancing = {
    enabled                    = true
    rebalancing_strategy       = "vm_workload_consolidation"
    rebalancing_frequency_mins = 10
  }
}

# 5. Cluster roles onboard the host. PCD expands each into its granular roles
#    and computes their settings from the blueprint and host configuration.
#    wait_until_converged blocks until the host reports the role healthy, so
#    the resources in platform.tf and app.tf find a working hypervisor, image
#    library, and storage backend. Nothing here references the assignment by
#    attribute, so the dependency is explicit.
resource "pcd_host_cluster_role" "hypervisor" {
  host_id              = var.host_id
  role                 = "hypervisor"
  host_cluster         = pcd_cluster.main.name
  wait_until_converged = true

  depends_on = [pcd_host_config_assignment.host1]
}

resource "pcd_host_cluster_role" "image_library" {
  host_id              = var.host_id
  role                 = "image-library"
  wait_until_converged = true

  depends_on = [pcd_host_config_assignment.host1]
}

# backends names a top-level key of the blueprint's storage_backends_json;
# assigning the role is what turns that definition into a running service.
resource "pcd_host_cluster_role" "storage" {
  host_id              = var.host_id
  role                 = "persistent-storage"
  backends             = ["nfs"]
  wait_until_converged = true

  depends_on = [pcd_host_config_assignment.host1]
}
```

Three things in this file are worth understanding before you change it.

**The volume type and the backend are one thing seen from two sides.** The
blueprint's `storage_backends_json` declares a backend named `nfs` (its top-level
key) with one driver configuration under it. The `persistent-storage` role's
`backends = ["nfs"]` turns that declaration into a running service on the host.
The volume type's `volume_backend_name = "nfs"` is how a volume created with
`volume_type = "nfs"` is routed to it, and `image_library_storage` names the same
type so the image library stores images there. Rename one and rename them all.

**The driver keys are the UI's keys.** `driver = "NFS"` is one of PCD's built-in
driver identifiers, and the keys inside `config` are exactly the ones the UI
offers under **Cluster Blueprint > Persistent Storage Connectivity > Add Volume
Backend Configuration** for that driver. For another driver, use its identifier
and keys; docs.platform9.com's storage backend configuration examples list them.
A custom driver takes its full class path as `driver`. This attribute is
sensitive because backend credentials live in it, and it is stored in Terraform
state as plain text: use a remote backend with encryption at rest.

**Explicit dependencies.** Nothing in a `pcd_host_cluster_role` references the
host configuration assignment by attribute, so each role lists it in
`depends_on`; assigning a role before the host has its network configuration
produces an inconsistent host. `wait_until_converged = true` makes each role's
apply block until the host reports the role healthy, which takes a few minutes
per role.

## Day 2: the platform

`platform.tf` adds what a workload runs on. The provider network is a flat
network on the `physnet1` label from the host configuration, so instances get
addresses on the same network as the host.

```terraform
# Day 2, part one: what a workload runs on. A flat provider network on the
# physnet1 label, a subnet with a DHCP pool, a security group, an image, and
# a flavor.

# The label only exists on the host once the hypervisor role has converged,
# and nothing in the network's attributes references that role, so the
# dependency is explicit.
resource "pcd_networking_network" "workload" {
  name     = "workload-net"
  shared   = true
  external = true

  segments = [{
    network_type     = "flat"
    physical_network = "physnet1"
  }]

  depends_on = [pcd_host_cluster_role.hypervisor]
}

resource "pcd_networking_subnet" "workload" {
  network_id  = pcd_networking_network.workload.id
  name        = "workload-subnet"
  cidr        = var.network_cidr
  ip_version  = 4
  gateway_ip  = var.network_gateway
  enable_dhcp = true

  allocation_pools = [{
    start = var.allocation_pool_start
    end   = var.allocation_pool_end
  }]

  dns_nameservers = var.dns_nameservers
}

resource "pcd_networking_secgroup" "workload" {
  name        = "workload-secgroup"
  description = "SSH and ICMP for the workload instance"
}

resource "pcd_networking_secgroup_rule" "ssh" {
  security_group_id = pcd_networking_secgroup.workload.id
  direction         = "ingress"
  ethertype         = "IPv4"
  protocol          = "tcp"
  port_range_min    = 22
  port_range_max    = 22
  remote_ip_prefix  = "0.0.0.0/0"
}

resource "pcd_networking_secgroup_rule" "icmp" {
  security_group_id = pcd_networking_secgroup.workload.id
  direction         = "ingress"
  ethertype         = "IPv4"
  protocol          = "icmp"
  remote_ip_prefix  = "0.0.0.0/0"
}

# CirrOS is a tiny image that proves the image library end to end; point
# image_source_url (or local_file_path) at your own image for real work. The
# image library host receives the upload, so the role must be converged.
resource "pcd_images_image" "cirros" {
  name             = "cirros"
  container_format = "bare"
  disk_format      = "qcow2"
  image_source_url = "https://download.cirros-cloud.net/0.6.2/cirros-0.6.2-x86_64-disk.img"
  min_disk_gb      = 1
  visibility       = "public"

  depends_on = [pcd_host_cluster_role.image_library]
}

resource "pcd_compute_flavor" "small" {
  name  = "small"
  vcpus = 1
  ram   = 512
  disk  = 5
}
```

The network depends on the hypervisor role because the `physnet1` label only
exists on the host once that role has converged, and the image depends on the
image-library role because the upload goes to the image-library host. Neither
dependency is visible in the resources' own attributes, so both are explicit.

## Day 2: the workload

`app.tf` boots the instance and attaches a volume to it.

```terraform
# Day 2, part two: an instance with a persistent volume attached.

# The instance references the network by ID, not the subnet, and PCD needs a
# subnet to give it an address, so the subnet dependency is explicit.
resource "pcd_compute_instance" "workload" {
  name        = "workload-vm"
  image_name  = pcd_images_image.cirros.name
  flavor_name = pcd_compute_flavor.small.name

  security_groups = [pcd_networking_secgroup.workload.name]

  network {
    uuid = pcd_networking_network.workload.id
  }

  depends_on = [pcd_networking_subnet.workload]
}

# The volume lands on the nfs backend through its volume type. The backend
# only accepts volumes once the persistent-storage role has converged.
resource "pcd_blockstorage_volume" "data" {
  name        = "workload-data"
  size        = 1
  volume_type = pcd_blockstorage_volume_type.nfs.name

  depends_on = [pcd_host_cluster_role.storage]
}

resource "pcd_compute_volume_attach" "data" {
  instance_id = pcd_compute_instance.workload.id
  volume_id   = pcd_blockstorage_volume.data.id
}
```

```terraform
output "instance_ip" {
  description = "The address the workload instance received on the provider network."
  value       = pcd_compute_instance.workload.access_ip_v4
}

output "instance_id" {
  value = pcd_compute_instance.workload.id
}

output "volume_id" {
  value = pcd_blockstorage_volume.data.id
}

output "host_config_id" {
  description = "The host configuration ID, which pcd_host_config imports by."
  value       = pcd_host_config.single_nic.id
}
```

## Apply and verify

```shell
terraform init
terraform apply
```

The apply takes about twenty minutes on a small host, most of it spent in the
three `wait_until_converged` roles. When it finishes, the outputs show the
instance's address, and `pcdctl` shows the region:

```shell
pcdctl volume service list          # cinder-volume <host-uuid>@nfs enabled/up
pcdctl hypervisor list              # the host, state up
pcdctl server list                  # workload-vm ACTIVE
pcdctl volume list                  # workload-data in-use, attached to workload-vm
ping "$(terraform output -raw instance_ip)"
terraform plan                      # No changes.
```

## Destroy

`terraform destroy` takes the region down in reverse order: the instance and
volume, the network, the roles, the host configuration and cluster, and finally
the blueprint and volume type. Two behaviors of PCD show up here:

- Removing a role starts a deauthorization on the host that PCD acknowledges
  before it completes. Until it lands, PCD refuses the host-configuration
  unassignment with `403 HostInAuthState: Host ... is authorized with roles
  assigned`, so the first destroy can stop there with the blueprint, cluster,
  and host configuration still standing. Wait a few minutes and run
  `terraform destroy` again; it finishes.
- The blueprint is deleted from PCD on destroy (since provider v0.1.10). The host
  stays authorized, with no roles, and can be onboarded again with the same
  configuration.

## Already have a region?

If the region already has a blueprint, do not create a second one: PCD keeps one
per region. Import it, and any volume types, networks, or images you already
have, following the [Importing guide](importing.md). An `import` block plus
`terraform plan -generate-config-out=generated.tf` writes the blueprint's block
for you, with `storage_backends_json` left unset so the backends PCD has are
kept.
