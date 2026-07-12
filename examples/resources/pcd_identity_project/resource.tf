resource "pcd_identity_project" "example" {
  name        = "tf-example-project"
  description = "Project managed by Terraform"
  enabled     = true
  tags        = ["terraform", "example"]
}
