# project_id: pcdctl project list. region: pcdctl region list (or the provider's region).
terraform import pcd_compute_quotaset.example <project_id>/<region>

# A user's quotas within the project: append the user ID (pcdctl user list).
terraform import pcd_compute_quotaset.alice <project_id>/<region>/<user_id>
