resource "pcd_networking_network" "example" {
  name = "tf-example-network"
}

resource "pcd_networking_subnet" "example" {
  network_id = pcd_networking_network.example.id
  name       = "tf-example-subnet"
  cidr       = "10.0.0.0/24"
  ip_version = 4
}

resource "pcd_networking_port" "example" {
  network_id     = pcd_networking_network.example.id
  name           = "tf-example-port"
  description    = "Managed by Terraform"
  admin_state_up = true

  fixed_ip = [
    {
      subnet_id  = pcd_networking_subnet.example.id
      ip_address = "10.0.0.10"
    }
  ]

  allowed_address_pairs = [
    {
      ip_address = "10.0.0.20"
    }
  ]

  tags = ["env:example"]
}
