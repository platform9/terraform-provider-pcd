# The image owner shares an image with another project. Members exist only
# while the image's visibility is "shared".
resource "pcd_images_image" "golden" {
  name             = "golden-ubuntu"
  container_format = "bare"
  disk_format      = "qcow2"
  image_source_url = "https://cloud-images.ubuntu.com/jammy/current/jammy-server-cloudimg-amd64.img"
  visibility       = "shared"
}

resource "pcd_identity_project" "team_b" {
  name = "team-b"
}

# The member project accepts with pcd_images_image_access_accept. An admin can
# decide for it instead by setting status here; never do both.
resource "pcd_images_image_access" "team_b" {
  image_id  = pcd_images_image.golden.id
  member_id = pcd_identity_project.team_b.id
}
