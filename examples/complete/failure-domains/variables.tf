variable "cluster_name" {
  type        = string
  description = "The cluster to split into two failure domains. Its availability zone has the same name."
}

variable "domain_a_hosts" {
  type        = list(string)
  description = "Hostnames (as the Hosts page shows them) of the cluster's hypervisors in failure domain A, for example the ones in the first site."
}

variable "domain_b_hosts" {
  type        = list(string)
  description = "Hostnames of the cluster's hypervisors in failure domain B. Together with domain_a_hosts this lists every hypervisor in the cluster, each once."
}

variable "image_name" {
  type        = string
  description = "The image the example instance boots from."
}

variable "flavor_name" {
  type        = string
  description = "The flavor of the example instance."
  default     = "m1.small"
}

variable "network_id" {
  type        = string
  description = "The network the example instance attaches to."
}
