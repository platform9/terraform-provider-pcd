resource "pcd_networking_router" "example" {
  name = "tf-example-vpn-router"
}

resource "pcd_vpnaas_service" "example" {
  name      = "tf-example-vpn"
  router_id = pcd_networking_router.example.id
}
