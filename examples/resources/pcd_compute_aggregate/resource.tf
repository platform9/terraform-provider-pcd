# Hypervisors resolved from the hostnames they report.
data "pcd_host" "hyp1" {
  name = "hyp1.example.com"
}

data "pcd_host" "hyp2" {
  name = "hyp2.example.com"
}

# A host aggregate inside cluster-1: zone is the cluster's name, because PCD
# makes each cluster an availability zone named after it, and a host can only
# join aggregates of its own cluster's zone. Scheduler filters that match
# aggregate metadata (against flavor extra specs, for example) can then steer
# instances to these hosts.
resource "pcd_compute_aggregate" "ssd" {
  name  = "ssd-hosts"
  zone  = "cluster-1"
  hosts = [data.pcd_host.hyp1.id, data.pcd_host.hyp2.id]

  metadata = {
    ssd = "true"
  }
}
