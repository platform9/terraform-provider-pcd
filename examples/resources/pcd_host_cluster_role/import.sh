# host_id is the resmgr host UUID: pcdctl hypervisor show <hypervisor-id> -c service_host
#   (or /etc/pf9/host_id.conf on the host; see the Importing guide for a host without roles).
# The role is one of hypervisor, image-library, persistent-storage, dns.
terraform import pcd_host_cluster_role.hypervisor <host_id>/hypervisor
