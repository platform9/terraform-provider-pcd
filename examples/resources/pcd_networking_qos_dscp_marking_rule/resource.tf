resource "pcd_networking_qos_policy" "example" {
  name = "tf-example-qos"
}

resource "pcd_networking_qos_dscp_marking_rule" "example" {
  qos_policy_id = pcd_networking_qos_policy.example.id
  dscp_mark     = 26
}
