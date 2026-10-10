# Look up an instance Terraform does not manage, then attach a floating IP to
# its port on the network named "app-net". Select a port by network rather than
# by index: ports created in the same second are ordered by port ID, not by the
# order the instance was booted with.
data "pcd_compute_instance" "web" {
  name = "web-01"
}

resource "pcd_networking_floatingip" "web" {
  pool = "public"
}

resource "pcd_networking_floatingip_associate" "web" {
  floating_ip_id = pcd_networking_floatingip.web.id
  port_id        = one([for n in data.pcd_compute_instance.web.network : n.port if n.name == "app-net"])
}
