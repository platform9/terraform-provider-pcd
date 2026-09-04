# pcdctl role assignment list --names shows the assignment; resolve each part with
#   pcdctl domain list, project list, group list, user list, and role list.
#   Leave the parts the assignment does not use empty (e.g. <domain_id>//<group_id>//<role_id>).
terraform import pcd_identity_role_assignment.example <domain_id>/<project_id>/<group_id>/<user_id>/<role_id>
