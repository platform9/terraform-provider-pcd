resource "pcd_blockstorage_volume_type" "example" {
  name        = "tf-example-ssd"
  description = "SSD-backed storage tier"
  is_public   = true

  extra_specs = {
    volume_backend_name = "synology-iscsi"
  }
}
