resource "pcd_identity_domain" "example" {
  name        = "tf-example-domain"
  description = "Domain managed by Terraform"
}

# A project in the new domain. Destroying the domain deletes every project,
# user and group in it, so manage them in the same configuration: Terraform
# then destroys them first, explicitly.
resource "pcd_identity_project" "example" {
  name      = "tf-example-project"
  domain_id = pcd_identity_domain.example.id
}
