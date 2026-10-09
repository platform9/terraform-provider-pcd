resource "pcd_networking_network" "example" {
  name = "tf-example-network"
}

resource "pcd_networking_subnet" "example" {
  network_id = pcd_networking_network.example.id
  cidr       = "10.0.0.0/24"
}

resource "pcd_compute_instance" "web" {
  name        = "tf-example-web"
  image_name  = "Ubuntu-22.04"
  flavor_name = "m1.small"
  key_pair    = "tf-example-key"

  network {
    uuid = pcd_networking_network.example.id
  }

  depends_on = [pcd_networking_subnet.example]
}

# Boot the instance into rescue mode to repair its disk, which the rescue
# system sees as a second disk. Removing this resource returns the instance
# to normal operation; Terraform unrescues it before changing the instance in
# the same apply.
resource "pcd_compute_instance_rescue" "web" {
  instance_id = pcd_compute_instance.web.id
}
