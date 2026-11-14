data "pcd_identity_project" "sso" {
  name = var.project_name
}

data "pcd_identity_role" "sso" {
  name = var.role_name
}

# The group users join when they sign in. depends_on puts it, and everything
# built on it, after the Keystone restart the SSO configuration causes.
resource "pcd_identity_group" "sso_users" {
  name        = var.group_name
  description = "Users who sign in through ${var.sso_provider}"
  domain_id   = var.domain_id

  depends_on = [pcd_identity_sso_configuration.this]
}

resource "pcd_identity_role_assignment" "sso_users" {
  group_id   = pcd_identity_group.sso_users.id
  project_id = data.pcd_identity_project.sso.id
  role_id    = data.pcd_identity_role.sso.id
}

# The PCD UI's SAML Groups page grants the role on every project in the
# domain instead, as an inherited grant. To do the same, use this in place of
# the assignment above:
#
# resource "pcd_identity_inherit_role_assignment" "sso_users" {
#   group_id  = pcd_identity_group.sso_users.id
#   domain_id = var.domain_id
#   role_id   = data.pcd_identity_role.sso.id
# }
