# host_id is the resmgr host UUID: pcdctl hypervisor show <hypervisor-id> -c service_host
#   (or /etc/pf9/host_id.conf on the host; see the Importing guide for a host without roles).
# host_config_id: GET /resmgr/v2/hostconfigs (see the Importing guide).
terraform import pcd_host_config_assignment.example <host_id>/<host_config_id>
