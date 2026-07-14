resource "pcd_host_config" "example" {
  name           = "hc-single-nic"
  mgmt_interface = "enp1s0"

  network_labels = {
    physnet1 = "enp1s0"
  }
}
