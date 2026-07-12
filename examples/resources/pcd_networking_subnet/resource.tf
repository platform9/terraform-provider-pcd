resource "pcd_networking_network" "example" {
  name = "tf-example-network"
}

resource "pcd_networking_subnet" "example" {
  network_id      = pcd_networking_network.example.id
  name            = "tf-example-subnet"
  description     = "Managed by Terraform"
  cidr            = "10.0.0.0/24"
  ip_version      = 4
  gateway_ip      = "10.0.0.1"
  enable_dhcp     = true
  dns_nameservers = ["8.8.8.8", "8.8.4.4"]

  allocation_pools = [
    {
      start = "10.0.0.10"
      end   = "10.0.0.200"
    }
  ]

  tags = ["env:dev"]
}
