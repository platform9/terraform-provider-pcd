resource "pcd_images_image" "example" {
  name             = "tf-example-ubuntu"
  container_format = "bare"
  disk_format      = "qcow2"
  image_source_url = "https://cloud-images.ubuntu.com/jammy/current/jammy-server-cloudimg-amd64.img"

  min_disk_gb = 10
  min_ram_mb  = 2048
  visibility  = "private"
  tags        = ["ubuntu", "jammy"]

  properties = {
    os_distro  = "ubuntu"
    os_version = "22.04"
  }
}
