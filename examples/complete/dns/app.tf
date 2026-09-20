data "pcd_images_image" "app" {
  name = var.image_name
}

data "pcd_compute_flavor" "app" {
  name = var.flavor_name
}

# The instance name becomes the port's dns_name, so the record is
# <instance_name>.<zone>. The subnet dependency is explicit: the port must be
# created after the subnet has dns_publish_fixed_ip set.
resource "pcd_compute_instance" "app" {
  name      = var.instance_name
  image_id  = data.pcd_images_image.app.id
  flavor_id = data.pcd_compute_flavor.app.id

  network {
    uuid = pcd_networking_network.app.id
  }

  depends_on = [pcd_networking_subnet.app]
}
