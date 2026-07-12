resource "pcd_networking_qos_policy" "example" {
  name = "tf-example-qos"
}

resource "pcd_networking_qos_minimum_bandwidth_rule" "example" {
  qos_policy_id = pcd_networking_qos_policy.example.id
  min_kbps      = 1000
  direction     = "egress"
}
