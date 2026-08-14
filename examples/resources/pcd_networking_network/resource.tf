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
