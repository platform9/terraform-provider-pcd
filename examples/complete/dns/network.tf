# A tenant network bound to the zone, and a subnet that publishes its fixed
# IPs. Both are set before the instance exists: Neutron creates records when a
# port is created, never for ports that already exist.
resource "pcd_networking_network" "app" {
  name       = "dns-demo-net"
  dns_domain = pcd_dns_zone.app.name
}

resource "pcd_networking_subnet" "app" {
  network_id           = pcd_networking_network.app.id
  name                 = "dns-demo-subnet"
  cidr                 = var.network_cidr
  ip_version           = 4
  dns_publish_fixed_ip = true
}
