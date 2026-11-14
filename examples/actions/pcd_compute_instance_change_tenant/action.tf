terraform {
  required_version = ">= 1.14"
}

data "pcd_identity_project" "team_b" {
  name = "team-b"
}

# Nova moves an instance only when the target project can use its network,
# image and flavor, and owns every security group on its ports (security
# groups do not move). So this example assumes a public image and flavor, and
# puts the instance on a shared network through a port with no security groups.
resource "pcd_networking_network" "example" {
  name   = "tf-example-network"
  shared = true
}

resource "pcd_networking_subnet" "example" {
  network_id = pcd_networking_network.example.id
  cidr       = "10.0.0.0/24"
}

resource "pcd_networking_port" "web" {
  network_id         = pcd_networking_network.example.id
  security_group_ids = []

  depends_on = [pcd_networking_subnet.example]
}

resource "pcd_compute_instance" "web" {
  name        = "tf-example-web"
  image_name  = "Ubuntu-22.04"
  flavor_name = "m1.small"

  network {
    port = pcd_networking_port.web.id
  }
}

# The PCD UI's Change Tenant: moves the instance, its ports and its attached
# volumes to team-b. Needs an admin token. Run it once:
#   terraform apply -invoke=action.pcd_compute_instance_change_tenant.web
# then hand the instance over to team-b's configuration (see "Moving an
# instance to another project" in the importing guide).
action "pcd_compute_instance_change_tenant" "web" {
  config {
    instance_id = pcd_compute_instance.web.id
    project_id  = data.pcd_identity_project.team_b.id
  }
}

# Several instances: one action instance per VM. Set the variable and run one
# on its own with:
#   terraform apply -var 'move_to_team_b=["<instance id>"]' \
#     -invoke='action.pcd_compute_instance_change_tenant.batch["<instance id>"]'
variable "move_to_team_b" {
  description = "IDs of the instances to move to team-b."
  type        = set(string)
  default     = []
}

action "pcd_compute_instance_change_tenant" "batch" {
  for_each = var.move_to_team_b

  config {
    instance_id = each.value
    project_id  = data.pcd_identity_project.team_b.id
  }
}
