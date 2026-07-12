resource "pcd_compute_servergroup" "example" {
  name     = "tf-example-servergroup"
  policies = ["anti-affinity"]
  region   = "RegionOne"
}
