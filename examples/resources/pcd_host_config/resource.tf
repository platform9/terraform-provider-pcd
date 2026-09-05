# Every traffic type on one interface, with the physical-network label the
# provider network binds to. resmgr requires the cluster blueprint and every
# interface to be set when the configuration is created.
resource "pcd_host_config" "example" {
  name         = "hc-single-nic"
  cluster_name = pcd_cluster_blueprint.region.name

  mgmt_interface           = "enp1s0"
  vm_console_interface     = "enp1s0"
  host_liveness_interface  = "enp1s0"
  tunneling_interface      = "enp1s0"
  imagelib_interface       = "enp1s0"
  live_migration_interface = "enp1s0"

  network_labels = {
    physnet1 = "enp1s0"
  }
}
