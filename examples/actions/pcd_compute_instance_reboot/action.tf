terraform {
  required_version = ">= 1.14"
}

variable "reboot_generation" {
  description = "Change this value to reboot the instance again."
  type        = number
  default     = 1
}

resource "pcd_networking_network" "example" {
  name = "tf-example-network"
}

resource "pcd_networking_subnet" "example" {
  network_id = pcd_networking_network.example.id
  cidr       = "10.0.0.0/24"
}

resource "pcd_compute_instance" "web" {
  name        = "tf-example-web"
  image_name  = "Ubuntu-22.04"
  flavor_name = "m1.small"

  network {
    uuid = pcd_networking_network.example.id
  }

  depends_on = [pcd_networking_subnet.example]
}

# The PCD UI's Hard Reboot. Omit type (or set "SOFT") for a plain Reboot.
action "pcd_compute_instance_reboot" "web" {
  config {
    instance_id = pcd_compute_instance.web.id
    type        = "HARD"
  }
}

# Reboots the instance after it is created and whenever reboot_generation
# changes. For a one-off reboot without a trigger, run:
#   terraform apply -invoke=action.pcd_compute_instance_reboot.web
resource "terraform_data" "reboot_web" {
  input = var.reboot_generation

  # Created only once the instance exists, so the trigger never fires while
  # the instance is still being built.
  depends_on = [pcd_compute_instance.web]

  lifecycle {
    action_trigger {
      events  = [after_create, after_update]
      actions = [action.pcd_compute_instance_reboot.web]
    }
  }
}
