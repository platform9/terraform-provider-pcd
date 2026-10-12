# Offer a volume from the project this configuration signs in to.
resource "pcd_blockstorage_volume" "example" {
  name = "app-data"
  size = 10
}

resource "pcd_blockstorage_volume_transfer" "example" {
  volume_id = pcd_blockstorage_volume.example.id
  name      = "app-data to team-b"
}

# The receiving project needs both values, for example through remote state
# or a secret store.
output "transfer_id" {
  value = pcd_blockstorage_volume_transfer.example.id
}

output "transfer_auth_key" {
  value     = pcd_blockstorage_volume_transfer.example.auth_key
  sensitive = true
}

# Once the receiving project has accepted the offer, the volume, this offer
# and any pcd_blockstorage_snapshot of the volume belong to that project.
# Replace their resource blocks above with removed blocks, so Terraform
# forgets them without destroying anything (Terraform 1.7 or later):
#
# removed {
#   from = pcd_blockstorage_volume.example
#   lifecycle {
#     destroy = false
#   }
# }
#
# removed {
#   from = pcd_blockstorage_volume_transfer.example
#   lifecycle {
#     destroy = false
#   }
# }
