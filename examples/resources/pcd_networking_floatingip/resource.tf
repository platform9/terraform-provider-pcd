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

  depends_on = [pcd_networking_subnet.example]
}

resource "pcd_networking_floatingip" "example" {
  pool        = "external-net"
  description = "Floating IP managed by Terraform"
  port_id     = pcd_networking_port.example.id
  tags        = ["env:dev", "managed-by:terraform"]
}
