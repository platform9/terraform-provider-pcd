# The host is named, not identified by UUID: pcd_host resolves the
# resource-manager UUID from the hostname its host agent reports.
data "pcd_host" "dns" {
  name = var.dns_host_name
}

# The dns cluster role installs designate-worker and designate-mdns on the
# host. It converges in a few minutes; everything below needs it running, so
# the wait is on.
resource "pcd_host_cluster_role" "dns" {
  host_id              = data.pcd_host.dns.id
  role                 = "dns"
  wait_until_converged = true

  # Uncomment to serve IPv6 backends (see "IPv6" in the DNS guide).
  # settings = {
  #   listen = "[::]:5354"
  # }
}
