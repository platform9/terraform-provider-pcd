# Resolve a host's resource-manager UUID from the hostname its host agent
# reports (usually the FQDN; the Hosts page in the PCD UI shows the same value).
data "pcd_host" "hyp1" {
  name = "hyp1.example.com"
}

# id is the host_id every host-scoped resource takes.
resource "pcd_host_config_assignment" "hyp1" {
  host_id        = data.pcd_host.hyp1.id
  host_config_id = pcd_host_config.example.id
}
