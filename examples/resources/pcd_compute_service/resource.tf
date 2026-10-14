data "pcd_host" "hyp1" {
  name = "hyp1.example.com"
}

# Stop new instances from landing on this hypervisor (the PCD UI's Disable
# Scheduling). "maintenance" does the same with the reason the UI writes, so
# the UI shows the host in maintenance; destroying the resource enables
# scheduling again.
resource "pcd_compute_service" "hyp1" {
  host_id = data.pcd_host.hyp1.id
  status  = "disabled"
}
