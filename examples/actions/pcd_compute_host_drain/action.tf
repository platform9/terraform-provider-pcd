terraform {
  required_version = ">= 1.14"
}

data "pcd_host" "hyp1" {
  name = "hyp1.example.com"
}

# The host in maintenance, as the PCD UI's Enable Maintenance Mode leaves it.
resource "pcd_compute_service" "hyp1" {
  host_id = data.pcd_host.hyp1.id
  status  = "maintenance"
}

# Moves every instance off the host; run it once the host is in maintenance:
#   terraform apply -invoke=action.pcd_compute_host_drain.hyp1
# Running it again moves whatever is still there.
action "pcd_compute_host_drain" "hyp1" {
  config {
    host_id = pcd_compute_service.hyp1.host_id
  }
}

# Or drain on every entry into maintenance: each one writes a new
# disabled_reason, which changes this resource's input and triggers the action.
# Leaving maintenance changes it too, so the condition keeps a move to
# "enabled" or "disabled" from triggering a drain ("disabled" clears the reason
# but still takes the host out of scheduling, which the action would drain).
resource "terraform_data" "drain_hyp1" {
  input = pcd_compute_service.hyp1.disabled_reason

  lifecycle {
    action_trigger {
      events    = [after_create, after_update]
      condition = pcd_compute_service.hyp1.status == "maintenance"
      actions   = [action.pcd_compute_host_drain.hyp1]
    }
  }
}
