resource "pcd_identity_group" "sso_admins" {
  name = "sso-admins"
}

# One rule per SSO group, in the shape the PCD UI writes: a user who signs in
# gets their email address as user name and email, and joins sso-admins when
# the identity provider lists them in its "pcd-admins" group. "email" and
# "groups" are the ids the SSO configuration's attribute map gives the SAML
# attributes.
resource "pcd_identity_federation_mapping" "idp1" {
  name = "idp1_mapping"
  rules = jsonencode([{
    local = [{
      user  = { name = "{0}", email = "{1}" }
      group = { id = pcd_identity_group.sso_admins.id }
    }]
    remote = [
      { type = "email" },
      { type = "email" },
      { type = "groups", any_one_of = ["pcd-admins"] },
    ]
  }])
}
