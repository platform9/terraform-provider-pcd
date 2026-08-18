# A tenant (self-service) network.
resource "pcd_networking_network" "example" {
  name           = "tf-example-network"
  description    = "Managed by Terraform"
  admin_state_up = true
  shared         = false
  external       = false
  tags           = ["tf-example", "networking"]
}

# Provider network (admin): a flat physical network on a host-config label.
resource "pcd_networking_network" "provider" {
  name   = "lab-provider-net"
  shared = true

  segments = [{
    network_type     = "flat"
    physical_network = "physnet1"
  }]
}

# Layer 2 / "Simple" network — the PCD UI's Simple Networking option, akin to
# a VMware port group: no subnet, no DHCP, no IP management. VMs on it manage
# their own addressing. Three things define it, all set here:
#   1. no pcd_networking_subnet is created for it
#   2. port_security_enabled = false (security groups do not apply)
#   3. the "simple_network" tag — what the PCD UI keys on to list it under
#      "Layer 2 Networks" in the Deploy VM wizard
resource "pcd_networking_network" "l2" {
  name                  = "vlan-100-l2"
  shared                = true
  port_security_enabled = false
  tags                  = ["simple_network"]

  segments = [{
    network_type     = "vlan"
    physical_network = "physnet1"
    segmentation_id  = 100
  }]
}
