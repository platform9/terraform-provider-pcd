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

# backends names a driver configuration (a second-level key) of the blueprint's
# storage_backends_json; assigning the role is what turns that definition into
# a running service on the host.
resource "pcd_host_cluster_role" "storage" {
  host_id              = var.host_id
  role                 = "persistent-storage"
  backends             = ["nfs-primary"]
  wait_until_converged = true

  depends_on = [pcd_host_config_assignment.host1]
}
