output "record_name" {
  description = "The name Neutron publishes for the instance; resolve it against the BIND server to confirm."
  value       = "${var.instance_name}.${var.zone_name}"
}

output "instance_ip" {
  value = pcd_compute_instance.app.access_ip_v4
}
