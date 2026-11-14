resource "pcd_cluster" "main" {
  name = "cluster-1"

  vm_high_availability = {
    enabled = true
  }
}

# The cluster's VM high-availability settings: evacuate a failed host's VMs
# with the fastest evacuation speed, and tell a host cut off from the network
# from a failed one through a heartbeat on shared NFS storage, mounted at this
# path on every hypervisor in the cluster.
resource "pcd_cluster_vmha_config" "main" {
  cluster_name       = pcd_cluster.main.name
  evacuation_speed   = "aggressive"
  nfs_heartbeat_path = "/mnt/vmha-heartbeat"
}
