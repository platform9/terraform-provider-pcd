resource "pcd_networking_network" "example" {
  name = "tf-example-net"
}

resource "pcd_compute_instance" "example" {
  name        = "tf-example-instance"
  image_name  = "ubuntu-22.04"
  flavor_name = "m1.small"

  network {
    uuid = pcd_networking_network.example.id
  }
}

resource "pcd_compute_interface_attach" "example" {
  instance_id = pcd_compute_instance.example.id
  network_id  = pcd_networking_network.example.id
  fixed_ip    = "10.0.0.42"
}
