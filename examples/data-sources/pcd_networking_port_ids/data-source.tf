resource "pcd_networking_network" "example" {
  name = "tf-example-network"
}

data "pcd_networking_port_ids" "example" {
  network_id     = pcd_networking_network.example.id
  device_owner   = "network:router_interface"
  admin_state_up = true
}
