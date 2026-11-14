# The Keystone side of PCD single sign-on. For single sign-on, name must equal
# the identity_provider of the SSO configuration and remote_ids must hold its
# entity_id; the SSO guide sets both from pcd_identity_sso_configuration, so
# the three can never disagree.
resource "pcd_identity_federation_identity_provider" "idp1" {
  name       = "IDP1"
  domain_id  = "default"
  remote_ids = ["https://idp.example.com/entity"]
}
