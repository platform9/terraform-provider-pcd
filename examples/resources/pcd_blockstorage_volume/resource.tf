resource "pcd_blockstorage_volume" "example" {
  name              = "tf-example-volume"
  size              = 10
  description       = "Example block storage volume managed by Terraform"
  volume_type       = "__DEFAULT__"
  availability_zone = "nova"

  metadata = {
    environment = "dev"
  }
}
