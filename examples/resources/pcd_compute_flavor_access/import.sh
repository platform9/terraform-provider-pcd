# flavor_id: pcdctl flavor list --private. tenant_id: access_project_ids in pcdctl flavor show <flavor_id>
terraform import pcd_compute_flavor_access.example <flavor_id>/<tenant_id>
