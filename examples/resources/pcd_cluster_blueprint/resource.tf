# PCD supports a single blueprint per region. The usual workflow is to import
# the existing blueprint (see import.sh) and then manage it here.
resource "pcd_cluster_blueprint" "example" {
  name            = "cluster-1"
  dns_domain_name = "example.local."

  virtual_networking = {
    enabled       = true
    underlay_type = "vlan"
    vnid_range    = "1000:2000"
  }

  # Where instance (ephemeral) disks live on each hypervisor. Set
  # instance_shared_storage = true only if this path is mounted as shared
  # storage (e.g. NFS) across all hosts.
  vm_storage              = "/opt/data/instances"
  instance_shared_storage = false

  # Optional: a floating IP through which VM VNC consoles are reached.
  # vnc_floating_ip = "203.0.113.10"

  # storage_backends_json is omitted here so the imported Cinder backends are
  # preserved. It is required only when creating a brand-new blueprint, and it
  # carries driver credentials (sensitive).

  # networking_type and enable_distributed_routing are set by PCD (ovn / true)
  # and are read-only here; they appear in state but are not configurable.
}
