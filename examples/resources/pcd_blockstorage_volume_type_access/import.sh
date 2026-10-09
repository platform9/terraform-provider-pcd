# volume_type_id: pcdctl volume type list --private. project_id: access_project_ids in pcdctl volume type show <volume_type_id>
terraform import pcd_blockstorage_volume_type_access.example <volume_type_id>/<project_id>
