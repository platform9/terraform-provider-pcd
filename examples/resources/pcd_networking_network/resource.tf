resource "pcd_networking_network" "example" {
  name           = "tf-example-network"
  description    = "Managed by Terraform"
  admin_state_up = true
  shared         = false
  external       = false
  tags           = ["tf-example", "networking"]
}
