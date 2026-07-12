data "pcd_identity_project" "demo" {
  name = "demo"
}

resource "pcd_blockstorage_quotaset" "example" {
  project_id = data.pcd_identity_project.demo.id
  volumes    = 50
  snapshots  = 50
  gigabytes  = 1000
  backups    = 20
}
