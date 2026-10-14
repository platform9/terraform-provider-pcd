terraform {
  required_providers {
    pcd = {
      source  = "platform9/pcd"
      version = "~> 0.1"
    }
  }
}

# Credentials come from the pcdctl RC file: in the PCD UI open Settings > API
# Access, copy it, set OS_PASSWORD, and `source pcdctlrc` before running
# Terraform. Failure domains are host aggregates, which need an admin user.
provider "pcd" {
  user_domain_id    = "default"
  project_domain_id = "default"

  # For a PCD with a self-signed certificate; remove it when the certificate
  # is trusted.
  insecure = true
}
