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

# A subnet whose fixed IPs are published to the network's DNS zone. The
# network names the zone (dns_domain); the subnet opts in. On an external
# network this is required before any record is created; see the DNS guide.
resource "pcd_networking_network" "app" {
  name       = "app-net"
  dns_domain = "app.example.com."
}

resource "pcd_networking_subnet" "app" {
  network_id           = pcd_networking_network.app.id
  name                 = "app-subnet"
  cidr                 = "10.1.0.0/24"
  dns_publish_fixed_ip = true
}

# A subnet with no gateway (the UI's "Disable Gateway"). Setting no_gateway to
# false later restores Neutron's default gateway in place, 10.2.0.1 here; the
# pool starts above it so that address stays free.
resource "pcd_networking_subnet" "isolated" {
  network_id = pcd_networking_network.example.id
  name       = "isolated-subnet"
  cidr       = "10.2.0.0/24"
  no_gateway = true

  allocation_pools = [
    {
      start = "10.2.0.10"
      end   = "10.2.0.200"
    }
  ]
}
