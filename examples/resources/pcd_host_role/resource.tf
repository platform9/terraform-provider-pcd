# host_id is the resmgr host UUID of an onboarded host.
resource "pcd_host_role" "example" {
  host_id   = "04575315-80ce-4617-9b96-6611d00c9942"
  role_name = "pf9-ostackhost-neutron"
}
