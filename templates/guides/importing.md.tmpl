---
page_title: "Importing existing resources - PCD Provider"
subcategory: ""
description: |-
  How to bring resources that already exist in PCD under Terraform management, where to find the IDs that import needs, and how to hand an instance moved to another project to that project's configuration.
---

# Importing existing resources

Most PCD environments already contain things Terraform should manage rather than
recreate: the region's single cluster blueprint, the volume type the image library
depends on, provider networks, images. `terraform import` brings such a resource
into Terraform state; from then on Terraform manages it in place.

The IDs import needs are assigned by PCD, so they are never something you typed
into a configuration. This guide covers the two ways to import and, for every
resource, where its ID comes from, and how to hand an instance that was moved
to another project over to that project's configuration.

## Set up the CLI

The lookups below use `pcdctl`, the PCD command-line tool. It wraps the OpenStack
CLI, so every command here is the OpenStack CLI command with the verb swapped. If
you already have the `openstack` client installed, it accepts the same subcommands
with the same RC file.

1. Install `pcdctl` on any machine that can reach PCD
   ([PCD CLI](https://docs.platform9.com/private-cloud-director/automation-and-cli/pcdctl-command-line)).
2. In the PCD UI, open **Settings > API Access** and copy the `pcdctl RC` file.
   Put your password into `OS_PASSWORD`, then source it:

   ```shell
   source pcdctlrc
   ```

3. On Community Edition, which ships a self-signed certificate, also set:

   ```shell
   export OS_INSECURE=true
   ```

   Without it every service command fails with a certificate verification error.

4. Check that it works:

   ```shell
   pcdctl volume type list
   ```

`pcdctl config set` stores the credentials that host onboarding commands use; it
does not authenticate the service commands above. The sourced RC file does.

## Two ways to import

### terraform import

Write the resource block first, look up the ID, import, and confirm the plan is
empty. The volume type below is the case that most often needs this: the blueprint
references it by name, but import wants its UUID.

```terraform
resource "pcd_blockstorage_volume_type" "nfs" {
  name        = "nfs"
  description = "NFS-backed persistent storage"
  is_public   = true
  extra_specs = { volume_backend_name = "nfs" }
}
```

```shell
pcdctl volume type show nfs -f value -c id
# 786a5f51-cf77-4b94-9e47-f42e5a1e5e27
terraform import pcd_blockstorage_volume_type.nfs 786a5f51-cf77-4b94-9e47-f42e5a1e5e27
terraform plan
```

The plan must report `No changes`. If it shows a difference, the block does not
match what PCD has: adjust the block, or accept the change on the next apply.
Importing by name does not work; the provider answers that no object exists with
that ID.

### Import blocks with generated configuration

With Terraform 1.5 or later you can let Terraform write the resource block for you.
This is the quickest way to learn the shape of a resource you have only ever
configured in the UI.

```terraform
import {
  to = pcd_blockstorage_volume_type.nfs
  id = "786a5f51-cf77-4b94-9e47-f42e5a1e5e27"
}

import {
  to = pcd_cluster_blueprint.region
  id = "region-1"
}
```

```shell
terraform plan -generate-config-out=generated.tf
```

Terraform writes one resource block per `import` block into `generated.tf`. Review
them, move them into your configuration, then `terraform apply` records the
imports in state. Sensitive attributes come out as `null # sensitive`; for the
blueprint that is exactly right, because a `pcd_cluster_blueprint` with
`storage_backends_json` unset keeps the backends PCD already has. Terraform marks
configuration generation as experimental, so review the output before you rely on
it.

## Finding IDs

Every table below lists the import ID the resource's page documents and the
command that prints it. Commands print a table; add `-f value -c ID` to print the
ID alone.

### Persistent storage

| Resource | Import ID | Where to find it |
|---|---|---|
| `pcd_blockstorage_volume_type` | `<volume_type_id>` | `pcdctl volume type list` |
| `pcd_blockstorage_volume_type_access` | `<volume_type_id>/<project_id>` | `pcdctl volume type list --private`; `pcdctl volume type show <volume_type_id>` (its `access_project_ids` lists the projects that hold access) |
| `pcd_blockstorage_volume` | `<id>` | `pcdctl volume list` |
| `pcd_blockstorage_snapshot` | `<snapshot_id>` | `pcdctl volume snapshot list` |
| `pcd_blockstorage_volume_backup` | `<backup_id>` | `pcdctl volume backup list` |
| `pcd_blockstorage_quotaset` | `<project_id>/<region>` | `pcdctl project list`; `pcdctl region list` |
| `pcd_blockstorage_volume_transfer` | `<transfer_id>` | `pcdctl volume transfer request list`; `auth_key` stays empty after an import |

### Compute

| Resource | Import ID | Where to find it |
|---|---|---|
| `pcd_compute_flavor` | `<id>` | `pcdctl flavor list` |
| `pcd_compute_flavor_access` | `<flavor_id>/<tenant_id>` | `pcdctl flavor list --private`; `pcdctl flavor show <flavor_id>` (its `access_project_ids` lists the projects that hold access) |
| `pcd_compute_instance` | `<id>` | `pcdctl server list` |
| `pcd_compute_instance_snapshot` | `<image_id>` | `pcdctl image list`; a snapshot's `instance_uuid` property names its instance |
| `pcd_compute_instance_rescue` | `<instance_id>` | `pcdctl server list --status RESCUE` (only an instance in rescue mode imports) |
| `pcd_compute_keypair` | `<name>` | `pcdctl keypair list` |
| `pcd_compute_servergroup` | `<id>` | `pcdctl server group list` |
| `pcd_compute_interface_attach` | `<instance_id>/<port_id>` | `pcdctl server list`; `pcdctl port list --server <instance_id>` |
| `pcd_compute_volume_attach` | `<instance_id>/<attachment_id>` | `pcdctl server list`; the attachment ID is the attached volume's ID from `pcdctl volume list` |
| `pcd_compute_quotaset` | `<project_id>/<region>`, or `<project_id>/<region>/<user_id>` for one user's quotas | `pcdctl project list`; `pcdctl region list`; `pcdctl user list` |
| `pcd_compute_lease_policy` | `<project_id>` | `pcdctl project list` |
| `pcd_compute_instance_lease` | `<project_id>/<instance_id>` | `pcdctl server show <instance> -c project_id -c id` |
| `pcd_compute_aggregate` | `<id>` (an integer) | `pcdctl aggregate list`; the aggregates PCD keeps for its clusters cannot be imported. For an aggregate with a zone, set `zone` to it in the configuration first, or the plan after the import replaces the aggregate |
| `pcd_compute_service` | `<host_id>` (the hypervisor's resmgr UUID) | `pcdctl compute service list --service nova-compute -c Host` |

### Networking

| Resource | Import ID | Where to find it |
|---|---|---|
| `pcd_networking_network` | `<id>` | `pcdctl network list` |
| `pcd_networking_subnet` | `<id>` | `pcdctl subnet list` |
| `pcd_networking_port` | `<id>` | `pcdctl port list` |
| `pcd_networking_router` | `<id>` | `pcdctl router list` |
| `pcd_networking_router_interface` | `<id>` (the interface port) | `pcdctl port list --router <router_id>` |
| `pcd_networking_router_route` | `<router_id>/<destination_cidr>/<next_hop>` | `pcdctl router show <router_id> -c routes` |
| `pcd_networking_subnet_route` | `<subnet_id>/<destination_cidr>/<next_hop>` | `pcdctl subnet show <subnet_id> -c host_routes` |
| `pcd_networking_secgroup` | `<id>` | `pcdctl security group list` |
| `pcd_networking_secgroup_rule` | `<id>` | `pcdctl security group rule list <security_group>` |
| `pcd_networking_floatingip` | `<id>` | `pcdctl floating ip list` |
| `pcd_networking_floatingip_associate` | `<floating_ip_id>` | `pcdctl floating ip list` |
| `pcd_networking_port_secgroup_associate` | `<port_id>` | `pcdctl port list` |
| `pcd_networking_qos_policy` | `<qos_policy_id>` | `pcdctl network qos policy list` |
| `pcd_networking_qos_bandwidth_limit_rule`, `..._dscp_marking_rule`, `..._minimum_bandwidth_rule` | `<qos_policy_id>/<rule_id>` | `pcdctl network qos rule list <qos_policy_id>` |
| `pcd_networking_quota` | `<project_id>/<region>` | `pcdctl project list`; `pcdctl region list` |

### Images

| Resource | Import ID | Where to find it |
|---|---|---|
| `pcd_images_image` | `<id>` | `pcdctl image list` |
| `pcd_images_image_access` | `<image_id>/<member_id>` | `pcdctl image list`; `pcdctl image member list <image_id>` |
| `pcd_images_image_access_accept` | `<image_id>/<member_id>`, or `<image_id>` for the image's only visible member, otherwise the provider's project | `pcdctl image list --member-status pending` (`--member-status all` for any status) |

### Identity

| Resource | Import ID | Where to find it |
|---|---|---|
| `pcd_identity_domain` | `<domain_id>` | `pcdctl domain list` |
| `pcd_identity_project` | `<id>` | `pcdctl project list` |
| `pcd_identity_user` | `<id>` | `pcdctl user list` |
| `pcd_identity_role` | `<id>` | `pcdctl role list` |
| `pcd_identity_group` | `<group_id>` | `pcdctl group list` |
| `pcd_identity_group_membership` | `<group_id>/<user_id>` | `pcdctl group list`; `pcdctl user list --group <group_id>` |
| `pcd_identity_role_assignment` | `<domain_id>/<project_id>/<group_id>/<user_id>/<role_id>` | `pcdctl role assignment list --names`, then resolve each name with the list commands above; leave the parts the assignment does not use empty |
| `pcd_identity_inherit_role_assignment` | `<domain_id>/<project_id>/<group_id>/<user_id>/<role_id>` | `pcdctl role assignment list --names --inherited`, then resolve each name as for `pcd_identity_role_assignment` |
| `pcd_identity_user_mfa` | `<user_id>` | `pcdctl user list` |
| `pcd_identity_application_credential` | `<id>` | `pcdctl application credential list` |
| `pcd_identity_federation_mapping` | `<name>` | `pcdctl mapping list` |
| `pcd_identity_federation_identity_provider` | `<name>` | `pcdctl identity provider list` |
| `pcd_identity_federation_protocol` | `<identity_provider>/<name>` | `pcdctl federation protocol list --identity-provider <identity_provider>` |
| `pcd_identity_sso_configuration` | `<identity_provider>` | the identity provider's name: `IDP1` for the default domain; `pcdctl identity provider list` |

### Load balancer

| Resource | Import ID | Where to find it |
|---|---|---|
| `pcd_lb_loadbalancer` | `<id>` | `pcdctl loadbalancer list` |
| `pcd_lb_listener` | `<id>` | `pcdctl loadbalancer listener list` |
| `pcd_lb_pool` | `<id>` | `pcdctl loadbalancer pool list` |
| `pcd_lb_member` | `<pool_id>/<member_id>` | `pcdctl loadbalancer member list <pool_id>` |
| `pcd_lb_monitor` | `<id>` | `pcdctl loadbalancer healthmonitor list` |

### DNS

| Resource | Import ID | Where to find it |
|---|---|---|
| `pcd_dns_zone` | `<id>` | `pcdctl zone list` |
| `pcd_dns_recordset` | `<zone_id>/<recordset_id>` | `pcdctl recordset list <zone_id>` |

### Key manager

| Resource | Import ID | Where to find it |
|---|---|---|
| `pcd_keymanager_secret` | `<id>` | `pcdctl secret list`; the ID is the UUID at the end of the secret href |
| `pcd_keymanager_container` | `<id>` | `pcdctl secret container list`; the ID is the UUID at the end of the container href |

### PCD-native resources

The cluster blueprint, clusters, host configurations, and host roles live in PCD's
resource manager rather than in an OpenStack service, so `pcdctl` has no list
command for most of them. Two lookups cover them all.

**The host UUID.** `pcd_host_config_assignment`, `pcd_host_cluster_role`, and
`pcd_host_role` take the host's resource-manager UUID. In a configuration, the
`pcd_host` data source resolves it from the hostname the host reports
(`data.pcd_host.hyp1.id`), so it never has to be typed; an import ID, however, is
literal. It is not the ID that `pcdctl hypervisor list` prints (that is the Compute
service's own hypervisor ID). Three ways to find it:

- On a host that already has the `hypervisor` role, the `service_host` field is the
  UUID, and the Compute service lists every hypervisor by it:

  ```shell
  pcdctl hypervisor show <hypervisor-id> -c service_host
  pcdctl compute service list --service nova-compute -c Host -c Zone
  ```

- On any authorized host, with or without roles, the host agent records it:

  ```shell
  cat /etc/pf9/host_id.conf
  # [hostagent]
  # host_id = 136fc11a-ec5a-4699-b097-f75796134f8d
  ```

- The resource manager lists every host with its hostname and roles
  (`/resmgr/v1/hosts` below).

**Host configurations, blueprints, and clusters.** Read them from the resource
manager with a token from `pcdctl`. Drop `-k` when PCD has a trusted certificate.

```shell
PCD=https://pcd.example.com
TOKEN=$(pcdctl token issue -f value -c id)
curl -sk -H "X-Auth-Token: $TOKEN" "$PCD/resmgr/v2/hostconfigs" | python3 -m json.tool  # "id" and "name"
curl -sk -H "X-Auth-Token: $TOKEN" "$PCD/resmgr/v2/blueprint"   | python3 -m json.tool  # "name"
curl -sk -H "X-Auth-Token: $TOKEN" "$PCD/resmgr/v2/clusters"    | python3 -m json.tool  # "name"
curl -sk -H "X-Auth-Token: $TOKEN" "$PCD/resmgr/v1/hosts"       | python3 -m json.tool  # "id", "roles"
```

| Resource | Import ID | Where to find it |
|---|---|---|
| `pcd_cluster_blueprint` | `<blueprint_name>` | `/resmgr/v2/blueprint` |
| `pcd_cluster` | `<cluster_name>` | `/resmgr/v2/clusters`, or `pcdctl aggregate list` |
| `pcd_host_config` | `<host_config_id>` | `/resmgr/v2/hostconfigs` |
| `pcd_host_config_assignment` | `<host_id>/<host_config_id>` | the host UUID and `/resmgr/v2/hostconfigs` |
| `pcd_host_cluster_role` | `<host_id>/<role>` | the host UUID; the role is `hypervisor`, `image-library`, `persistent-storage`, or `dns` |
| `pcd_host_role` | `<host_id>/<role_name>` | the host UUID; `roles` in `/resmgr/v1/hosts` lists the granular `pf9-*` names |

## Worked example: the region's cluster blueprint

PCD keeps one cluster blueprint per region, so a region that already runs has one
to import rather than create. The import ID is the blueprint's name.

```terraform
import {
  to = pcd_cluster_blueprint.region
  id = "region-1"
}
```

```shell
terraform plan -generate-config-out=generated.tf
```

The generated block looks like this. `storage_backends_json` is `null` because it
is sensitive; leave it unset and the provider keeps the backends the region has.
`image_library_storage` is the name of a volume type, which you can import the
same way.

```terraform
resource "pcd_cluster_blueprint" "region" {
  dns_domain_name              = "pcd.local."
  image_library_shared_storage = true
  image_library_storage        = "nfs"
  instance_shared_storage      = false
  name                         = "region-1"
  storage_backends_json        = null # sensitive
  virtual_networking = {
    enabled       = true
    underlay_type = "vlan"
    vnid_range    = "1000:2000"
  }
  vm_storage = "/opt/data/instances"
}
```

Move the block into your configuration, remove the `import` block, and run
`terraform apply` to record the import; `terraform plan` is then empty. From here
on a change to, say, `dns_domain_name` is an in-place update.

Two things to keep in mind once the blueprint is in state. Destroying the resource
deletes the blueprint from PCD (since v0.1.10), so to stop managing it without
deleting it use `terraform state rm pcd_cluster_blueprint.region`. And a
`pcd_host_config` that references the blueprint through `cluster_name`, plus the
clusters and host roles built on it, must go before the blueprint does.

For a region built from nothing, see the
[Community Edition guide](community-edition.md).

## Moving an instance to another project

The `pcd_compute_instance_change_tenant` action moves an instance, with its
ports and attached volumes, to another project: the PCD UI's **Change Tenant**.
The instance keeps its ID and addresses. A configuration describes the objects
of the project it runs in, so an instance that one configuration manages is
handed over to the target project's configuration in three steps.

1. Move it, with an admin token and Terraform 1.14 or later. Declare the action
   in the source configuration and run it once:

   ```terraform
   terraform {
     required_version = ">= 1.14"
   }

   action "pcd_compute_instance_change_tenant" "web" {
     config {
       instance_id = pcd_compute_instance.web.id
       project_id  = "<target project ID>"
     }
   }
   ```

   ```shell
   terraform apply -invoke=action.pcd_compute_instance_change_tenant.web
   ```

2. In the source configuration, replace the resource blocks of what moved with
   `removed` blocks, so Terraform forgets them without destroying them: the
   instance, its ports, its attached volumes, and any
   `pcd_compute_volume_attach` or `pcd_compute_interface_attach`. Resources that
   refer to the instance but stay in the source project (a load balancer member,
   a DNS record, an instance snapshot, a `pcd_compute_instance_lease`, a
   floating IP) keep their blocks: replace each reference to a removed resource
   with a literal value, or with a `pcd_compute_instance` data source read under
   an admin token. Delete the step 1 `action` block, and any other block that
   still refers to the removed ones (an output, for example): Terraform refuses
   a reference to a resource the configuration no longer declares. Then apply:

   ```terraform
   removed {
     from = pcd_compute_instance.web

     lifecycle {
       destroy = false
     }
   }
   ```

3. In the target project's configuration, import what moved by ID, generate its
   configuration, and apply. The tables under [Finding IDs](#finding-ids) give
   each resource's import ID:

   ```terraform
   import {
     to = pcd_compute_instance.web
     id = "<instance ID>"
   }
   ```

   ```shell
   terraform plan -generate-config-out=generated.tf
   ```

Nova checks everything before it moves anything, and the action fails with
"Instance not moved" and Nova's message when a check does not pass. The
instance must be active, shut off, paused or suspended, with no task in
progress, and not locked. Then:

- The target project exists.
- The instance's owner may create instances in the target project; a user
  with the `member` or `admin` role there normally may.
- The instance is in no server group and has no floating IP. Disassociate the
  floating IP first; floating IPs do not move.
- Every security group on the instance's ports belongs to the target project.
  Security groups do not move: create matching groups in the target project and
  put them on the ports before the move. That needs an admin token, because
  Neutron accepts another project's security group on a port only from an
  admin, or when the group is shared with the port's project.
- The target project can use the instance's image (public, community, its own,
  or shared with it and accepted) and flavor (public, or private with access
  for the target project). An instance that boots from a volume skips the image
  check.
- Every network the instance is on is shared, the target project's own, or
  shared with it through an RBAC policy.
- No attached volume has snapshots, is encrypted, or is migrating, every one is
  `available` or `in-use`, and the target project can use each volume's type.
- The target project has instance quota left for the instance's owner.

When the instance is already in the target project, the action warns, names
the instance's status, and sends nothing.

Prepare for what does not move or changes with the move:

- Key pairs belong to the user, not the project, and do not move; the imported
  instance reads `key_pair` as null.
- A volume that is also attached to another instance moves with this one, and
  the other instance's project can then no longer manage it. Detach it from the
  other instance first.
- The lease service applies the target project's lease policy to the instance,
  counted from the instance's creation: an instance older than the policy's
  duration is deleted or powered off on the service's next pass, within about a
  minute of the move. Check the target project's `pcd_compute_lease_policy`
  before the move, and lengthen it first if the instance must not be acted on.
- The instance's own lease (`pcd_compute_instance_lease`) stays with the source
  project: it does not exempt the instance from the target project's policy,
  and when it runs out the lease service still powers off or deletes the
  instance. Its resource stays in the source configuration, because its ID
  names the source project. The lease service keeps one lease per instance, so
  destroy that lease before you give the instance a lease in the target
  project's configuration.

Nova moves the ports and volumes before it answers. If a step fails, it moves
them back, sets the instance's project back to the source project, and answers
with an error; when it cannot move every volume and port back, it also sets
the instance to `ERROR`. So after a refusal the action reads the instance once
more, and reports "Instance not moved" only when it still reports the source
project and is not in `ERROR`, or was already in `ERROR` before the action
(Nova refuses to move an instance in `ERROR` before it touches anything).

Otherwise, and when the request fails in any other way than one of those
refusals (a dropped connection, a gateway error such as a 502 or 504, or a
server error), the action fails with "Move outcome unknown": Nova may have
moved the instance, or part of it. If the request timed out, Nova may still be
working on the move, so wait a few minutes. Then read the instance's project
and status under an admin token, for example with the `pcd_compute_instance`
data source:

- In the target project, the move finished: go on with the handoff above.
- In the source project and not in `ERROR`, nothing moved: run the action
  again.
- In `ERROR`, Nova could not move every volume and port back, and it refuses
  another move while the instance is in `ERROR`: an administrator has to put
  each attached volume and port back in the instance's project and reset the
  instance's state before the action can run again.

When Nova accepts the move but the instance does not report the target project
within two minutes, reports `ERROR` before it does, or cannot be read, the
action fails with "Move not confirmed". Nova accepts the move only after it has
moved the instance: read the instance's project and status under an admin
token, and when it reports the target project, go on with the handoff. When it
reports another project or is in `ERROR`, something changed it after Nova
accepted this move, and the steps under "Move outcome unknown" above apply.

After the move, a configuration whose credentials are scoped to the source
project can no longer read the instance unless they carry the `admin` role:
its next refresh would find the instance gone and plan a new one, which is why
the source configuration forgets it with `removed`. A configuration that keeps
managing a moved instance with an admin token sees no change on the instance
itself, but a `pcd_networking_port` that sets `tenant_id` plans a replacement,
because Neutron now reports the new project. Later changes to the instance
count against the target project's quota, and a resize to a private flavor
needs flavor access for the target project.

To change the owner as well, mind the order: Nova checks the current owner in
the target project when it moves an instance, and the new owner in the
instance's project when it changes the owner. Change the owner first when the
new owner may create instances in the source project; move first when the
current owner may create instances in the target project.
