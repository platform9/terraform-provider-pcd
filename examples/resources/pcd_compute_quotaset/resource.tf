data "pcd_identity_project" "demo" {
  name = "demo"
}

resource "pcd_compute_quotaset" "example" {
  project_id = data.pcd_identity_project.demo.id
  cores      = 32
  ram        = 65536
  instances  = 20
  key_pairs  = 10
}

# One user's quotas within the project, as on the PCD UI's Users page. A user's
# quotas must fit within what the project has left, so this one waits for the
# project's quotas above. Destroying it removes all of alice's overrides in the
# project.
data "pcd_identity_user" "alice" {
  name = "alice@example.com"
}

resource "pcd_compute_quotaset" "alice" {
  project_id = data.pcd_identity_project.demo.id
  user_id    = data.pcd_identity_user.alice.id
  cores      = 8
  ram        = 16384
  instances  = 4

  depends_on = [pcd_compute_quotaset.example]
}
