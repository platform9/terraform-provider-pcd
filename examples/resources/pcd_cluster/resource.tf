# A cluster (host cluster / host group) is the unit hypervisors join. VM HA
# and auto-rebalancing are cluster-scoped settings.
resource "pcd_cluster" "main" {
  name = "cluster-1"

  vm_high_availability = {
    enabled = true
  }

  auto_resource_rebalancing = {
    enabled                    = true
    rebalancing_strategy       = "vm_workload_consolidation"
    rebalancing_frequency_mins = 20
  }
}

# Hypervisors join the cluster through their cluster role.
resource "pcd_host_cluster_role" "hypervisor" {
  host_id      = "04575315-80ce-4617-9b96-6611d00c9942"
  role         = "hypervisor"
  host_cluster = pcd_cluster.main.name
}
