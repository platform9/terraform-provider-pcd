resource "pcd_identity_user" "example" {
  name        = "tf-example-user"
  description = "Example user managed by Terraform"
  enabled     = true
  password    = "ChangeMe-123!"
}
