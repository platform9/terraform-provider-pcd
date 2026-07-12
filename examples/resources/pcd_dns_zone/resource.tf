resource "pcd_dns_zone" "example" {
  name        = "example.com."
  email       = "admin@example.com"
  ttl         = 3600
  description = "Managed by Terraform"
}
