# The dns cluster role installs designate-worker and designate-mdns on the
# host. It converges in a few minutes; everything below needs it running, so
# the wait is on.
resource "pcd_host_cluster_role" "dns" {
  host_id              = var.dns_host_id
  role                 = "dns"
  wait_until_converged = true
}
