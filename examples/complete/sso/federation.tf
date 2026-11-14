# One rule per group, in the shape the PCD UI writes: the user's email address
# becomes their user name and email, and members of idp_group join the group.
# The remote types are the ids attribute-map.xml gives the SAML attributes.
resource "pcd_identity_federation_mapping" "this" {
  name = "${lower(var.identity_provider)}_mapping"
  rules = jsonencode([{
    local = [{
      user  = { name = "{0}", email = "{1}" }
      group = { id = pcd_identity_group.sso_users.id }
    }]
    remote = [
      { type = "email" },
      { type = "email" },
      { type = "groups", any_one_of = [var.idp_group] },
    ]
  }])
}

# The identity provider must have the SSO configuration's name and list its
# entity ID; taking both from the SSO configuration keeps them in step.
resource "pcd_identity_federation_identity_provider" "this" {
  name       = pcd_identity_sso_configuration.this.identity_provider
  domain_id  = var.domain_id
  remote_ids = [pcd_identity_sso_configuration.this.entity_id]
}

# PCD signs users in through the saml2 protocol and no other. Keystone deletes
# an identity provider's protocols with it, so identity_provider takes the
# identity provider's id, not its name: the id is unknown while the identity
# provider is replaced, and Terraform then replaces the protocol with it.
resource "pcd_identity_federation_protocol" "saml2" {
  identity_provider = pcd_identity_federation_identity_provider.this.id
  name              = "saml2"
  mapping_id        = pcd_identity_federation_mapping.this.name
}
