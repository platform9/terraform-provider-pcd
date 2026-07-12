resource "pcd_identity_application_credential" "example" {
  name        = "tf-example-app-cred"
  description = "Application credential managed by Terraform"
  roles       = ["member"]
  expires_at  = "2027-01-01T00:00:00Z"
}
