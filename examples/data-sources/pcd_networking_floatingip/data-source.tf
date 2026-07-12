# Look up a floating IP by its address.
data "pcd_networking_floatingip" "example" {
  address = "203.0.113.42"
}
