resource "pcd_blockstorage_volume" "example" {
  name              = "tf-example-volume"
  size              = 10
  description       = "Example block storage volume managed by Terraform"
  volume_type       = "__DEFAULT__"
  availability_zone = "nova"

  # Changing volume_type retypes the volume in place. "on-demand" lets Cinder
  # move the data when the new type is on another storage backend; the
  # default, "never", fails such a retype instead.
  volume_retype_policy = "on-demand"

  metadata = {
    environment = "dev"
  }
}
