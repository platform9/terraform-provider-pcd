# PCD's SAML configuration. Saving it restarts Keystone and the Shibboleth
# daemon for a minute or two; the resource waits until both answer again.
resource "pcd_identity_sso_configuration" "this" {
  identity_provider = var.identity_provider
  sso_provider      = var.sso_provider
  entity_id         = var.entity_id
  metadata_url      = var.metadata_url
  attribute_map_xml = file("${path.module}/attribute-map.xml")
}
