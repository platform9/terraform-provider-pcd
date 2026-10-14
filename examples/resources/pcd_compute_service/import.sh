# The import ID is the hypervisor's resmgr host UUID:
#   pcdctl compute service list --service nova-compute -c Host
terraform import pcd_compute_service.hyp1 <host_id>
