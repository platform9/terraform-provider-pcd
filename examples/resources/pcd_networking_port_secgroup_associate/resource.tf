resource "pcd_networking_network" "example" {
  name = "tf-example-network"
}

resource "pcd_networking_subnet" "example" {
  network_id = pcd_networking_network.example.id
  cidr       = "10.0.0.0/24"
}

resource "pcd_networking_port" "example" {
  name       = "tf-example-port"
  network_id = pcd_networking_network.example.id
}

resource "pcd_networking_secgroup" "web" {
  name = "tf-example-web"
}

resource "pcd_networking_secgroup" "db" {
  name = "tf-example-db"
}

resource "pcd_networking_port_secgroup_associate" "example" {
  port_id = pcd_networking_port.example.id
  enforce = true
  security_group_ids = [
    pcd_networking_secgroup.web.id,
    pcd_networking_secgroup.db.id,
  ]
}

# Per-NIC security groups on an instance, as the PCD UI's Edit Security Groups
# sets them: find the port Nova created for the instance on a network, then
# manage that port's groups. Leave the instance's security_groups unset, or
# the two would rewrite each other's groups on every apply.
#
# The port lookup must match exactly one port. With two NICs on the same
# network, use the pcd_networking_port_ids data source or a known port ID.
# With enforce = true, destroying this resource removes every group from the
# port, which drops all traffic on a port that has port security enabled.
resource "pcd_compute_instance" "app" {
  name        = "tf-example-app"
  image_name  = "Ubuntu-22.04"
  flavor_name = "m1.small"

  network {
    uuid = pcd_networking_network.example.id
  }

  depends_on = [pcd_networking_subnet.example]
}

data "pcd_networking_port" "app" {
  device_id  = pcd_compute_instance.app.id
  network_id = pcd_networking_network.example.id
}

resource "pcd_networking_port_secgroup_associate" "app" {
  port_id            = data.pcd_networking_port.app.id
  enforce            = true
  security_group_ids = [pcd_networking_secgroup.web.id]
}
