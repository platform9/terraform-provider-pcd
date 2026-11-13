# pcdctl role assignment list --names --inherited shows the grant; resolve each part with
#   pcdctl domain list, project list, group list, user list, and role list.
#   Leave the parts the grant does not use empty (a user on a domain: <domain_id>///<user_id>/<role_id>).
terraform import pcd_identity_inherit_role_assignment.example <domain_id>/<project_id>/<group_id>/<user_id>/<role_id>
