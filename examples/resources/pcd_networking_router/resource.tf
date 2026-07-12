resource "pcd_networking_network" "external" {
  name     = "tf-example-external-net"
  external = true
}

resource "pcd_networking_router" "example" {
  name                = "tf-example-router"
  description         = "Router managed by Terraform"
  admin_state_up      = true
  external_network_id = pcd_networking_network.external.id
  enable_snat         = true
  tags                = ["terraform", "example"]
}
