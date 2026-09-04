# Find the volume type ID with: pcdctl volume type list
#   (the name alone is not accepted; pcdctl volume type show <name> -f value -c id prints the ID)
terraform import pcd_blockstorage_volume_type.example <volume_type_id>
