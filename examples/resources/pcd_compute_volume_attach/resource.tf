resource "pcd_compute_volume_attach" "example" {
  instance_id = pcd_compute_instance.example.id
  volume_id   = pcd_blockstorage_volume.example.id
  device      = "/dev/vdb"

  # Keep the volume if the instance is deleted outside Terraform. Setting the
  # flag makes Terraform own it: a change made elsewhere, for example in the
  # PCD UI, is reverted on the next apply. Leave it out to leave it alone.
  delete_on_termination = false
}

resource "pcd_compute_instance" "example" {
  name        = "tf-example-instance"
  image_name  = "ubuntu-22.04"
  flavor_name = "m1.small"

  network {
    uuid = pcd_networking_network.example.id
  }

  depends_on = [pcd_networking_subnet.example]
}

resource "pcd_blockstorage_volume" "example" {
  name = "tf-example-volume"
  size = 10
}

resource "pcd_networking_network" "example" {
  name = "tf-example-network"
}

resource "pcd_networking_subnet" "example" {
  network_id = pcd_networking_network.example.id
  cidr       = "10.0.0.0/24"
}
