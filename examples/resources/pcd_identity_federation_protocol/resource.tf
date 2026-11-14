# PCD signs users in through the identity provider's saml2 protocol, which
# applies the mapping. In a configuration that also manages them, set
# identity_provider from the pcd_identity_federation_identity_provider's id
# (not its name, so that replacing the identity provider, which deletes its
# protocols, replaces the protocol too) and mapping_id from the
# pcd_identity_federation_mapping's name, so they are created first.
resource "pcd_identity_federation_protocol" "saml2" {
  identity_provider = "IDP1"
  name              = "saml2"
  mapping_id        = "idp1_mapping"
}
