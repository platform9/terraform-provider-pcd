# Look an aggregate up by name, for example the one PCD keeps for a cluster
# (named after the cluster), to read its hosts.
data "pcd_compute_aggregate" "cluster" {
  name = "cluster-1"
}

output "cluster_hosts" {
  value = data.pcd_compute_aggregate.cluster.hosts
}
