# Accept, in another project, a volume offered with
# pcd_blockstorage_volume_transfer. The aliased provider block switches only
# the project. It does not inherit the default provider block's arguments:
# what it leaves out comes from the OS_* environment variables or clouds.yaml.
# Name the project with tenant_name or tenant_id (both work).
provider "pcd" {
  alias       = "team_b"
  tenant_name = "team-b"
}

variable "transfer_id" {
  type = string
}

variable "transfer_auth_key" {
  type      = string
  sensitive = true
}

resource "pcd_blockstorage_volume_transfer_accept" "example" {
  provider    = pcd.team_b
  transfer_id = var.transfer_id
  auth_key    = var.transfer_auth_key
}

output "volume_id" {
  value = pcd_blockstorage_volume_transfer_accept.example.volume_id
}

# After the apply, manage the volume in this project with an import block
# (Terraform 1.5 or later), using the volume_id output:
#
# import {
#   provider = pcd.team_b
#   to       = pcd_blockstorage_volume.data
#   id       = "<volume_id>"
# }
#
# resource "pcd_blockstorage_volume" "data" {
#   provider = pcd.team_b
#   name     = "app-data"
#   size     = 10
# }
