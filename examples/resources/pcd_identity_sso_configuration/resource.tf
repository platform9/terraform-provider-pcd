# PCD's SAML configuration for the default domain's identity provider, IDP1.
# Saving it restarts Keystone for about half a minute, sometimes longer; the resource waits.
resource "pcd_identity_sso_configuration" "idp1" {
  identity_provider = "IDP1"
  sso_provider      = "okta"
  entity_id         = "http://www.okta.com/exk1a2b3c4d5e6f7g8h9"
  metadata_url      = "https://example.okta.com/app/exk1a2b3c4d5e6f7g8h9/sso/saml/metadata"
  attribute_map_xml = file("${path.module}/attribute-map.xml")
}

# Register these with the identity provider.
output "sso_acs_url" {
  value = pcd_identity_sso_configuration.idp1.acs_url
}

output "sso_sp_entity_id" {
  value = pcd_identity_sso_configuration.idp1.sp_entity_id
}
