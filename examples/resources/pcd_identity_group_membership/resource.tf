resource "pcd_identity_group" "example" {
  name = "tf-example-group"
}

resource "pcd_identity_user" "example" {
  name     = "tf-example-user"
  password = "ChangeMe-123!"
}

resource "pcd_identity_group_membership" "example" {
  group_id = pcd_identity_group.example.id
  user_id  = pcd_identity_user.example.id
}
