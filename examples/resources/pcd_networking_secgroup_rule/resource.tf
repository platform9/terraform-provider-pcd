resource "pcd_networking_secgroup" "example" {
  name = "tf-example-secgroup"
}

resource "pcd_networking_secgroup_rule" "example" {
  direction         = "ingress"
  ethertype         = "IPv4"
  security_group_id = pcd_networking_secgroup.example.id
  protocol          = "tcp"
  port_range_min    = 22
  port_range_max    = 22
  remote_ip_prefix  = "10.0.0.0/24"
  description       = "Allow SSH from the internal subnet"
}
