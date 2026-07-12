resource "pcd_networking_network" "example" {
  name = "tf-example-network"
}

resource "pcd_networking_subnet" "example" {
  network_id = pcd_networking_network.example.id
  name       = "tf-example-subnet"
  cidr       = "10.0.0.0/24"
}

resource "pcd_networking_port" "example" {
  network_id = pcd_networking_network.example.id
  name       = "tf-example-port"

  fixed_ip {
    subnet_id = pcd_networking_subnet.example.id
  }
}

resource "pcd_networking_floatingip" "example" {
  pool = "external-net"
}

resource "pcd_networking_floatingip_associate" "example" {
  floating_ip_id = pcd_networking_floatingip.example.id
  port_id        = pcd_networking_port.example.id
}
