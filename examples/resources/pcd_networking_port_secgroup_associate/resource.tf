resource "pcd_networking_network" "example" {
  name = "tf-example-network"
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
