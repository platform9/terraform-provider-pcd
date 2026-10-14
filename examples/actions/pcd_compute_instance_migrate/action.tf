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

# A shut-off instance in cluster-1, which the to_cluster_2 action below moves.
resource "pcd_compute_instance" "batch" {
  name              = "tf-example-batch"
  image_name        = "Ubuntu-22.04"
  flavor_name       = "m1.small"
  availability_zone = "cluster-1"
  power_state       = "shutoff"

  network {
    uuid = pcd_networking_network.example.id
  }

  depends_on = [pcd_networking_subnet.example]
}

data "pcd_host" "hyp2" {
  name = "hyp2.example.com"
}

# Live-migrate the instance to hyp2 (omit host to let the scheduler choose):
#   terraform apply -invoke=action.pcd_compute_instance_migrate.web
action "pcd_compute_instance_migrate" "web" {
  config {
    instance_id = pcd_compute_instance.web.id
    host        = data.pcd_host.hyp2.id
  }
}

# Or migrate whenever migrate_generation changes.
variable "migrate_generation" {
  type    = number
  default = 1
}

resource "terraform_data" "migrate_web" {
  input = var.migrate_generation

  lifecycle {
    action_trigger {
      events  = [after_update]
      actions = [action.pcd_compute_instance_migrate.web]
    }
  }
}

# Move a shut-off instance to another cluster: name a host in that cluster and
# the cluster's availability zone. Once the move is done, change the
# instance's availability_zone to that cluster before the next plan; changing
# it in the same apply as the move would replace the instance.
action "pcd_compute_instance_migrate" "to_cluster_2" {
  config {
    instance_id       = pcd_compute_instance.batch.id
    host              = data.pcd_host.hyp3.id
    availability_zone = "cluster-2"
  }
}

data "pcd_host" "hyp3" {
  name = "hyp3.example.com"
}

# Several instances: one action instance per VM, and the scheduler picks each
# target. Terraform runs the triggered actions in parallel, up to its
# -parallelism. One can also be run on its own:
#   terraform apply -invoke='action.pcd_compute_instance_migrate.evacuate["<instance id>"]'
variable "evacuate_ids" {
  type    = set(string)
  default = []
}

action "pcd_compute_instance_migrate" "evacuate" {
  for_each = var.evacuate_ids

  config {
    instance_id = each.value
  }
}

resource "terraform_data" "evacuate" {
  for_each = var.evacuate_ids
  input    = var.migrate_generation

  lifecycle {
    action_trigger {
      events  = [after_update]
      actions = [action.pcd_compute_instance_migrate.evacuate[each.key]]
    }
  }
}
