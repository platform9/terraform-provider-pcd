resource "pcd_compute_volume_attach" "example" {
  instance_id = pcd_compute_instance.example.id
  volume_id   = pcd_blockstorage_volume.example.id
  device      = "/dev/vdb"
}

resource "pcd_compute_instance" "example" {
  name        = "tf-example-instance"
  image_name  = "ubuntu-22.04"
  flavor_name = "m1.small"

  network {
    uuid = pcd_networking_network.example.id
  }
}

resource "pcd_blockstorage_volume" "example" {
  name = "tf-example-volume"
  size = 10
}

resource "pcd_networking_network" "example" {
  name = "tf-example-network"
}
