# PCD signs users in through the identity provider's saml2 protocol, which
# applies the mapping. In a configuration that also manages them, reference
# the pcd_identity_federation_identity_provider and
# pcd_identity_federation_mapping names instead, so they are created first.
resource "pcd_identity_federation_protocol" "saml2" {
  identity_provider = "IDP1"
  name              = "saml2"
  mapping_id        = "idp1_mapping"
}
