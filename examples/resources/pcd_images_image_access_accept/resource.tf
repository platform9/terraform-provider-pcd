# The member project's configuration: a provider scoped to that project
# accepts an image another project shared with it. Use literal values or
# variables for the alias's settings, not attributes of resources in the same
# configuration; the other settings come from the OS_* environment, which must
# not export OS_PROJECT_ID or OS_TENANT_ID next to the alias's tenant_name.
provider "pcd" {
  alias       = "team_b"
  tenant_name = "team-b"
}

# Find the shared image by name, whatever this project decided: "pending"
# would stop finding it once the image is accepted.
data "pcd_images_image" "golden" {
  provider      = pcd.team_b
  name          = "golden-ubuntu"
  visibility    = "shared"
  member_status = "all"
}

resource "pcd_images_image_access_accept" "golden" {
  provider = pcd.team_b
  image_id = data.pcd_images_image.golden.id
  status   = "accepted"
}
