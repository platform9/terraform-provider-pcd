data "pcd_identity_project" "demo" {
  name = "demo"
}

resource "pcd_networking_quota" "example" {
  project_id     = data.pcd_identity_project.demo.id
  network        = 20
  port           = 200
  router         = 10
  subnet         = 20
  security_group = 20
  floatingip     = 50
}
