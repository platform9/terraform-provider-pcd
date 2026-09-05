resource "pcd_host_config" "example" {
  name         = "hc-single-nic"
  cluster_name = pcd_cluster_blueprint.region.name

  mgmt_interface           = "enp1s0"
  vm_console_interface     = "enp1s0"
  host_liveness_interface  = "enp1s0"
  tunneling_interface      = "enp1s0"
  imagelib_interface       = "enp1s0"
  live_migration_interface = "enp1s0"

  network_labels = { physnet1 = "enp1s0" }
}

# The host is named, not pasted in by UUID: pcd_host resolves the
# resource-manager UUID from the hostname the host reports.
data "pcd_host" "hyp1" {
  name = "hyp1.example.com"
}

resource "pcd_host_config_assignment" "example" {
  host_id        = data.pcd_host.hyp1.id
  host_config_id = pcd_host_config.example.id
}
