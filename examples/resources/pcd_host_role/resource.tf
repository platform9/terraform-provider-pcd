# host_id is the resmgr host UUID of an onboarded host; pcd_host resolves it
# from the hostname the host reports.
data "pcd_host" "hyp1" {
  name = "hyp1.example.com"
}

resource "pcd_host_role" "example" {
  host_id   = data.pcd_host.hyp1.id
  role_name = "pf9-ostackhost-neutron"
}
