resource "pcd_networking_network" "example" {
  name = "tf-example-network"
}

resource "pcd_compute_instance" "example" {
  name        = "tf-example-instance"
  image_name  = "Ubuntu-22.04"
  flavor_name = "m1.small"
  key_pair    = "tf-example-key"

  security_groups = ["default"]

  metadata = {
    environment = "dev"
  }

  network {
    uuid = pcd_networking_network.example.id
  }
}
