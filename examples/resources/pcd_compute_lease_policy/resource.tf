data "pcd_identity_project" "dev" {
  name = "dev"
}

# VMs in the dev project live three days from their creation; then mors powers
# them off. Creating the policy acts within a minute on VMs already older.
resource "pcd_compute_lease_policy" "dev" {
  project_id     = data.pcd_identity_project.dev.id
  expiry_minutes = 3 * 24 * 60
  action         = "power off"
}
