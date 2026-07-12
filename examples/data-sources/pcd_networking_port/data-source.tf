resource "pcd_networking_network" "example" {
  name = "tf-example-network"
}

data "pcd_networking_port" "example" {
  name       = "tf-example-port"
  network_id = pcd_networking_network.example.id
}
