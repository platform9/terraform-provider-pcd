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
