resource "pcd_networking_network" "example" {
  name = "tf-example-lb-net"
}

resource "pcd_networking_subnet" "example" {
  network_id = pcd_networking_network.example.id
  cidr       = "10.0.0.0/24"
}

resource "pcd_lb_loadbalancer" "example" {
  name          = "tf-example-lb"
  vip_subnet_id = pcd_networking_subnet.example.id
}

resource "pcd_lb_pool" "example" {
  name            = "tf-example-pool"
  loadbalancer_id = pcd_lb_loadbalancer.example.id
  protocol        = "HTTP"
  lb_method       = "ROUND_ROBIN"
}

resource "pcd_lb_member" "example" {
  pool_id       = pcd_lb_pool.example.id
  address       = "10.0.0.10"
  protocol_port = 8080
  subnet_id     = pcd_networking_subnet.example.id
}
