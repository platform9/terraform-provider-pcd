# A private volume type is usable only by the projects granted access to it;
# even the project that created it needs a grant.
resource "pcd_blockstorage_volume_type" "fast" {
  name      = "fast-ssd"
  is_public = false

  extra_specs = {
    volume_backend_name = "ssd"
  }
}

resource "pcd_identity_project" "db_team" {
  name = "db-team"
}

resource "pcd_blockstorage_volume_type_access" "db_team" {
  volume_type_id = pcd_blockstorage_volume_type.fast.id
  project_id     = pcd_identity_project.db_team.id
}
