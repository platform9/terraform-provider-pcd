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
  and the host's IP address on port 9494. Once the image-library role converges,
  the Image Library Service on that host is published at `https://<host-ip>:9494`
  and the provider uploads images there, as the UI does; an image uploaded through
  the control plane's public endpoint lands where no hypervisor can boot it. If
  the host is not routable from where Terraform runs (a jump host, a tunnel), set
  `endpoint_overrides = { image = "https://<reachable-address>:9494/v2/" }` in
  the provider block.
- **Terraform 1.5 or later** and the `pcdctl RC` file from **Settings > API
  Access** in the PCD UI. Source it, and because Community Edition ships a
  self-signed certificate, set `OS_INSECURE=true` as well:

  ```shell
  source pcdctlrc
  export OS_INSECURE=true
  ```

## Name the host

Every host-scoped resource takes the host's resource-manager UUID, but the example
never asks for it: the `pcd_host` data source resolves it from the hostname the
host reports. That is the name the Hosts page in the PCD UI shows, and what the
host itself prints (usually its fully qualified name):

```shell
hostname -f
# hyp1.example.com
```

The host is known to the resource manager once `pcdctl authorize-node` has run and
the host agent has reported in. The UUID itself is only needed for
`terraform import`; the
[Importing guide](https://registry.terraform.io/providers/platform9/pcd/latest/docs/guides/importing)
says where to find it.

Put the hostname into `terraform.tfvars` as `host_name`, with the rest of the inputs:

```hcl
# Copy to terraform.tfvars and fill in your values.

# The hostname the host reports to PCD: hostname -f on the host, or the Hosts page.
host_name = "hyp1.example.com"

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
#    volume_backend_name must equal the top-level key of a backend declared in
#    the blueprint below, and the blueprint's image_library_storage names this
#    type, so it comes first.
resource "pcd_blockstorage_volume_type" "nfs" {
  name        = "nfs"
  description = "NFS-backed persistent storage"
  is_public   = true

  extra_specs = {
    volume_backend_name = "nfs"
  }
}

# 2. The region's single cluster blueprint. storage_backends_json declares the
#    Persistent Storage backends. The top-level key ("nfs") is the backend name
#    and becomes volume_backend_name on the host; the key under it
#    ("nfs-primary") names one driver configuration, which the persistent-storage
#    role's backends list selects; the keys inside config are the ones the PCD
#    UI offers under Add Volume Backend Configuration for that driver. Write
#    boolean options as booleans (true/false), never as quoted strings: the
#    host reads them back as booleans, and a quoted "true" never matches, so
#    the host would never finish converging.
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
      "nfs-primary" = {
        driver = "NFS"
        config = {
          nfs_shares_config           = "/opt/pf9/etc/pf9-cindervolume-base/conf.d/nfs_shares"
          nfs_mount_points            = var.nfs_export
          nfs_mount_point_base        = "/opt/pf9/etc/pf9-cindervolume-base/volumes/"
          nfs_snapshot_support        = true
          nas_secure_file_permissions = false
          nas_secure_file_operations  = false
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

# The host is named, not identified by UUID: pcd_host resolves the
# resource-manager UUID from the hostname the host agent reports, once the host
# has been authorized (pcdctl authorize-node) and has reported in.
data "pcd_host" "hyp1" {
  name = var.host_name
}

resource "pcd_host_config_assignment" "host1" {
  host_id        = data.pcd_host.hyp1.id
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
  host_id              = data.pcd_host.hyp1.id
  role                 = "hypervisor"
  host_cluster         = pcd_cluster.main.name
  wait_until_converged = true

  depends_on = [pcd_host_config_assignment.host1]
}

resource "pcd_host_cluster_role" "image_library" {
  host_id              = data.pcd_host.hyp1.id
  role                 = "image-library"
  wait_until_converged = true

  depends_on = [pcd_host_config_assignment.host1]
}

# backends names a driver configuration (a second-level key) of the blueprint's
# storage_backends_json; assigning the role is what turns that definition into
# a running service on the host.
resource "pcd_host_cluster_role" "storage" {
  host_id              = data.pcd_host.hyp1.id
  role                 = "persistent-storage"
  backends             = ["nfs-primary"]
  wait_until_converged = true

  depends_on = [pcd_host_config_assignment.host1]
}
```

Three things in this file are worth understanding before you change it.

**Three names, and what each becomes.** `storage_backends_json` has two levels
of keys. The top-level key (`nfs`) is the backend name: on the host it becomes
`volume_backend_name` in the storage service's configuration, which is exactly
what the volume type's `extra_specs.volume_backend_name` must equal for volumes
of that type to land there. The key under it (`nfs-primary`) names one driver
configuration for that backend: it is what the `persistent-storage` role's
`backends` list selects, and it becomes the backend section on the host, so the
storage service reports itself as `<host-uuid>@nfs-primary`. `image_library_storage`
names the volume type, so the image library stores images on the same backend.
The names are yours to choose; keep the three references consistent.

**The driver keys are the UI's keys.** `driver = "NFS"` is one of PCD's built-in
driver identifiers, and the keys inside `config` are exactly the ones the UI
offers under **Cluster Blueprint > Persistent Storage Connectivity > Add Volume
Backend Configuration** for that driver. For another driver, use its identifier
and keys; docs.platform9.com's storage backend configuration examples list them.
A custom driver takes its full class path as `driver`. Write boolean options as
booleans (`nfs_snapshot_support = true`), never as the quoted strings the UI's
form shows: the host reads them back as booleans, a quoted `"true"` never
matches what it wrote, and the host keeps converging forever with no error
anywhere. This attribute is sensitive because backend credentials live in it,
and it is stored in Terraform state as plain text: use a remote backend with
encryption at rest.

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
pcdctl volume service list          # cinder-volume <host-uuid>@nfs-primary enabled/up
pcdctl hypervisor list              # the host, state up
pcdctl server list                  # workload-vm ACTIVE
pcdctl volume list                  # workload-data in-use, attached to workload-vm
ping "$(terraform output -raw instance_ip)"
terraform plan                      # No changes.
```

## Destroy

`terraform destroy` takes the region down in reverse order: the volume
attachment, instance, and volume; the image; the network and security group;
the roles; the cluster and host configuration; and finally the blueprint and
volume type. PCD acknowledges a role removal before the deauthorization it
starts on the host has finished, and until it has, it refuses anything that
still depends on that role. On the lab a full teardown took three runs of
`terraform destroy`, a few minutes apart:

1. The first run removed the workload, the image, the network, and the
   `hypervisor` and `image-library` roles, then stopped twice: the
   `persistent-storage` role was refused with `422 ValidationError: Cannot
   remove persistent-storage role: Volume ... (image-<id>) is present on host`,
   and the cluster with `500 HostClusterDeleteFailed: hostlist is not empty`,
   because the hypervisor deauthorization had not landed yet.

   The volume the first error names is the image library's backing volume for
   the image just deleted. PCD does not remove it with the image, and the
   storage role cannot go while any volume is on the backend. List it and
   delete it:

   ```shell
   pcdctl volume list --all-projects      # image-<id>, in the service project
   pcdctl volume delete <volume-id>
   ```

2. The second run, once the host's role list in the resource manager
   (`GET /resmgr/v2/hosts/<host-uuid>`, see the Importing guide) no longer
   showed the two removed roles, deleted the cluster and the storage role, then
   stopped at the host-configuration unassignment with `403 HostInAuthState:
   Host ... is authorized with roles assigned, please remove the roles first`,
   because the storage deauthorization was still landing.

3. The third run, a couple of minutes after the role list emptied, removed the
   host-configuration assignment, the host configuration, the blueprint
   (deleted from PCD since provider v0.1.10), and the volume type.

The host stays authorized, with no roles, and can be onboarded again with the
same configuration.

## Already have a region?

If the region already has a blueprint, do not create a second one: PCD keeps one
per region. Import it, and any volume types, networks, or images you already
have, following the [Importing guide](importing.md). An `import` block plus
`terraform plan -generate-config-out=generated.tf` writes the blueprint's block
for you, with `storage_backends_json` left unset so the backends PCD has are
kept.
