output "instance_ip" {
  description = "The address the workload instance received on the provider network."
  value       = pcd_compute_instance.workload.access_ip_v4
}

output "instance_id" {
  value = pcd_compute_instance.workload.id
}

output "volume_id" {
  value = pcd_blockstorage_volume.data.id
}

output "host_config_id" {
  description = "The host configuration ID, which pcd_host_config imports by."
  value       = pcd_host_config.single_nic.id
}
