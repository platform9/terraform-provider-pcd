# PCD-9784 — import-ID lookups and a working Community Edition example (docs design)

Date: 2026-09-03. Ticket: [PCD-9784](https://platform9.atlassian.net/browse/PCD-9784).
Status: approved in conversation; execution follows the plan in `docs/superpowers/plans/`.

## Problem

The registry docs tell users to `terraform import pcd_blockstorage_volume_type.example
<volume_type_id>` without saying where that UUID comes from, and the cluster-blueprint docs
do not walk a reader from a volume type to a blueprint to a converged host. Users end up
building the region in the UI and importing it to discover the data shapes. The ticket's
acceptance criteria:

1. Everywhere an import uses a server-generated ID, show how to retrieve it with `pcdctl`,
   `airctl`, or the `openstack` CLI.
2. Publish a functional Terraform example that stands up a working PCD Community Edition
   environment on the registry docs.

Two doc surfaces are affected: the registry docs generated from this repo, and the
first-party page at docs.platform9.com (`reference/terraform-provider.md` in the GitBook
repo), which is edited by the pcd-doc-writer agent from a handoff file.

## Verified facts (CE lab, 2026-09-03)

These were checked against a live Community Edition lab and are the ground truth for the
docs below.

- `terraform import pcd_blockstorage_volume_type.x <name>` fails with "no object exists";
  the import ID is the UUID. `pcdctl volume type list` and
  `pcdctl volume type show <name> -f value -c id` return it. A post-import `plan` is clean.
- `pcdctl` service subcommands mirror the OpenStack CLI exactly, but authenticate only from
  a sourced RC file (**Settings > API Access** in the UI). `pcdctl config set` alone is not
  enough. On Community Edition the RC also needs `OS_INSECURE=true` (self-signed cert).
- The resmgr host UUID used by `pcd_host_*` is *not* the ID in `pcdctl hypervisor list`.
  It is `service_host` in `pcdctl hypervisor show <hypervisor-id>`, the `Host` column of
  `pcdctl compute service list --service nova-compute`, and `host_id` in
  `/etc/pf9/host_id.conf` on the host. A host with no roles yet only has the last two.
- Host configuration IDs, blueprint names, and cluster names are read from the resmgr API
  (`/resmgr/v2/hostconfigs`, `/resmgr/v2/blueprint`, `/resmgr/v2/clusters`) with a token
  from `pcdctl token issue -f value -c id`. Cluster names also appear in
  `pcdctl aggregate list`.
- Terraform 1.5+ `import {}` blocks with `terraform plan -generate-config-out=<file>` work
  for the volume type, blueprint, cluster, and host config. The sensitive
  `storage_backends_json` is written as `null # sensitive`, so generated HCL never carries
  backend credentials, and leaving it unset on an imported blueprint preserves the
  backends.
- In the live blueprint, `imageLibraryStorage` holds the volume type's *name*. The lab's
  Synology backend uses the full driver class path
  `cinder.volume.drivers.synology.synology_iscsi.SynoISCSIDriver`.
- Corrected during execution (the lab run used differing key names): the top-level key of
  `storage_backends_json` becomes `volume_backend_name` on the host, so it is what a
  volume type's `volume_backend_name` must equal; the second-level key is the driver
  configuration name, which `pcd_host_cluster_role.backends` lists and which becomes the
  Cinder backend section (`<host-uuid>@<config>`). The provider's `backends` description
  said "top-level keys"; it was wrong and is fixed in this change. Boolean driver options
  must be JSON booleans: a quoted `"true"` never validates on the host, which then
  converges forever while resmgr refuses every role change.

## Decisions

| Question | Decision |
|---|---|
| CLI verb in the registry docs | `pcdctl` everywhere (house style), with one note that the `openstack` CLI accepts the same subcommands. |
| Lab validation of the CE example | Full run: empty the region, export NFS from the hypervisor VM itself, apply the example end to end with an NFS backend, verify a VM with an attached volume, destroy. Use distinct backend and configuration names so the docs can state which key `volume_backend_name` must match. |
| Release | Docs-only `v0.1.11` after the PR merges, so the registry renders the new guides before the first-party page links to them. |
| Adjacent fixes | Included: the stale native-resource table in `README.md`; on the first-party page, the blueprint import ID (a name, not an ID), the empty blueprint block (`name` is required), the resource table, and the pcdctl configuration claim. |

## Deliverable 1 — registry docs (this repo)

### `docs/guides/importing.md` (new, from `templates/guides/importing.md.tmpl`)

"Importing existing resources". Sections:

1. **Why import**: PCD keeps one blueprint per region; most environments already have
   volume types, networks, and images.
2. **Set up the CLI**: `pcdctl` install pointer, RC file from **Settings > API Access**,
   `OS_INSECURE=true` on Community Edition, and the note that `openstack` works the same.
3. **Two ways to import**: `terraform import` (one resource, needs the HCL written first)
   and `import {}` blocks with `-generate-config-out` (Terraform writes the HCL; sensitive
   attributes come out as `null`, which is what you want for the blueprint).
4. **Finding IDs**: one table per service family: resource, import ID format, lookup
   command. PCD-native resources get their own subsection covering the host UUID, host
   configuration IDs, and blueprint and cluster names.
5. **Worked examples**: the volume type (the ticket's case) and the blueprint, each ending
   in an empty plan.

### Per-resource `import.sh`

Every `examples/resources/*/import.sh` gains comment lines above the command naming the
lookup, in `pcdctl` form, for each placeholder in the import ID. The generated Import
section on every registry page then answers "where does this ID come from". Composite IDs
get one line per part.

### `templates/resources.md.tmpl`

One sentence under **Import** linking to the importing guide by its registry URL.

### Resource descriptions and examples

- `pcd_blockstorage_volume_type`: say that `extra_specs.volume_backend_name` must match the
  backend name in the blueprint's `storage_backends_json`, and that the blueprint's
  `image_library_storage` names a volume type.
- `pcd_cluster_blueprint`: describe the `storage_backends_json` shape (top-level backend
  name, inner configuration name, `driver`, `config`) in the attribute description and the
  example, and point at the storage backend configuration examples on docs.platform9.com
  for driver-specific keys. The lab run decides the exact wording of which key
  `volume_backend_name` matches.

### `docs/guides/community-edition.md` (new) and `examples/complete/community-edition/`

"Zero to a running VM on Community Edition". The guide pulls its HCL from the example
directory with `tffile`, so the published text and the runnable files cannot drift. Files:

- `provider.tf` (provider from `OS_*` environment, `insecure = true` with the CE note)
- `variables.tf` (`host_id`, `host_interface`, `nfs_share`, names with defaults)
- `region.tf` (volume type, blueprint with the NFS backend, host config and assignment,
  cluster, three cluster roles with `wait_until_converged`)
- `platform.tf` (flat provider network and subnet, security group and rules, CirrOS image,
  flavor)
- `app.tf` (instance, volume, attachment) and `outputs.tf`
- `terraform.tfvars.example` and a short `README.md`

Guide sections: overview, prerequisites (CE installed, host prepared and authorized with
`pcdctl`, an NFS export, the host UUID lookup), the files in order with what each does and
the explicit `depends_on` reasons, apply and verify, destroy order and the blueprint-destroy
caveat, and "already have a region? import it" linking to the importing guide.

### Housekeeping

`README.md` native-resource table adds `pcd_cluster` and `pcd_host_cluster_role` and moves
VM HA and rebalancing to the cluster row. `CHANGELOG.md` gets a Documentation entry under
`[Unreleased]`. `make generate` and `terraform fmt -check examples/` must be clean.

## Deliverable 2 — first-party docs handoff

A Markdown file for the pcd-doc-writer agent, saved outside the repo next to the earlier
draft (`~/Documents/Claude Code Projects/Terraform Provider/`). It carries
`[version=2026.4]`, the ticket key, the verified commands verbatim, and per-section edits to
`reference/terraform-provider.md`:

1. **Create a Volume Type**: explain what the type is for, the `volume_backend_name`
   linkage, and that `image_library_storage` names the type.
2. **Define the Cluster Blueprint**: describe the `storage_backends_json` shape and link the
   storage backend configuration examples; keep the sensitive-state hint.
3. **Manage PCD-Native Infrastructure**: import by blueprint *name*; a valid resource block
   (`name` is required); import blocks with `-generate-config-out`; a new "Look Up IDs for
   Import" subsection with `pcdctl` commands for the common resources and the host UUID and
   host configuration ID lookups; links to the registry importing guide.
4. **Resource table**: add `pcd_cluster` and `pcd_host_cluster_role`; VM HA and
   rebalancing belong to `pcd_cluster`; mark `pcd_host_role` as the low-level API.
5. **Day 1 / Day 2**: link the registry Community Edition guide and `examples/complete/`.
6. Open questions for the writer: sync the identical `2026.8` copy; the pcdctl page's claim
   that `config set` suffices for service commands.

## Validation on the lab

1. Empty the region with the testsuite's own teardown of its `ts-*` resources.
2. Export `/srv/nfs/pcd` from the hypervisor VM to `172.16.122.0/24` (done, no change to
   the KVM host).
3. Run the example from a scratch directory with a dev build of the provider:
   `apply` with the NFS backend, confirm the roles converge, the VM boots, and the volume
   attaches; `destroy` and confirm the region is empty.
4. Record the backend-key finding and any driver-key corrections in the docs before the PR.

## Out of scope, noted for follow-up

- A `pcd_blockstorage_volume_type` data source would let configurations reference an
  existing type by name without any lookup. Code change; not part of this ticket.
- Back-porting the first-party edits to versions other than 2026.4 is the writer's open
  question, not decided here.
