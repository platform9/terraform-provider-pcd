resource "pcd_networking_qos_policy" "example" {
  name = "tf-example-qos"
}

resource "pcd_networking_qos_bandwidth_limit_rule" "example" {
  qos_policy_id  = pcd_networking_qos_policy.example.id
  max_kbps       = 3000
  max_burst_kbps = 300
  direction      = "egress"
}
