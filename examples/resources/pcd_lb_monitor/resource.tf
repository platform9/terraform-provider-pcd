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
  protocol        = "TCP" # OVN provider is L4
  lb_method       = "SOURCE_IP_PORT"
}

resource "pcd_lb_monitor" "example" {
  pool_id     = pcd_lb_pool.example.id
  type        = "TCP"
  delay       = 10
  timeout     = 5
  max_retries = 3
}
