# Cluster roles are how hosts are onboarded: PCD expands each one into its
# granular pf9-* roles, with settings computed from the cluster blueprint and
# the host's host configuration.

# The host is named, not pasted in by UUID: pcd_host resolves the
# resource-manager UUID from the hostname the host reports.
data "pcd_host" "hyp1" {
  name = "hyp1.example.com"
}

# A compute host. host_cluster is required for the hypervisor role (create the
# cluster with pcd_cluster). wait_until_converged blocks until the host reports
# role_status = ok, so instances can be scheduled by resources later in the
# same apply.
resource "pcd_host_cluster_role" "hypervisor" {
  host_id              = data.pcd_host.hyp1.id
  role                 = "hypervisor"
  host_cluster         = pcd_cluster.main.name
  wait_until_converged = true
}

# Image library (Glance) on the same host.
resource "pcd_host_cluster_role" "image_library" {
  host_id = data.pcd_host.hyp1.id
  role    = "image-library"
}

# Block storage: `backends` names driver configurations from the blueprint's
# storage_backends_json, i.e. its second-level keys (for a blueprint declaring
# { "synology": { "synology-iscsi": { driver = ..., config = {...} } } } that is
# "synology-iscsi"). The top-level key is what a volume type's
# volume_backend_name refers to.
resource "pcd_host_cluster_role" "storage" {
  host_id  = data.pcd_host.hyp1.id
  role     = "persistent-storage"
  backends = ["synology-iscsi"]
}
