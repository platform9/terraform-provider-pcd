variable "dns_host_name" {
  type        = string
  description = "The hostname the host that takes the dns role reports to PCD (hostname -f on the host, or the Hosts page in the UI). pcd_host resolves its UUID."
}

variable "dns_host_ip" {
  type        = string
  description = "The DNS host's address as the BIND server and Terraform reach it. designate-mdns listens here on 5354. Keep it at most 32 characters long (see the guide on IPv6)."
}

variable "dns_host_ssh_user" {
  type        = string
  description = "The user Terraform connects to the DNS host as; it needs passwordless sudo."
  default     = "ubuntu"
}

variable "dns_host_ssh_key" {
  type        = string
  description = "Path to the private key for dns_host_ssh_user."
}

variable "dns_host_ssh_home" {
  type        = string
  description = "The home directory of dns_host_ssh_user on the DNS host. pools.yaml is staged in a private .pcd-dns directory there before it is installed. Defaults to /home/<dns_host_ssh_user>; set it when the user's home is elsewhere, /root for root for example."
  default     = null
}

variable "bind_port" {
  type        = number
  description = "The port BIND listens on. 53 unless something else already owns it on the host, as dnsmasq, installed by pcdctl prep-node, does on a PCD host."
  default     = 53
}

variable "rndc_key_file" {
  type        = string
  description = "Path, on the DNS host, of the rndc key the Designate worker can read."
  default     = "/etc/designate/rndc.key"
}

# PCD packages Designate in its own virtualenv with its own configuration
# file and runs it as the pf9 user. These defaults are what a 2026.4 host has;
# the DNS guide says how to confirm them.
variable "designate_manage" {
  type        = string
  description = "Path, on the DNS host, of designate-manage. The default is the pf9-designate package's wrapper, which sets the library paths the virtualenv's binary needs; the virtualenv's binary cannot run on its own."
  default     = "/usr/sbin/designate-manage"
}

variable "designate_conf" {
  type        = string
  description = "Path, on the DNS host, of Designate's configuration file (designate-manage needs it to reach the database)."
  default     = "/opt/pf9/etc/pf9-designate/designate.conf"
}

variable "designate_user" {
  type        = string
  description = "The user Designate's services run as on the DNS host; pools.yaml is owned by it and designate-manage runs as it."
  default     = "pf9"
}

variable "ns_hostname" {
  type        = string
  description = "The NS record advertised for every zone in the pool, ending in a dot. Point it at the BIND server from wherever the zone is resolved."
  default     = "ns1.pcd.example.com."
}

variable "zone_name" {
  type        = string
  description = "The zone instances publish into, ending in a dot."
  default     = "app.pcd.example.com."
}

variable "zone_email" {
  type        = string
  description = "The SOA contact for the zone."
  default     = "dns-admin@pcd.example.com"
}

variable "network_cidr" {
  type        = string
  description = "The CIDR of the tenant subnet the instance boots on."
  default     = "10.90.0.0/24"
}

variable "image_name" {
  type        = string
  description = "An image already in the image library."
  default     = "cirros"
}

variable "flavor_name" {
  type        = string
  description = "A flavor that exists in the region."
  default     = "small"
}

variable "instance_name" {
  type        = string
  description = "The instance name; it becomes the hostname part of the DNS record. It must be a lowercase DNS label: Nova derives the record's hostname from the name by sanitizing it, so any other name publishes under a different name from the one the record_name output prints."
  default     = "dns-demo"

  validation {
    condition     = can(regex("^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$", var.instance_name))
    error_message = "The instance_name value must be a lowercase DNS label: 1 to 63 lowercase letters, digits and hyphens, not starting or ending with a hyphen. Nova lowercases any other name, turns spaces, underscores and dots into hyphens and drops other characters to make the record's hostname, so the record_name output would not match the published record."
  }
}
