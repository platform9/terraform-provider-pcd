terraform {
  required_providers {
    pcd = {
      source = "platform9/pcd"
    }
  }
}

provider "pcd" {
  auth_url    = "https://pcd.example.com/keystone/v3"
  region      = "Infra"
  user_name   = "admin@example.localnet"
  password    = var.pcd_password
  tenant_name = "service"

  user_domain_id    = "default"
  project_domain_id = "default"

  # Community Edition uses a self-signed certificate.
  insecure = true
}

variable "pcd_password" {
  type      = string
  sensitive = true
}

# Confirms authentication end to end.
data "pcd_identity_auth_scope" "current" {
  name = "current"
}

output "authenticated_user" {
  value = data.pcd_identity_auth_scope.current.user_name
}

output "project" {
  value = data.pcd_identity_auth_scope.current.project_name
}
