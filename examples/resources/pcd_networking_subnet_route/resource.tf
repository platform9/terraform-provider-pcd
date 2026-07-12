resource "pcd_networking_network" "example" {
  name = "tf-example-network"
}

resource "pcd_networking_subnet" "example" {
  network_id = pcd_networking_network.example.id
  name       = "tf-example-subnet"
  cidr       = "10.0.0.0/24"
}

resource "pcd_networking_subnet_route" "example" {
  subnet_id        = pcd_networking_subnet.example.id
  destination_cidr = "192.168.100.0/24"
  next_hop         = "10.0.0.1"
}
