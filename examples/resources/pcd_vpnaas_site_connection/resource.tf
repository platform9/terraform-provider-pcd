resource "pcd_networking_network" "example" {
  name = "tf-example-vpn-net"
}

resource "pcd_networking_subnet" "example" {
  name       = "tf-example-vpn-subnet"
  network_id = pcd_networking_network.example.id
  cidr       = "10.1.0.0/24"
}

resource "pcd_networking_router" "example" {
  name = "tf-example-vpn-router"
}

resource "pcd_vpnaas_service" "example" {
  name      = "tf-example-vpn"
  router_id = pcd_networking_router.example.id
}

resource "pcd_vpnaas_ike_policy" "example" {
  name = "tf-example-ike"
}

resource "pcd_vpnaas_ipsec_policy" "example" {
  name = "tf-example-ipsec"
}

resource "pcd_vpnaas_endpoint_group" "local" {
  name      = "tf-example-local-endpoints"
  type      = "subnet"
  endpoints = [pcd_networking_subnet.example.id]
}

resource "pcd_vpnaas_endpoint_group" "peer" {
  name      = "tf-example-peer-endpoints"
  type      = "cidr"
  endpoints = ["10.2.0.0/24"]
}

resource "pcd_vpnaas_site_connection" "example" {
  name              = "tf-example-connection"
  ike_policy_id     = pcd_vpnaas_ike_policy.example.id
  ipsec_policy_id   = pcd_vpnaas_ipsec_policy.example.id
  vpn_service_id    = pcd_vpnaas_service.example.id
  local_ep_group_id = pcd_vpnaas_endpoint_group.local.id
  peer_ep_group_id  = pcd_vpnaas_endpoint_group.peer.id
  peer_address      = "172.24.4.233"
  peer_id           = "172.24.4.233"
  psk               = "pre-shared-key"

  dpd = {
    action   = "restart"
    timeout  = 120
    interval = 30
  }
}
