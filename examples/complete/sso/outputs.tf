output "acs_url" {
  description = "PCD's reply (assertion consumer service) URL, as the identity provider must have it."
  value       = pcd_identity_sso_configuration.this.acs_url
}

output "sp_entity_id" {
  description = "PCD's SAML entity ID (identifier), as the identity provider must have it."
  value       = pcd_identity_sso_configuration.this.sp_entity_id
}

output "login_url" {
  description = "The path on PCD's host that starts a sign-in through the identity provider."
  value       = pcd_identity_sso_configuration.this.login_url
}
