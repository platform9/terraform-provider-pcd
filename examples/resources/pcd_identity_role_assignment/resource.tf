resource "pcd_identity_project" "example" {
  name = "tf-example-project"
}

resource "pcd_identity_user" "example" {
  name = "tf-example-user"
}

resource "pcd_identity_role" "example" {
  name = "tf-example-role"
}

resource "pcd_identity_role_assignment" "example" {
  role_id    = pcd_identity_role.example.id
  user_id    = pcd_identity_user.example.id
  project_id = pcd_identity_project.example.id
}
