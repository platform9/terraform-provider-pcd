data "pcd_host" "a" {
  for_each = toset(var.domain_a_hosts)
  name     = each.value
}

data "pcd_host" "b" {
  for_each = toset(var.domain_b_hosts)
  name     = each.value
}

# The two failure domains: host aggregates in the cluster's availability zone,
# tagged the way the PCD UI tags a stretched cluster. Aggregate names are
# unique in the region and cannot be a cluster's name.
resource "pcd_compute_aggregate" "domain_a" {
  name  = "${var.cluster_name}-domain-a"
  zone  = var.cluster_name
  hosts = [for h in data.pcd_host.a : h.id]

  metadata = {
    pf9_failure_domain = "true"
    pf9_fd_label       = "A"
  }
}

resource "pcd_compute_aggregate" "domain_b" {
  name  = "${var.cluster_name}-domain-b"
  zone  = var.cluster_name
  hosts = [for h in data.pcd_host.b : h.id]

  metadata = {
    pf9_failure_domain = "true"
    pf9_fd_label       = "B"
  }
}

# An instance that runs in domain A, and in domain B only when PCD reports A
# down. The value is "<primary aggregate ID>:<fallback aggregate ID>".
resource "pcd_compute_instance" "app" {
  name              = "app"
  image_name        = var.image_name
  flavor_name       = var.flavor_name
  availability_zone = var.cluster_name

  metadata = {
    pf9_failure_domain = "${pcd_compute_aggregate.domain_a.id}:${pcd_compute_aggregate.domain_b.id}"
  }

  network {
    uuid = var.network_id
  }
}
