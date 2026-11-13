# The PCD UI uses a user's email address as the user name and expects email to
# match it; display_name fills the Users list's Display Name column.
resource "pcd_identity_user" "example" {
  name         = "alice@example.com"
  email        = "alice@example.com"
  display_name = "Alice Example"
  description  = "Example user managed by Terraform"
  enabled      = true
  password     = "ChangeMe-123!"
}
