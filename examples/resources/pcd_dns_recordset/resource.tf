resource "pcd_dns_zone" "example" {
  name  = "example.com."
  email = "admin@example.com"
}

resource "pcd_dns_recordset" "example" {
  zone_id = pcd_dns_zone.example.id
  name    = "www.example.com."
  type    = "A"
  ttl     = 300
  records = ["10.0.0.1", "10.0.0.2"]
}
