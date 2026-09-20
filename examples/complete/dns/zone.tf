# The zone. Creating it needs the pool above: Designate schedules the zone
# onto the pool and pushes it to BIND before reporting ACTIVE.
resource "pcd_dns_zone" "app" {
  name  = var.zone_name
  email = var.zone_email
  ttl   = 300

  depends_on = [ssh_resource.pools]
}
