resource "pcd_networking_network" "example" {
  name = "tf-example-network"
}

resource "pcd_networking_subnet" "example" {
  network_id = pcd_networking_network.example.id
  cidr       = "10.0.0.0/24"
}

resource "pcd_compute_instance" "golden" {
  name        = "tf-example-golden"
  image_name  = "Ubuntu-22.04"
  flavor_name = "m1.small"

  network {
    uuid = pcd_networking_network.example.id
  }

  depends_on = [pcd_networking_subnet.example]
}

# Capture the instance as an image. Renaming the snapshot is done in place;
# a new snapshot is taken only when instance_id changes.
resource "pcd_compute_instance_snapshot" "golden" {
  instance_id = pcd_compute_instance.golden.id
  name        = "golden-2026-10"
}

# Boot more instances from the captured image. When the snapshot is replaced
# (for example because instance_id changes), these instances are rebuilt in
# place from the new image, which erases their root disks; add
# lifecycle { ignore_changes = [image_id] } to keep them on the image they
# booted from.
resource "pcd_compute_instance" "web" {
  count       = 2
  name        = "tf-example-web-${count.index}"
  image_id    = pcd_compute_instance_snapshot.golden.id
  flavor_name = "m1.small"

  network {
    uuid = pcd_networking_network.example.id
  }

  depends_on = [pcd_networking_subnet.example]
}
