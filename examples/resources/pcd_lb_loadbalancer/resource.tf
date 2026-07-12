resource "pcd_networking_network" "example" {
  name = "tf-example-lb-net"
}

resource "pcd_networking_subnet" "example" {
  network_id = pcd_networking_network.example.id
  cidr       = "10.0.0.0/24"
}

resource "pcd_lb_loadbalancer" "example" {
  name          = "tf-example-lb"
  vip_subnet_id = pcd_networking_subnet.example.id
}
