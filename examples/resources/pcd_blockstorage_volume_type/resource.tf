# A volume type is what tenants pick when they create a volume. Its
# volume_backend_name must match a backend declared in the cluster blueprint's
# storage_backends_json (the "nfs" below pairs with the pcd_cluster_blueprint
# example). The blueprint's image_library_storage names a volume type too, so
# create the type before the blueprint.
resource "pcd_blockstorage_volume_type" "nfs" {
  name        = "nfs"
  description = "NFS-backed persistent storage"
  is_public   = true

  extra_specs = {
    volume_backend_name = "nfs"
  }
}
