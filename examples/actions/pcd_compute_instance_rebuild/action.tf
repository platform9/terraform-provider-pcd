terraform {
  required_version = ">= 1.14"
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

# Reimage the instance with the image it already runs, returning its root disk
# to the image's contents. The instance keeps its ID, IP addresses and volumes.
action "pcd_compute_instance_rebuild" "web" {
  config {
    instance_id = pcd_compute_instance.web.id
  }
}

# Bump reimage_generation to reimage the instance again on the next apply.
variable "reimage_generation" {
  type    = number
  default = 1
}

resource "terraform_data" "reimage_web" {
  input = var.reimage_generation

  lifecycle {
    action_trigger {
      events  = [after_update]
      actions = [action.pcd_compute_instance_rebuild.web]
    }
  }
}

# Or once, without a trigger:
#   terraform apply -invoke=action.pcd_compute_instance_rebuild.web
