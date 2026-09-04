# host_id is the resmgr host UUID: pcdctl hypervisor show <hypervisor-id> -c service_host
#   (or /etc/pf9/host_id.conf on the host; see the Importing guide for a host without roles).
# role_name: a granular pf9-* role, as listed under roles by GET /resmgr/v1/hosts/<host_id>.
terraform import pcd_host_role.example <host_id>/<role_name>
