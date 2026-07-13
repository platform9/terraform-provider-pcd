resource "pcd_blockstorage_volume" "example" {
  name = "tf-example-volume"
  size = 1
}

resource "pcd_blockstorage_volume_backup" "example" {
  name      = "tf-example-backup"
  volume_id = pcd_blockstorage_volume.example.id
}
