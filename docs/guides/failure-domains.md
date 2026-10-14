---
page_title: "Failure domains (stretched clusters) - PCD Provider"
subcategory: ""
description: |-
  Split a cluster's hypervisors into two failure domains with tagged host aggregates, and keep an instance in one domain with the other as its fallback.
---

# Failure domains (stretched clusters)

A failure domain is a group of a cluster's hypervisors that fail together, for
example the hosts in one site of a cluster that spans two. PCD splits a cluster
into two failure domains, A and B, and keeps an instance in the domain you
choose, moving it to the other one only when PCD reports the first one down.
The PCD UI calls such a cluster a stretched cluster.

There is no failure-domain object in PCD's API. Each domain is a Nova host
aggregate, and an instance's choice is a key in its metadata:

- Two aggregates per cluster, both in the cluster's availability zone (in PCD,
  the cluster's name), with the metadata `pf9_failure_domain = "true"` and
  `pf9_fd_label = "A"` or `"B"`.
- On the instance, the metadata key `pf9_failure_domain` set to
  `"<primary aggregate ID>:<fallback aggregate ID>"`, the aggregates' integer
  IDs. PCD's scheduler places the instance only on the primary aggregate's
  hosts, and on the fallback's once PCD reports the primary down.

So the provider builds failure domains from two resources it already has:
`pcd_compute_aggregate` for the domains and the `metadata` of
`pcd_compute_instance` for the placement. Managing the aggregates needs an admin
token; the instance metadata is set by whoever manages the instance.

## What the PCD UI checks and Terraform does not

The UI refuses a stretched cluster that breaks these rules. Terraform does not
know them, so keep them yourself:

- The cluster has VM high availability enabled
  (`vm_high_availability = { enabled = true }` on its `pcd_cluster`).
- No other host aggregate of yours holds the cluster's hosts. The aggregate PCD
  keeps for the cluster itself (named after the cluster) is fine.
- Every hypervisor of the cluster is in exactly one of the two domains, and
  each domain has at least one.
- Domains are not edited while PCD is failing instances over between them, and a
  host that runs instances with failure-domain affinity is not moved to the other
  domain.
- A cluster gets one pair of domains.

The scheduler places instances by failure domain only where PCD's scheduler
runs its failure-domain filter. If you are not sure your region does, ask
Platform9 support before relying on the placement.

## The configuration

The files are in the provider repository under
[`examples/complete/failure-domains/`](https://github.com/platform9/terraform-provider-pcd/tree/main/examples/complete/failure-domains).
`variables.tf` takes the cluster's name, the hostnames in each domain, and what
the example instance needs.

```terraform
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
```

The provider reads its credentials from the environment: source the RC file
the PCD UI offers under Settings > API Access.

```terraform
terraform {
  required_providers {
    pcd = {
      source  = "platform9/pcd"
      version = "~> 0.1.20"
    }
  }
}

# Credentials come from the pcdctl RC file: in the PCD UI open Settings > API
# Access, copy it, set OS_PASSWORD, and `source pcdctlrc` before running
# Terraform. Failure domains are host aggregates, which need an admin user.
provider "pcd" {
  user_domain_id    = "default"
  project_domain_id = "default"

  # For a PCD with a self-signed certificate; remove it when the certificate
  # is trusted.
  insecure = true
}
```

`main.tf` resolves the hosts, creates the two domains, and boots an instance
that runs in domain A with domain B as its fallback.

```terraform
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
```

Aggregate names are unique in the region and share their namespace with
clusters, because PCD names each cluster's own aggregate after the cluster, so
neither domain can be named after a cluster.

## Adding hosts

`hosts` on `pcd_compute_aggregate` is authoritative, so a host joins a domain by
being added to that domain's list. The PCD UI asks for the domain when it adds
a host to a stretched cluster; in Terraform, add the host to `domain_a_hosts` or
`domain_b_hosts`. Because each domain's host list lives in one resource, a
configuration that onboards each host in its own module cannot add the host to
a domain from that module; keep the domain lists in one place.

Nova knows a host only once its hypervisor role has converged, and adding a
host to an aggregate waits up to five minutes for that. When the hypervisor
role is assigned in the same configuration, set `wait_until_converged = true`
on its `pcd_host_cluster_role` and reference `pcd_host_cluster_role.<name>.host_id`
in `hosts`, so the aggregate waits for the role.

Moving a host from one domain to the other changes both aggregates in one
apply, in no fixed order, so the host is briefly in both domains or in
neither. Both are valid for Nova, because the two aggregates share the
cluster's zone.

## Changing or removing an instance's domain

Changing `pf9_failure_domain` on a running instance updates the scheduler's
record of it in place; the instance moves only the next time it is scheduled,
for example by a migration or an evacuation. Removing the key from `metadata`
removes the record. Remove the key by keeping `metadata` set without it, for
example `metadata = {}` when it was the only key. Deleting the `metadata`
argument leaves the instance's metadata unmanaged, and the key stays. (The PCD
UI's Remove Stretched Cluster rewrites the whole metadata instead, which leaves
the old record in place.)

## Removing failure domains

Remove the instances' `pf9_failure_domain` keys, or destroy the instances,
before destroying the aggregates. An instance whose metadata names deleted
aggregates cannot be scheduled again, so its next migration or evacuation
fails with `NoValidHost`. Within one configuration Terraform orders this
correctly, because the instance's metadata references the aggregates' IDs: on
`terraform destroy` the instance goes first. To remove only the domains and
keep the instances, take the key out of their `metadata` and apply before
removing the aggregates. Keep `metadata` set without the key, for example
`metadata = {}` when it was the only key: deleting the `metadata` argument
leaves the instance's metadata unmanaged, and the key stays.

The PCD UI lists failure-domain aggregates on its Stretched Clusters page,
not with the other host aggregates.
