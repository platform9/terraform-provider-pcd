terraform {
  required_providers {
    pcd = {
      source  = "platform9/pcd"
      version = "~> 0.1.22"
    }
  }
}

# Credentials come from the pcdctl RC file: in the PCD UI open Settings > API
# Access, copy it, set OS_PASSWORD, and `source pcdctlrc` before running
# Terraform. Configuring single sign-on needs the admin role.
provider "pcd" {
  user_domain_id    = "default"
  project_domain_id = "default"

  # For a PCD with a self-signed certificate. Remove it, or set cacert_file,
  # when the certificate is one this machine trusts.
  insecure = true
}
