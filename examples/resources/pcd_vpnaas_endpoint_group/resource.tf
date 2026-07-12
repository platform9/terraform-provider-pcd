resource "pcd_vpnaas_endpoint_group" "peer" {
  name      = "tf-example-peer-endpoints"
  type      = "cidr"
  endpoints = ["10.2.0.0/24", "10.3.0.0/24"]
}
