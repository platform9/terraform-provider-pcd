# Look up an instance Terraform does not manage, then attach a floating IP to
# its first port.
data "pcd_compute_instance" "web" {
  name = "web-01"
}

resource "pcd_networking_floatingip" "web" {
  pool = "public"
}

resource "pcd_networking_floatingip_associate" "web" {
  floating_ip_id = pcd_networking_floatingip.web.id
  port_id        = data.pcd_compute_instance.web.network[0].port
}
