variable "identity_provider" {
  type        = string
  description = "The identity provider's name in PCD: IDP1 for the default domain, which is what the PCD UI uses."
  default     = "IDP1"
}

variable "domain_id" {
  type        = string
  description = "The domain users who sign in through the identity provider belong to."
  default     = "default"
}

variable "sso_provider" {
  type        = string
  description = "The identity provider product: okta, oneLogin, google, or another name."
  default     = "okta"
}

variable "entity_id" {
  type        = string
  description = "The identity provider's SAML entity ID (issuer)."
}

variable "metadata_url" {
  type        = string
  description = "The URL of the identity provider's SAML metadata. PCD downloads it, so it must be reachable from PCD."
}

variable "idp_group" {
  type        = string
  description = "The group, as the identity provider's groups attribute names it, whose members may sign in."
  default     = "pcd-users"
}

variable "group_name" {
  type        = string
  description = "The PCD group that users who sign in join."
  default     = "sso-users"
}

variable "project_name" {
  type        = string
  description = "The existing project the group gets a role on."
}

variable "role_name" {
  type        = string
  description = "The role the group gets on the project."
  default     = "member"
}
