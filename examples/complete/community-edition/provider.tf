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
# Terraform. The provider reads OS_AUTH_URL, OS_USERNAME, OS_PASSWORD,
# OS_PROJECT_NAME, and OS_REGION_NAME from the environment.
provider "pcd" {
  user_domain_id    = "default"
  project_domain_id = "default"

  # Community Edition ships a self-signed certificate.
  insecure = true
}
