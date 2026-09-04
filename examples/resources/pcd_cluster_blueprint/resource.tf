# PCD supports a single blueprint per region. The usual workflow is to import
# the existing blueprint (see import.sh) and then manage it here. To build a
# region from nothing, see the Community Edition guide.
resource "pcd_cluster_blueprint" "example" {
  name            = "cluster-1"
  dns_domain_name = "example.local."

  virtual_networking = {
    enabled       = true
    underlay_type = "vlan"
    vnid_range    = "1000:2000"
  }

  # The image library stores images on a volume type; create it first with
  # pcd_blockstorage_volume_type and reference it by name.
  image_library_storage        = "nfs"
  image_library_shared_storage = true

  # Where instance (ephemeral) disks live on each hypervisor. Set
  # instance_shared_storage = true only if this path is mounted as shared
  # storage (e.g. NFS) across all hosts.
  vm_storage              = "/opt/data/instances"
  instance_shared_storage = false

  # Optional: a floating IP through which VM VNC consoles are reached.
  # vnc_floating_ip = "203.0.113.10"

  # storage_backends_json declares the Cinder storage backends. It is omitted
  # here so an imported blueprint keeps the backends PCD already has; it is
  # required only when creating a brand-new blueprint, and it carries driver
  # credentials (sensitive). The shape, with the NFS driver as the example:
  #
  # storage_backends_json = jsonencode({
  #   nfs = {            # backend name: what pcd_host_cluster_role.backends and
  #     nfs = {          #   a volume type's volume_backend_name refer to
  #       driver = "NFS" # a built-in driver identifier, or a full driver class path
  #       config = {
  #         nfs_shares_config           = "/opt/pf9/etc/pf9-cindervolume-base/conf.d/nfs_shares"
  #         nfs_mount_points            = "192.0.2.10:/srv/nfs/pcd"
  #         nfs_mount_point_base        = "/opt/pf9/etc/pf9-cindervolume-base/volumes/"
  #         nfs_snapshot_support        = "true"
  #         nas_secure_file_permissions = "false"
  #         nas_secure_file_operations  = "false"
  #       }
  #     }
  #   }
  # })

  # networking_type and enable_distributed_routing are set by PCD (ovn / true)
  # and are read-only here; they appear in state but are not configurable.
}
