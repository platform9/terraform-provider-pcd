resource "pcd_networking_router" "example" {
  name = "tf-example-router"
}

resource "pcd_networking_router_route" "example" {
  router_id        = pcd_networking_router.example.id
  destination_cidr = "10.20.0.0/24"
  next_hop         = "10.0.0.1"
}
