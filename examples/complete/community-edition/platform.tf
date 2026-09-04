# Day 2, part one: what a workload runs on. A flat provider network on the
# physnet1 label, a subnet with a DHCP pool, a security group, an image, and
# a flavor.

# The label only exists on the host once the hypervisor role has converged,
# and nothing in the network's attributes references that role, so the
# dependency is explicit.
resource "pcd_networking_network" "workload" {
  name     = "workload-net"
  shared   = true
  external = true

  segments = [{
    network_type     = "flat"
    physical_network = "physnet1"
  }]

  depends_on = [pcd_host_cluster_role.hypervisor]
}

resource "pcd_networking_subnet" "workload" {
  network_id  = pcd_networking_network.workload.id
  name        = "workload-subnet"
  cidr        = var.network_cidr
  ip_version  = 4
  gateway_ip  = var.network_gateway
  enable_dhcp = true

  allocation_pools = [{
    start = var.allocation_pool_start
    end   = var.allocation_pool_end
  }]

  dns_nameservers = var.dns_nameservers
}

resource "pcd_networking_secgroup" "workload" {
  name        = "workload-secgroup"
  description = "SSH and ICMP for the workload instance"
}

resource "pcd_networking_secgroup_rule" "ssh" {
  security_group_id = pcd_networking_secgroup.workload.id
  direction         = "ingress"
  ethertype         = "IPv4"
  protocol          = "tcp"
  port_range_min    = 22
  port_range_max    = 22
  remote_ip_prefix  = "0.0.0.0/0"
}

resource "pcd_networking_secgroup_rule" "icmp" {
  security_group_id = pcd_networking_secgroup.workload.id
  direction         = "ingress"
  ethertype         = "IPv4"
  protocol          = "icmp"
  remote_ip_prefix  = "0.0.0.0/0"
}

# CirrOS is a tiny image that proves the image library end to end; point
# image_source_url (or local_file_path) at your own image for real work. The
# image library host receives the upload, so the role must be converged.
resource "pcd_images_image" "cirros" {
  name             = "cirros"
  container_format = "bare"
  disk_format      = "qcow2"
  image_source_url = "https://download.cirros-cloud.net/0.6.2/cirros-0.6.2-x86_64-disk.img"
  min_disk_gb      = 1
  visibility       = "public"

  depends_on = [pcd_host_cluster_role.image_library]
}

resource "pcd_compute_flavor" "small" {
  name  = "small"
  vcpus = 1
  ram   = 512
  disk  = 5
}
