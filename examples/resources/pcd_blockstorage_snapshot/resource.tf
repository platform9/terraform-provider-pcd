resource "pcd_blockstorage_volume" "example" {
  name = "tf-example-volume"
  size = 1
}

resource "pcd_blockstorage_snapshot" "example" {
  name      = "tf-example-snapshot"
  volume_id = pcd_blockstorage_volume.example.id
}
