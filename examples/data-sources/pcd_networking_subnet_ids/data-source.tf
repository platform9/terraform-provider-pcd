resource "pcd_networking_network" "example" {
  name = "tf-example-network"
}

# Look up the IDs of all subnets attached to the network.
data "pcd_networking_subnet_ids" "example" {
  network_id = pcd_networking_network.example.id
  ip_version = 4
}
