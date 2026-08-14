# Cluster roles are how hosts are onboarded: PCD expands each one into its
# granular pf9-* roles, with settings computed from the cluster blueprint and
# the host's host configuration.

# A compute host. host_cluster is required for the hypervisor role (create the
# cluster with pcd_cluster). wait_until_converged blocks until the host reports
# role_status = ok, so instances can be scheduled by resources later in the
# same apply.
resource "pcd_host_cluster_role" "hypervisor" {
  host_id              = "04575315-80ce-4617-9b96-6611d00c9942"
  role                 = "hypervisor"
  host_cluster         = pcd_cluster.main.name
  wait_until_converged = true
}

# Image library (Glance) on the same host.
resource "pcd_host_cluster_role" "image_library" {
  host_id = "04575315-80ce-4617-9b96-6611d00c9942"
  role    = "image-library"
}

# Block storage: `backends` names entries from the blueprint's
# storage_backends_json (its top-level keys).
resource "pcd_host_cluster_role" "storage" {
  host_id  = "04575315-80ce-4617-9b96-6611d00c9942"
  role     = "persistent-storage"
  backends = ["synology"]
}
