# The PCD UI's "Domain-Level Access": alice gets the member role on every
# project in the Default domain, including projects created later.
data "pcd_identity_domain" "default" {
  name = "Default"
}

data "pcd_identity_role" "member" {
  name = "member"
}

data "pcd_identity_user" "alice" {
  name = "alice@example.com"
}

resource "pcd_identity_inherit_role_assignment" "example" {
  domain_id = data.pcd_identity_domain.default.id
  user_id   = data.pcd_identity_user.alice.id
  role_id   = data.pcd_identity_role.member.id
}
