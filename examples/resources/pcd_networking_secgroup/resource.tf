resource "pcd_networking_secgroup" "example" {
  name                 = "tf-example-secgroup"
  description          = "Managed by Terraform"
  stateful             = true
  delete_default_rules = false
  tags                 = ["env:dev", "team:platform"]
}
