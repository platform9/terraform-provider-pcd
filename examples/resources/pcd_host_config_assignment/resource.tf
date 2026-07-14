resource "pcd_host_config" "example" {
  name           = "hc-single-nic"
  mgmt_interface = "enp1s0"
  network_labels = { physnet1 = "enp1s0" }
}

resource "pcd_host_config_assignment" "example" {
  host_id        = "04575315-80ce-4617-9b96-6611d00c9942"
  host_config_id = pcd_host_config.example.id
}
