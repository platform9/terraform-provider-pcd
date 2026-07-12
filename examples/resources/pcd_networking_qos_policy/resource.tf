resource "pcd_networking_qos_policy" "example" {
  name        = "tf-example-qos"
  description = "QoS policy managed by Terraform"
  shared      = true
  tags        = ["terraform", "qos"]
}
