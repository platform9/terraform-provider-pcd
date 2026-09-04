variable "host_id" {
  type        = string
  description = "The resmgr UUID of the host to onboard. On the host: cat /etc/pf9/host_id.conf"
}

variable "host_interface" {
  type        = string
  description = "The host's network interface. This single-NIC layout puts every traffic type on it."
  default     = "enp1s0"
}

variable "nfs_export" {
  type        = string
  description = "The NFS export the Persistent Storage backend mounts, as <server-ip>:/exported/path."
}

variable "network_cidr" {
  type        = string
  description = "The CIDR of the network the host's interface sits on; instances get addresses on it (e.g. 192.168.1.0/24)."
}

variable "network_gateway" {
  type        = string
  description = "The gateway address on that network."
}

variable "allocation_pool_start" {
  type        = string
  description = "First address PCD may hand to an instance. Keep the pool clear of addresses other devices use."
}

variable "allocation_pool_end" {
  type        = string
  description = "Last address PCD may hand to an instance."
}

variable "dns_nameservers" {
  type        = list(string)
  description = "Nameservers instances receive over DHCP."
  default     = ["8.8.8.8"]
}

variable "blueprint_name" {
  type        = string
  description = "The name of the region's cluster blueprint. PCD keeps one per region."
  default     = "region-1"
}

variable "cluster_name" {
  type        = string
  description = "The name of the cluster the hypervisor joins."
  default     = "cluster-1"
}

variable "dns_domain_name" {
  type        = string
  description = "The internal DNS domain suffix for instances (must end with a dot)."
  default     = "pcd.local."
}
