data "pcd_identity_project" "dev" {
  name = "dev"
}

resource "pcd_compute_lease_policy" "dev" {
  project_id     = data.pcd_identity_project.dev.id
  expiry_minutes = 3 * 24 * 60
}

data "pcd_networking_network" "dev" {
  name = "dev-network"
}

# The VM boots in the provider's project, here dev, the project of the policy.
resource "pcd_compute_instance" "build_agent" {
  name        = "build-agent"
  image_name  = "Ubuntu-22.04"
  flavor_name = "m1.small"

  network {
    uuid = data.pcd_networking_network.dev.id
  }
}

variable "build_agent_lease_renewal" {
  type        = number
  description = "Bump to move the build agent's lease two days past the apply."
  default     = 1
}

# Two days from the apply that created it (or last bumped the renewal), in the
# form mors takes. time_offset comes from the hashicorp/time provider; it keeps
# the value fixed between applies, where timeadd(timestamp(), ...) would change
# on every plan.
resource "time_offset" "build_agent_lease" {
  offset_days = 2
  triggers = {
    renewal = var.build_agent_lease_renewal
  }
}

# The build agent's own lease, in place of the project's three days. Setting
# project_id from the policy creates the policy first.
resource "pcd_compute_instance_lease" "build_agent" {
  instance_id = pcd_compute_instance.build_agent.id
  project_id  = pcd_compute_lease_policy.dev.project_id
  expiry      = time_offset.build_agent_lease.rfc3339
  action      = "power off"
}

# For a VM this configuration does not manage (created in the PCD UI, say),
# look it up by name with the pcd_compute_instance data source:
#
# data "pcd_compute_instance" "legacy" {
#   name = "legacy-build-agent"
# }
#
# resource "pcd_compute_instance_lease" "legacy" {
#   instance_id = data.pcd_compute_instance.legacy.id
#   project_id  = pcd_compute_lease_policy.dev.project_id
#   expiry      = time_offset.build_agent_lease.rfc3339
# }
