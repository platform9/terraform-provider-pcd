# DNS Integration Implementation Plan (PCD-9926, PCD-9943, PCD-9945, PCD-9946, PCD-9948, PCD-9972)

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A Terraform user of PCD can bind a network to a Designate zone, choose which subnets publish fixed IPs, render and validate the Designate pool file for the hosts that carry the `dns` role, and override `designate-mdns`'s listen address, with every attribute round-tripping through plan, apply, refresh and import; the three tickets that are PCD product defects get lab-verified evidence and a handoff.

**Architecture:** Two Neutron attributes ride on the existing `pcd_networking_network` and `pcd_networking_subnet` resources through gophercloud's `extensions/dns` option wrappers and the `dns_publish_fixed_ip` field gophercloud already carries on subnets. A new `pcd_dns_pools_config` data source turns typed pool attributes into the `pools.yaml` that `designate-manage pool update` reads, validated at plan time, because PCD exposes no API for pool targets (verified below). `pcd_host_cluster_role` gains a `settings` map for the `dns` role, written through the resource-manager v1 role API onto `pf9-designate` after the cluster role is assigned. A DNS guide and a runnable example tie the pieces together, an Opus adversarial panel reviews the branch, and a tiered CE lab test plan proves the behavior end to end.

**Tech Stack:** Go 1.25, terraform-plugin-framework v1.19.0, gophercloud/v2 v2.13.0 (`networking/v2/extensions/dns`, `networking/v2/subnets`), gopkg.in/yaml.v3 (already a direct dependency), terraform-plugin-testing v1.16.0, the sibling `pcd-tf-testsuite` loader for lab credentials, the CE lab on cannon-ubuntu (PCD 2026.4.2, Designate 18.0.1 / 2024.1, Neutron chart 2026.4.2-156).

---

## Triage: what each ticket needs and where the work lives

| Ticket | Ask | Provider work in this plan | What stays with the product (Task 9 drafts a handoff for 9946, 9948, 9972; "none" means nothing to hand off) |
| --- | --- | --- | --- |
| PCD-9926 | `dns_domain` on `pcd_networking_network`, default `""` removes the association | Task 1 (resource + data source + tests + example) | none |
| PCD-9945 | `dns_publish_fixed_ip` on `pcd_networking_subnet`, default `false`; UI checkbox | Task 2 (resource + data source + tests + example) | UI checkbox already ships in 2026.4.2 (verified in the UI bundle) |
| PCD-9943 | (a) a resource that templates the pool configuration and removes 154 lines of variable validation; (b) a resource that delivers it to the DNS hosts and diffs live against desired | Task 3 (`pcd_dns_pools_config` data source: typed schema, validation, YAML) and Task 4 (guide + example showing delivery and the `dns` role) | (b) needs a PCD API: Designate's pool API exposes name, description, attributes and `ns_records` only, never targets or nameservers, so no client can read the live target configuration back. Handoff asks for a `pf9-designate` role setting or an API that carries targets |
| PCD-9948 | IPv6 for `designate-mdns`: `bindv6only=0`, `listen = [::]:5354` by default, static IP on `br-tun` from the blueprint, a documented override path | Task 5 (`settings` on `pcd_host_cluster_role` for `dns`, which makes the documented override declarative) | defaults, sysctl and the `br-tun` address belong to the role package and blueprint |
| PCD-9946 | `zone_masters.host` is `VARCHAR(32)`; full-length IPv6 masters fail `designate-manage pool update` | No provider code. Task 3 warns at plan time about masters longer than 32 characters so the failure is explained before it happens | Designate schema migration in `pf9-designate`; upstream bug (unreported as of 2026-09-17) |
| PCD-9972 | Supported extension points for third-party ML2 drivers and Neutron notifications | No provider code (nothing in the provider's domain) | Documentation and chart changes; lab evidence attached in Task 9 |

---

## Decisions to confirm before execution

Each has a default the plan follows. Change the default and the affected task says what to alter.

1. **Product-side tickets (9946, 9948 product half, 9972).** Default: Task 9 drafts Jira comments with the lab evidence and recommended fixes; you post them. No provider code for 9972; none for 9946 beyond the data-source warning.
2. **`dns_domain` clears when omitted.** Default: `Optional + Computed + Default("")`, exactly what PCD-9926 asks for. Consequence: a network whose zone was set outside Terraform and whose configuration omits `dns_domain` plans a clearing update after upgrading the provider. The alternative (`Optional + Computed`, no default, clear with an explicit `dns_domain = ""`) is the upstream OpenStack provider's behavior; it is a one-line schema change in Task 1 and one changed acceptance step. The same choice applies to `dns_publish_fixed_ip` (default `false`).
3. **Where `settings` lives.** Default: on `pcd_host_cluster_role`, allowed only with `role = "dns"`, applied to the granular `pf9-designate` role. Alternative: a `settings` map on `pcd_host_role`; simpler API concept, but a `pcd_host_role` for `pf9-designate` next to the `dns` cluster role would remove the granular role on destroy while the cluster role still expects it.
4. **Delivery in the guide uses `loafoe/ssh`.** The guide and example use the community `ssh_resource` (v2.7.0) to copy `pools.yaml` to the DNS host and run `designate-manage pool update`, because no PCD API delivers pool targets. Alternative: document only the file and the command, and leave delivery to the reader.
5. **Long IPv6 masters warn, not error.** Default: a warning diagnostic naming PCD-9946. An error would block a user whose Designate is already fixed.
6. **PR split.** Default: three PRs on `pushkar/` branches, in order: `pushkar/networking-dns-attributes` (Tasks 1–2), `pushkar/dns-pools-config` (Tasks 3–4), `pushkar/dns-role-settings` (Task 5); one `## [Unreleased]` section accumulates, release after the third merges. The version is decided at release time (PR #53 on `pushkar/pcd-9799-9803` is still open and also targets the next release).

---

## Established facts (verified 2026-09-17; the implementer does not need to re-derive these)

**Provider code (this repository at `3c92a7b`):**
- `pcd_networking_network` reads through `networkExtended{networks.Network; external.NetworkExternalExt; portsecurity.PortSecurityExt}` and builds create options by wrapping `networks.CreateOpts` with extension option types (`internal/services/networking/network_resource.go`). Update wraps `networks.UpdateOpts` the same way. The data source reuses `networkExtended`.
- `pcd_networking_subnet` builds a plain `subnets.CreateOpts` / `subnets.UpdateOpts`; gophercloud v2.13.0 already carries `DNSPublishFixedIP *bool` on both and `DNSPublishFixedIP bool` on `subnets.Subnet` (`openstack/networking/v2/subnets/requests.go:130,211`, `results.go:90`).
- gophercloud's `openstack/networking/v2/extensions/dns` has `NetworkCreateOptsExt{DNSDomain string}` (sent only when non-empty), `NetworkUpdateOptsExt{DNSDomain *string}` (sent when non-nil, so `""` clears), and `NetworkDNSExt{DNSDomain string}` for reads. No subnet types exist there because the subnet field is in the base package.
- `pcd_host_cluster_role` PUTs `{}` for `dns` to `PUT /resmgr/v2/hosts/<id>/roles/dns`; its marker granular role is `pf9-designate` (`clusterRoleMarkers`). `pcd_host_role` PUTs `{}` to resmgr v1 and reads role membership only. `getJSON`, `putJSON`, `isNotFound`, `hostRecord` live in `internal/services/resmgr/resmgr.go`.
- Validation precedent without a new dependency: `ResourceWithValidateConfig` on `host_cluster_role_resource.go` and `compute/instance_resource.go`. `terraform-plugin-framework-validators` is not a dependency and stays out. `stringdefault` is already used by `loadbalancer_resource.go`.
- `pcd_dns_zone` (`internal/services/dns/zone_resource.go`) is the model for a `dns` package file; `dns.go` holds `configureClient` and the wait helpers. `gopkg.in/yaml.v3` is imported by `internal/clients/clouds.go`.
- Unit-test style: `*_internal_test.go` in the resource's package, table cases, failure messages that say what a wrong answer does to a user (`resmgr/host_cluster_role_internal_test.go`). Acceptance tests use `acctest.PreCheck`, `acctest.LabConfig(t)`, `tf-acc-` names, `CheckDestroy` helpers in `networking/network_subnet_test.go` and `dns/dns_test.go`; resmgr mutations are opt-in via `PCD_ACC_RESMGR=1` and `PCD_ACC_HOST_ID`.
- Docs are generated: `make generate` renders `docs/` from schema descriptions plus `examples/resources/<name>/resource.tf`, `examples/data-sources/<name>/data-source.tf`, and `templates/guides/*.md.tmpl` (which embed example files with `{{ tffile "..." }}` / `{{ codefile "hcl" "..." }}`). CI runs build, vet, gofmt, unit tests, golangci-lint v2.12.2, a docs-generate smoke test, `goreleaser check`, and `terraform fmt -check -recursive ./examples`.

**The CE lab (management VM 172.16.122.253, Neutron chart `neutron-2026.4.2-156`, Designate chart `designate-2026.4.2-156`):**
- Rendered `ml2_conf.ini`: `extension_drivers = port_security,qos,dns_domain_keywords`, `mechanism_drivers = openvswitch,ovn`, `type_drivers = flat,vlan,local,geneve,vxlan`. `dns_domain_keywords` is a superset of `dns_domain_ports` and `dns`, so the `dns_domain` network attribute and `dns_publish_fixed_ip` are live on the lab's Neutron.
- Rendered `neutron.conf`: `external_dns_driver = designate`, a `[designate]` section, `dns_domain = pcd.local.` (the blueprint's `dns_domain_name`), and `[oslo_messaging_notifications] driver = noop`. The Helm values show `images.tags.neutron_server: quay.io/platform9/pf9-neutron:2026.4.2-1605` and `mechanism_drivers: null` (the chart computes `openvswitch,ovn`). This is PCD-9972's evidence.
- The installed Designate is `18.0.1.dev9` (2024.1 line). From the running `designate-api` pod: `zone_masters.host VARCHAR(32)`, `pool_target_masters.host VARCHAR(255)`, `pool_nameservers.host VARCHAR(255)`. Upstream master has the same widths; no Launchpad bug exists for it. This is PCD-9946's evidence.
- The Designate chart deploys API, central and producer on the management plane only (`deployment_mdns: false`); `designate-mdns` and `designate-worker` run on the host with the `dns` role, where `designate-manage` lives (`/opt/pf9/pf9-designate/...`). The chart carries a placeholder `pools.yaml` (pdns4 target on `${POWERDNS_SERVICE_HOST}`), which is why a fresh region shows a `default` pool with no usable nameservers and zone creation answers `500 no_servers_configured` until a real pool is applied (seen 2026-07-13).
- The 2026.4.2 UI bundle (`/ui/assets/index-B-pQNh0W.js`) sends `dns_domain` on network create/update and `dns_publish_fixed_ip` on subnet create/update; its "DNS Zone" dropdown sets `dns_domain` to the zone name; clearing the zone makes the UI itself PATCH every subnet to `dns_publish_fixed_ip: false` (client-side, not Neutron). Its DNS Pools table renders `backend` and `servers` as empty strings, and it wires `getPools`/`createPool`/`updatePool`/`deletePool` to Designate's `/v2/pools`, whose representation (upstream `PoolAPIv2Adapter`) has `id`, `name`, `description`, `attributes`, `ns_records`, `project_id`, timestamps; the POST/PATCH/DELETE handlers are marked deprecated ("unforeseen side affects when used with the designate-manage pool commands"). Targets and nameservers are not in the API.
- `designate-manage pool update --file F [--delete]` matches pools by `id` then by `name`, updates the pool, then rewrites every existing zone's masters from the pool targets (the step that fails on a 33+ character host), and with `--delete` removes pools absent from the file.
- The lab's standing region (blueprint `ce-region`, host config `hc-single-nic`, cluster `ce-cluster`, hypervisor / image-library / persistent-storage on hyp1 `136fc11a`, instance `workload-vm`) was applied 2026-09-04 from `~/Documents/PCD-CE/ce-run-2026-09-04/`; that configuration needs a build of `pushkar/pcd-9799-9803` (it uses `data "pcd_host"`, not on `main`). Re-check with `python3 run.py api hosts` before assuming it still stands.
- On 2026-09-17 the management VM had rebooted; every API call answered 503 because `keystone-api` stayed in `CreateContainerConfigError` (`failed to prepare subPath for volumeMount "http-wildcard-cert"`). Recreating the stuck pods is the fix (memory `ce-lab-after-cannon-reboot` has the one-liner); the auto-mode classifier refuses to let Claude run it, and another session ran it the same evening.

**Task 0's read-only probes, run 2026-09-17 after the lab recovered (`<scratchpad>/probe.out`):**
- Neutron extension aliases containing "dns": `dns-domain-ports`, `dns-integration`, `dns-integration-domain-keywords`, `subnet-dns-publish-fixed-ip`. Network responses carry `dns_domain` (`""` on `workload-net`, which is `router:external = true`); subnet responses carry `dns_publish_fixed_ip` (`false` on `workload-subnet`).
- `GET /designate/v2/pools`: one pool `default` (`794ccc2c-d751-44fe-b57f-8894c9f5c842`) with `attributes {}`, `description null`, `ns_records []`, `project_id null`; the representation has no `targets` or `nameservers` key. No zones exist. So PCD-9943(b) stays a file delivery, and the `pcd_dns_pool` branch of Task 0 does not apply.
- `GET /resmgr/v1/roles/pf9-designate`: `active_version 2026.4.2-847`, `default_settings {"debug": "True", "listen": "0.0.0.0:5354"}`; those two are the role's only customizable settings.
- A per-host role GET returns the bare settings object: `/roles/pf9-cindervolume-config` → `{"backends": {...}}`, `/roles/pf9-ostackhost-neutron` → a flat map of strings (plus a few lists); `/roles/pf9-designate` → 404 while the role is unassigned. Task 5's decode assumption holds.
- hyp1 (`136fc11a`) carries `hypervisor`, `persistent-storage`, `image-library` (granular roles all `applied`; aggregate `role_status` read `failed` for a while after the reboot, as the memory says). The standing region (blueprint `ce-region`, cluster `ce-cluster`, host config `hc-single-nic`) is intact. No `dns` role anywhere.
- On hyp1: PCD's own `dnsmasq` owns `172.16.122.251:53` (and `127.0.0.1:53`), so BIND9 must use 5353 as in the 2026-07-13 setup; `bind9` is not installed; `designate-manage` is absent until the `dns` role arrives; `net.ipv6.bindv6only = 0`; `br-tun` holds `172.16.122.251/24` and only a link-local IPv6 address, so the lab can prove the `[::]:5354` listener but not an IPv6 backend; user `pf9` (uid 1001, group `pf9group`) exists and PCD roles keep their config under `/opt/pf9/etc/pf9-<role>/`; `/etc/designate` does not exist; passwordless sudo works for `ubuntu`.
- `designate-manage` on a PCD host: the 2026-07-13 run used `sudo -u pf9 designate-manage --config-file /opt/pf9/etc/pf9-designate/designate.conf pool update --file /etc/designate/pools.yaml`, and PCD-9946's traceback places the binary in the `/opt/pf9/pf9-designate` virtualenv (`/opt/pf9/pf9-designate/bin/designate-manage`), which is outside sudo's `secure_path`. The example therefore parameterizes the binary, the config file and the user (Task 4), and tier 3 records the real paths once the role is on the host.
- Neutron's `dns_domain` rules (neutron-lib `validate_dns_domain`, `_validate_dns_format`, and the attribute's `convert_to = convert_string_to_case_insensitive`): the value is lower-cased before validation and stored lower-cased; it must end with a dot; each label is 1–63 characters of `[a-z0-9-]` with no leading or trailing hyphen; an all-numeric last label is rejected; and the whole string (with its dot) must be at most 253 characters (`max_len - 2`, "adding a sub-domain would exceed the FQDN limit").

**Neutron's publishing rules (upstream admin guide, "DNS integration with an external service"):** a port's fixed IPs are published under `<dns_name>.<dns_domain>` where `dns_domain` comes from the port or its network. On a network with `router:external = true`, nothing is published unless at least one subnet has `dns_publish_fixed_ip = true` (PCD-9945's finding). On other networks, records come from floating IP association, or from `dns_publish_fixed_ip` on the subnet, or, for provider networks that are not external, directly. Records are written when a port is created or updated, never retroactively, so the network and subnet attributes must be set before the instance boots. The zone named by `dns_domain` must already exist in Designate; Neutron logs and skips when it does not.

---

## Global Constraints

- Branches start with `pushkar/`: this worktree's branch `claude/pcd-jira-plan-testing-0e66ac` is renamed before anything is pushed (`git branch -m pushkar/dns-integration-plan`). Never `claude/`.
- No `Co-Authored-By` trailer and no "Generated with Claude Code" footer in commits or PR text.
- American English spelling in code comments, descriptions, changelog, commit messages and PR text.
- Simplest change that satisfies the ticket; touch only the files each task lists. Do not change `pcd_host_role`, `pcd_dns_zone`, or `pcd_networking_port`.
- Every commit passes `gofmt -l .` (empty), `go vet ./...`, `golangci-lint run ./...` (0 issues), `go test ./internal/... -timeout 120s`, and, after a schema or example change, `make generate` followed by `terraform fmt -check -recursive ./examples`.
- Minimum Terraform stays 1.0: no provider-defined functions, no features newer than protocol 6 basics.
- Lab rules: credentials only through the testsuite loader (`lab.Config()`, `lab_env.py`); never type the admin password. Ask the user before any `terraform apply` or `destroy` on the lab, and before assigning or removing the `dns` role on hyp1: the standing region and another session may be using it.
- Run all commands from the worktree root: `/Users/pushkarmulay/Documents/Claude Code Projects/Terraform Provider/terraform-provider-pcd/.claude/worktrees/pcd-9784-docs-plan-c0afcb`. The testsuite is at `/Users/pushkarmulay/Documents/Claude Code Projects/Terraform Provider/pcd-tf-testsuite`; `<scratchpad>` is the session scratchpad directory.
- Two rules the review panel added after reproducing the failures: (1) an acceptance `ExpectError` regex matches Terraform's wrapped stderr, so match the diagnostic *summary* (one line, never wrapped), not a phrase from the detail; (2) a zero-value `types.Map` / `types.List` / `types.Set` in a test model compiles but is not a usable null (`!!! MISSING TYPE !!!` on `State.Set`); always construct nulls with `types.MapNull(elemType)` and friends.
- Anything that runs Terraform against an example in this repository (`terraform validate`, the lab tiers) uses the dev build through `TF_CLI_CONFIG_FILE=<scratchpad>/dev.tfrc`; a bare `terraform init` installs the released provider, which lacks every attribute this plan adds.

---

## File Structure

- Modify `internal/services/networking/network_resource.go` — `dns_domain` attribute, `dns.NetworkDNSExt` in `networkExtended`, `networkCreateOpts` / `networkUpdateOpts` helpers (the existing create and update bodies moved out of the methods so they can be unit-tested), `ValidateConfig`, `invalidDNSDomain`.
- Modify `internal/services/networking/network_data_source.go` — `dns_domain` computed.
- Create `internal/services/networking/network_internal_test.go` — wire-body and validator tests, no lab.
- Modify `internal/services/networking/subnet_resource.go` — `dns_publish_fixed_ip` attribute, `subnetCreateOpts` / `subnetUpdateOpts` helpers.
- Modify `internal/services/networking/subnet_data_source.go` — `dns_publish_fixed_ip` computed.
- Create `internal/services/networking/subnet_internal_test.go`.
- Modify `internal/services/networking/network_subnet_test.go` — two acceptance tests.
- Modify `examples/resources/pcd_networking_network/resource.tf`, `examples/resources/pcd_networking_subnet/resource.tf`.
- Create `internal/services/dns/pools_config_data_source.go` — `pcd_dns_pools_config`: schema, decoding, `validatePools`, `renderPoolsYAML`.
- Create `internal/services/dns/pools_config_internal_test.go`.
- Create `examples/data-sources/pcd_dns_pools_config/data-source.tf`.
- Modify `internal/provider/provider.go` — register the data source.
- Create `templates/guides/dns.md.tmpl` and `examples/complete/dns/{README.md,provider.tf,variables.tf,terraform.tfvars.example,role.tf,pool.tf,zone.tf,network.tf,app.tf,outputs.tf}`.
- Modify `internal/services/resmgr/host_cluster_role_resource.go` — `settings` attribute, `settingsChanged`, `mergeSettings`, `readSettings`, `settingString`, `applySettings`, `waitRoleSettings`; Create/Read/Update wiring; `ValidateConfig` rule.
- Modify `internal/services/resmgr/host_cluster_role_internal_test.go` — tests for the helpers.
- Modify `internal/services/resmgr/resmgr_test.go` — opt-in acceptance test for `dns` + `settings`.
- Modify `examples/resources/pcd_host_cluster_role/resource.tf` — a `dns` example with `settings`.
- Modify `CHANGELOG.md`, `README.md` (one row), and the generated `docs/` (via `make generate`).
- Scratch (never committed): `<scratchpad>/probe.py`, `<scratchpad>/dns-run/`, `<scratchpad>/bin/`, `<scratchpad>/dev.tfrc`, `<scratchpad>/review.js`.

---

### Task 0: Recover the lab and run the read-only probes

**Status: done on 2026-09-17.** The answers are in "Established facts" under "Task 0's read-only probes"; every assumption Tasks 3 and 5 flagged held (no `targets` in the pool API, `listen`/`debug` the only `pf9-designate` settings, bare settings objects on per-host role GETs, all four DNS extension aliases present). Re-run Step 3 before execution starts if more than a few days have passed, and re-run Step 1 whenever the API answers 503. The one probe that could not run without the `dns` role (the Designate binary and config paths on the host) moved to Task 8 tier 3 step 5.

The probes settle four assumptions later tasks rest on. Nothing here mutates the lab except the Keystone pod recreation, which the user runs.

**Files:**
- Create (scratch): `<scratchpad>/probe.py`

- [ ] **Step 1: Confirm what is wrong with the lab and hand the fix to the user**

Run:
```bash
curl -sk -m 15 -o /dev/null -w 'ui %{http_code}\n' https://pcd.pf9.io/ ; curl -sk -m 15 -o /dev/null -w 'keystone %{http_code}\n' https://pcd.pf9.io/keystone/v3
ssh pk@cannon-ubuntu 'virsh -c qemu:///system list --all'
ssh pk@cannon-ubuntu 'ssh -i ~/.ssh/pcd_automation -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null ubuntu@172.16.122.253 "uptime; sudo -n kubectl -n pcd get pods --no-headers | grep -v -E \" Running | Completed \""'
```
Expected on a healthy lab: `ui 302`, `keystone 200`, both `pcd-ce-lab-*` domains `running`, no non-running pods. If the VMs are shut off, follow the memory `ce-lab-after-cannon-reboot` (start them with virsh, wait). If `keystone-api` shows `CreateContainerConfigError` and `kubectl describe pod` says `failed to prepare subPath for volumeMount "http-wildcard-cert"`, ask the user to run, from cannon:

```bash
ssh -i ~/.ssh/pcd_automation ubuntu@172.16.122.253 'sudo kubectl -n pcd delete pod -l application=keystone,component=api'
```
then poll `curl -sk -m 10 -o /dev/null -w '%{http_code}\n' https://pcd.pf9.io/keystone/v3` every 30 s until it answers 200. Pods stuck in `Unknown` (neutron-server, nova-*, glance-api, cinder-api, barbican-api, octavia-api) normally recover once Keystone serves; if any still is not `Running` five minutes later, report which ones rather than deleting more.

- [ ] **Step 2: Write the probe**

```python
#!/usr/bin/env python3
"""Read-only probes for the DNS work: prints what later tasks assume. Never mutates."""
import json
import sys

SUITE = "/Users/pushkarmulay/Documents/Claude Code Projects/Terraform Provider/pcd-tf-testsuite"
sys.path.insert(0, SUITE)
from lib import lab, api as apimod  # noqa: E402

api = lab.Config().api()
HYP1 = lab.Config().require("HYP1_ID")


def get(path):
    try:
        return api.get(path)
    except apimod.APIError as e:
        return {"HTTP": e.status, "body": e.body[:300]}


def section(title, value, limit=3500):
    print("=====", title)
    print(json.dumps(value, indent=1, sort_keys=True)[:limit])


ext = get("/neutron/v2.0/extensions")["extensions"]
section("neutron dns extension aliases", sorted(e["alias"] for e in ext if "dns" in e["alias"]))
section("networks: dns_domain and router:external", [
    {"id": n["id"], "name": n["name"], "dns_domain": n.get("dns_domain"), "external": n.get("router:external")}
    for n in get("/neutron/v2.0/networks")["networks"]])
section("subnets: dns_publish_fixed_ip", [
    {"id": s["id"], "name": s["name"], "dns_publish_fixed_ip": s.get("dns_publish_fixed_ip")}
    for s in get("/neutron/v2.0/subnets")["subnets"]])
section("designate pools (full representation)", get("/designate/v2/pools"))
section("designate zones", [{"id": z["id"], "name": z["name"], "status": z["status"], "pool_id": z["pool_id"]}
                             for z in get("/designate/v2/zones").get("zones", [])])
section("resmgr v1 role names", sorted(r.get("name", r) if isinstance(r, dict) else r for r in get("/resmgr/v1/roles")))
section("resmgr v1 role pf9-designate (definition, customizable settings)", get("/resmgr/v1/roles/pf9-designate"), 6000)
section("resmgr v2 hosts (roles)", [{"id": h["id"], "roles": h.get("roles"), "hostconfig_id": h.get("hostconfig_id")}
                                    for h in get("/resmgr/v2/hosts")])
v1 = get("/resmgr/v1/hosts/%s" % HYP1)
section("resmgr v1 hyp1 role_status", {"role_status": v1.get("role_status"), "roles": v1.get("roles"),
                                       "roles_status_details": v1.get("roles_status_details")})
section("resmgr v1 hyp1 pf9-designate settings (404 unless the dns role is assigned)",
        get("/resmgr/v1/hosts/%s/roles/pf9-designate" % HYP1))
section("resmgr v1 hyp1 pf9-cindervolume-config settings (shape of a per-host role GET)",
        get("/resmgr/v1/hosts/%s/roles/pf9-cindervolume-config" % HYP1))
section("blueprints / clusters / hostconfigs", {"blueprints": get("/resmgr/v2/blueprint"),
                                                "clusters": get("/resmgr/v2/clusters"),
                                                "hostconfigs": get("/resmgr/v2/hostconfigs")}, 5000)
```

- [ ] **Step 3: Run it and record the answers in this plan's execution notes**

Run: `python3 <scratchpad>/probe.py 2>&1 | tee <scratchpad>/probe.out`

Record, at the bottom of this document under "Execution notes":
1. The dns extension aliases (expect `dns-integration`, `dns-domain-ports`, `dns-integration-domain-keywords`, `subnet-dns-publish-fixed-ip`). If `subnet-dns-publish-fixed-ip` is missing, Task 2 still ships but its acceptance test cannot pass on this lab; say so.
2. Whether a pool's JSON carries `targets` or `nameservers`. Expected: no. If it does, PCD has extended the API and PCD-9943(b) becomes a `pcd_dns_pool` resource: stop after Task 2 and ask the user before designing it.
3. The `pf9-designate` role definition: the names, defaults and types of its customizable settings (look for `listen`, `debug`, anything pool-related). Task 5's description text names `listen` and `debug`; correct it to what the definition shows.
4. The shape of a per-host role GET (the `pf9-cindervolume-config` probe): a bare settings object, or wrapped. Task 5's `waitRoleSettings` decodes a bare `map[string]any`; adapt the decode if it is wrapped.
5. Which roles hyp1 carries, and whether the standing region is still there.

---

### Task 1: `dns_domain` on `pcd_networking_network` (PCD-9926)

**Files:**
- Modify: `internal/services/networking/network_resource.go`
- Modify: `internal/services/networking/network_data_source.go`
- Create: `internal/services/networking/network_internal_test.go`
- Modify: `internal/services/networking/network_subnet_test.go`
- Modify: `examples/resources/pcd_networking_network/resource.tf`

**Interfaces:**
- Produces: `func networkCreateOpts(ctx context.Context, plan *networkModel, diags *diag.Diagnostics) networks.CreateOptsBuilder` and `func networkUpdateOpts(plan, state *networkModel) networks.UpdateOptsBuilder` (package-private, used by `Create`, `Update` and the tests), `func invalidDNSDomain(s string) string` (empty when valid), and the `dns_domain` attribute on the resource and data source.

- [ ] **Step 1: Write the failing unit tests**

Create `internal/services/networking/network_internal_test.go`:

```go
// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package networking

import (
	"context"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

func dnsNetworkModel(dnsDomain types.String) *networkModel {
	return &networkModel{
		Name:         types.StringValue("net"),
		Description:  types.StringValue(""),
		AdminStateUp: types.BoolValue(true),
		Shared:       types.BoolNull(),
		External:     types.BoolNull(),
		PortSecurity: types.BoolNull(),
		Segments:     types.ListNull(types.ObjectType{}),
		Tags:         types.SetNull(types.StringType),
		DNSDomain:    dnsDomain,
	}
}

// networkCreateBody renders the wire body exactly as networks.Create would.
func networkCreateBody(t *testing.T, m *networkModel) map[string]any {
	t.Helper()
	var diags diag.Diagnostics
	opts := networkCreateOpts(context.Background(), m, &diags)
	if diags.HasError() {
		t.Fatalf("networkCreateOpts: %v", diags)
	}
	body, err := opts.ToNetworkCreateMap()
	if err != nil {
		t.Fatal(err)
	}
	return body["network"].(map[string]any)
}

func networkUpdateBody(t *testing.T, plan, state *networkModel) map[string]any {
	t.Helper()
	body, err := networkUpdateOpts(plan, state).ToNetworkUpdateMap()
	if err != nil {
		t.Fatal(err)
	}
	return body["network"].(map[string]any)
}

// dns_domain rides on the create body only when a zone is named: "" is the
// server default, and a Neutron without the dns extension rejects the key.
func TestNetworkCreateOptsDNSDomain(t *testing.T) {
	if got := networkCreateBody(t, dnsNetworkModel(types.StringValue("lab.example.com.")))["dns_domain"]; got != "lab.example.com." {
		t.Fatalf("dns_domain = %v; the zone association would not be created", got)
	}
	if _, sent := networkCreateBody(t, dnsNetworkModel(types.StringValue("")))["dns_domain"]; sent {
		t.Fatalf("dns_domain sent on create although empty")
	}
	if _, sent := networkCreateBody(t, dnsNetworkModel(types.StringNull()))["dns_domain"]; sent {
		t.Fatalf("dns_domain sent on create although null")
	}
	// The other extensions must still be wrapped underneath it. gophercloud's
	// external extension writes the *bool itself into the map, not its value.
	m := dnsNetworkModel(types.StringValue("lab.example.com."))
	m.External = types.BoolValue(true)
	ext, ok := networkCreateBody(t, m)["router:external"].(*bool)
	if !ok || ext == nil || !*ext {
		t.Fatalf("router:external = %v; wrapping dns_domain dropped the external extension", networkCreateBody(t, m)["router:external"])
	}
}

// An update sends dns_domain exactly when it changed, including the change
// to "" that removes the association (the ticket's stated way to detach).
func TestNetworkUpdateOptsDNSDomain(t *testing.T) {
	for _, tc := range []struct {
		name        string
		plan, state string
		wantSent    bool
	}{
		{name: "unchanged", plan: "a.example.com.", state: "a.example.com."},
		{name: "unchanged empty", plan: "", state: ""},
		{name: "set", plan: "a.example.com.", state: "", wantSent: true},
		{name: "changed", plan: "b.example.com.", state: "a.example.com.", wantSent: true},
		{name: "cleared", plan: "", state: "a.example.com.", wantSent: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := networkUpdateBody(t, dnsNetworkModel(types.StringValue(tc.plan)), dnsNetworkModel(types.StringValue(tc.state)))
			got, sent := body["dns_domain"]
			if sent != tc.wantSent {
				t.Fatalf("dns_domain sent = %v, want %v (body %v)", sent, tc.wantSent, body)
			}
			if sent && got != tc.plan {
				t.Fatalf("dns_domain = %v, want %q", got, tc.plan)
			}
		})
	}
	// A state written before the attribute existed reads back as null after
	// refresh fills it, but a null state must still be handled: send the value.
	if _, sent := networkUpdateBody(t, dnsNetworkModel(types.StringValue("a.example.com.")), dnsNetworkModel(types.StringNull()))["dns_domain"]; !sent {
		t.Fatalf("dns_domain not sent when state is null")
	}
}

// Neutron enforces these (neutron_lib validate_dns_domain, after lower-casing
// the value); catching them at plan time turns a 400 mid-apply, or a
// "inconsistent result after apply" on a mixed-case value, into a message that
// names the attribute.
func TestInvalidDNSDomain(t *testing.T) {
	label63 := strings.Repeat("a", 63)
	for _, tc := range []struct {
		in      string
		wantErr bool
	}{
		{in: ""},
		{in: "example.com."},
		{in: "sub.example.com."},
		{in: "a-b.example.com."},
		{in: label63 + ".example.com."},
		{in: "10.in-addr.arpa."},
		{in: strings.Repeat("a.", 120) + "example.com."}, // 252 characters with the dot
		{in: "example.com", wantErr: true},             // no trailing dot
		{in: ".", wantErr: true},                       // empty label
		{in: "a..example.com.", wantErr: true},         // empty label
		{in: "App.Example.Com.", wantErr: true},        // Neutron lower-cases; refuse rather than drift
		{in: "my_zone.example.com.", wantErr: true},    // underscore
		{in: "-bad.example.com.", wantErr: true},       // leading hyphen
		{in: "bad-.example.com.", wantErr: true},       // trailing hyphen
		{in: label63 + "a.example.com.", wantErr: true}, // 64-character label
		{in: "example.123.", wantErr: true},            // all-numeric TLD
		{in: strings.Repeat("a.", 121) + "example.com.", wantErr: true}, // 254 characters with the dot
	} {
		if msg := invalidDNSDomain(tc.in); (msg != "") != tc.wantErr {
			t.Errorf("invalidDNSDomain(%q) = %q, wantErr %v", tc.in, msg, tc.wantErr)
		}
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/services/networking/ -run 'TestNetworkCreateOptsDNSDomain|TestNetworkUpdateOptsDNSDomain|TestInvalidDNSDomain' -count=1`
Expected: build failure, `undefined: networkCreateOpts` (and the `DNSDomain` field).

- [ ] **Step 3: Implement the attribute**

In `internal/services/networking/network_resource.go`:

Add to the imports:
```go
	"regexp"
	"strings"

	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/extensions/dns"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringdefault"
```

Add the interface assertion next to the existing ones:
```go
	_ resource.ResourceWithValidateConfig = (*networkResource)(nil)
```

Add to `networkModel` after `PortSecurity`:
```go
	DNSDomain    types.String `tfsdk:"dns_domain"`
```

Add to `networkExtended`:
```go
	dns.NetworkDNSExt
```

Add to the schema's `Attributes`, after `port_security_enabled`:
```go
			"dns_domain": schema.StringAttribute{
				Optional: true, Computed: true, Default: stringdefault.StaticString(""),
				MarkdownDescription: "The Designate zone that ports on this network publish DNS records to, as a fully " +
					"qualified name ending in a dot (typically `pcd_dns_zone.example.name`), in lowercase: Neutron stores " +
					"the value lower-cased, so a mixed-case literal is refused at plan time. A network maps to at most one " +
					"zone, which must already exist. Defaults to `\"\"`, which is also how an association is removed: omit " +
					"the attribute and the next apply clears it, so add it to the configuration of any network whose zone " +
					"was set outside Terraform. Which fixed IPs get records depends on the subnets' `dns_publish_fixed_ip` " +
					"and on whether the network is external; the DNS guide explains the rules. Records are created when a " +
					"port is created, so set this before booting instances.",
			},
```

Replace the body of `Create` between the client construction and `networks.Create` with:
```go
	createOpts := networkCreateOpts(ctx, &plan, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}
```
and move the code it replaces into this function, placed directly after `segmentsCreateOptsExt.ToNetworkCreateMap`:

```go
// networkCreateOpts builds the create body: the base options, then each
// extension the plan uses wrapped around it. Kept apart from Create so the
// wire body can be unit-tested without a lab.
func networkCreateOpts(ctx context.Context, plan *networkModel, diags *diag.Diagnostics) networks.CreateOptsBuilder {
	adminUp := plan.AdminStateUp.ValueBool()
	base := networks.CreateOpts{
		Name:         plan.Name.ValueString(),
		Description:  plan.Description.ValueString(),
		AdminStateUp: &adminUp,
		TenantID:     plan.TenantID.ValueString(),
	}
	if !plan.Shared.IsNull() && !plan.Shared.IsUnknown() {
		shared := plan.Shared.ValueBool()
		base.Shared = &shared
	}

	var createOpts networks.CreateOptsBuilder = base
	if !plan.External.IsNull() && !plan.External.IsUnknown() {
		ext := plan.External.ValueBool()
		createOpts = external.CreateOptsExt{CreateOptsBuilder: createOpts, External: &ext}
	}
	if !plan.PortSecurity.IsNull() && !plan.PortSecurity.IsUnknown() {
		ps := plan.PortSecurity.ValueBool()
		createOpts = portsecurity.NetworkCreateOptsExt{CreateOptsBuilder: createOpts, PortSecurityEnabled: &ps}
	}
	if !plan.Segments.IsNull() && !plan.Segments.IsUnknown() {
		var segs []segmentModel
		diags.Append(plan.Segments.ElementsAs(ctx, &segs, false)...)
		if diags.HasError() {
			return createOpts
		}
		providerSegs := make([]provider.Segment, 0, len(segs))
		for _, s := range segs {
			providerSegs = append(providerSegs, provider.Segment{
				PhysicalNetwork: s.PhysicalNetwork.ValueString(),
				NetworkType:     s.NetworkType.ValueString(),
				SegmentationID:  int(s.SegmentationID.ValueInt64()),
			})
		}
		createOpts = segmentsCreateOptsExt{CreateOptsBuilder: createOpts, segments: providerSegs}
	}
	// The dns extension, only when a zone is named: "" is the server default,
	// and sending the key at all is a 400 on a Neutron without the extension.
	if v := plan.DNSDomain.ValueString(); v != "" {
		createOpts = dns.NetworkCreateOptsExt{CreateOptsBuilder: createOpts, DNSDomain: v}
	}
	return createOpts
}

// networkUpdateOpts builds the update body from what changed between plan and
// state. dns_domain is sent whenever it differs, including a change to "",
// which is how the association with a zone is removed.
func networkUpdateOpts(plan, state *networkModel) networks.UpdateOptsBuilder {
	name := plan.Name.ValueString()
	description := plan.Description.ValueString()
	adminUp := plan.AdminStateUp.ValueBool()
	base := networks.UpdateOpts{Name: &name, Description: &description, AdminStateUp: &adminUp}
	if !plan.Shared.IsNull() && !plan.Shared.IsUnknown() {
		shared := plan.Shared.ValueBool()
		base.Shared = &shared
	}

	var updateOpts networks.UpdateOptsBuilder = base
	if !plan.External.Equal(state.External) && !plan.External.IsNull() && !plan.External.IsUnknown() {
		ext := plan.External.ValueBool()
		updateOpts = external.UpdateOptsExt{UpdateOptsBuilder: updateOpts, External: &ext}
	}
	if !plan.PortSecurity.Equal(state.PortSecurity) && !plan.PortSecurity.IsNull() && !plan.PortSecurity.IsUnknown() {
		ps := plan.PortSecurity.ValueBool()
		updateOpts = portsecurity.NetworkUpdateOptsExt{UpdateOptsBuilder: updateOpts, PortSecurityEnabled: &ps}
	}
	if !plan.DNSDomain.Equal(state.DNSDomain) && !plan.DNSDomain.IsUnknown() {
		v := plan.DNSDomain.ValueString()
		updateOpts = dns.NetworkUpdateOptsExt{UpdateOptsBuilder: updateOpts, DNSDomain: &v}
	}
	return updateOpts
}

// dnsLabel is neutron-lib's DNS_LABEL_REGEX: it runs after Neutron lower-cases
// the value, so uppercase never reaches it.
var dnsLabel = regexp.MustCompile(`^[a-z0-9-]{1,63}$`)

// invalidDNSDomain reports why s is not an acceptable dns_domain, or "" when
// it is. It applies the rules of neutron-lib's validate_dns_domain, plus one
// of its own: Neutron lower-cases the value before storing it, which Terraform
// would report as "inconsistent result after apply", so mixed case is refused
// here with the value the user should write. A bad value therefore fails at
// plan time with a message that names the attribute, not as a 400 mid-apply.
func invalidDNSDomain(s string) string {
	if s == "" {
		return ""
	}
	if lower := strings.ToLower(s); lower != s {
		return fmt.Sprintf("Neutron stores dns_domain lower-cased; write %q.", lower)
	}
	if !strings.HasSuffix(s, ".") {
		return fmt.Sprintf("%q must be a fully qualified domain name ending in a dot, for example %q.", s, s+".")
	}
	// neutron-lib caps the value two short of the 255-character FQDN size so a
	// record name can still be prefixed.
	if len(s) > 253 {
		return fmt.Sprintf("%q is longer than 253 characters.", s)
	}
	labels := strings.Split(strings.TrimSuffix(s, "."), ".")
	for _, label := range labels {
		switch {
		case label == "":
			return fmt.Sprintf("%q has an empty label.", s)
		case strings.HasPrefix(label, "-") || strings.HasSuffix(label, "-"):
			return fmt.Sprintf("label %q of %q must not start or end with a hyphen.", label, s)
		case !dnsLabel.MatchString(label):
			return fmt.Sprintf("label %q of %q must be 1 to 63 characters, each a lowercase letter, a digit or a hyphen.", label, s)
		}
	}
	if last := labels[len(labels)-1]; len(labels) > 1 && strings.Trim(last, "0123456789") == "" {
		return fmt.Sprintf("the top-level label %q of %q must not be all numeric.", last, s)
	}
	return ""
}

// ValidateConfig rejects a dns_domain Neutron would reject, at plan time.
func (r *networkResource) ValidateConfig(ctx context.Context, req resource.ValidateConfigRequest, resp *resource.ValidateConfigResponse) {
	var cfg networkModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &cfg)...)
	if resp.Diagnostics.HasError() || cfg.DNSDomain.IsNull() || cfg.DNSDomain.IsUnknown() {
		return
	}
	if msg := invalidDNSDomain(cfg.DNSDomain.ValueString()); msg != "" {
		resp.Diagnostics.AddAttributeError(path.Root("dns_domain"), "Invalid dns_domain", msg)
	}
}
```

Replace the body of `Update` between the client construction and `networks.Update` with:
```go
	updateOpts := networkUpdateOpts(&plan, &state)
```
(delete the now-duplicated `name`/`description`/`adminUp`/`base`/`updateOpts` block).

In `readInto`, after `m.TenantID = ...`:
```go
	m.DNSDomain = types.StringValue(n.DNSDomain)
```

In `internal/services/networking/network_data_source.go`: add `DNSDomain types.String \`tfsdk:"dns_domain"\`` to `networkDataSourceModel` after `PortSecurity`; add to the schema after `port_security_enabled`:
```go
			"dns_domain":            schema.StringAttribute{Computed: true, MarkdownDescription: "The Designate zone the network's ports publish records to; `\"\"` when none."},
```
and in `Read`, after `data.PortSecurity = ...`: `data.DNSDomain = types.StringValue(n.DNSDomain)`.

- [ ] **Step 4: Run the unit tests and the full checks**

Run: `go test ./internal/services/networking/ -run 'TestNetworkCreateOptsDNSDomain|TestNetworkUpdateOptsDNSDomain|TestInvalidDNSDomain' -count=1 && gofmt -l . && go vet ./... && golangci-lint run ./... && go test ./internal/... -timeout 120s`
Expected: PASS, no gofmt output, 0 lint issues.

- [ ] **Step 5: Add the acceptance test**

Append to `internal/services/networking/network_subnet_test.go` (add `"regexp"` to its imports):

```go
// TestAccNetworkingNetworkDNSDomain rejects a malformed zone name at plan
// time, then associates a network with a zone name, moves it to another,
// clears it by omitting the attribute, and imports it. Neutron checks the
// name's format but not that the zone exists, so no Designate zone is needed
// here; the DNS guide's example is what proves records get published.
func TestAccNetworkingNetworkDNSDomain(t *testing.T) {
	const rn = "pcd_networking_network.dns"
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { acctest.PreCheck(t) },
		ProtoV6ProviderFactories: acctest.ProtoV6ProviderFactories,
		CheckDestroy:             testAccCheckNetworkDestroy(t),
		Steps: []resource.TestStep{
			{
				Config: testAccNetworkDNSDomainConfig(`dns_domain = "tf-acc-no-dot.example.com"`),
				// Match the diagnostic summary: Terraform wraps the detail text
				// mid-sentence in the stderr ExpectError sees.
				ExpectError: regexp.MustCompile(`Invalid dns_domain`),
			},
			{
				Config: testAccNetworkDNSDomainConfig(`dns_domain = "tf-acc-a.example.com."`),
				Check: resource.ComposeAggregateTestCheckFunc(
					testAccCheckNetworkExists(t, rn),
					resource.TestCheckResourceAttr(rn, "dns_domain", "tf-acc-a.example.com."),
					resource.TestCheckResourceAttrPair("data.pcd_networking_network.dns", "dns_domain", rn, "dns_domain"),
				),
			},
			{
				Config: testAccNetworkDNSDomainConfig(`dns_domain = "tf-acc-b.example.com."`),
				Check:  resource.TestCheckResourceAttr(rn, "dns_domain", "tf-acc-b.example.com."),
			},
			{
				// Omitting the attribute plans "" and the apply clears the association.
				Config: testAccNetworkDNSDomainConfig(""),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(rn, "dns_domain", ""),
					resource.TestCheckResourceAttr("data.pcd_networking_network.dns", "dns_domain", ""),
				),
			},
			{ResourceName: rn, ImportState: true, ImportStateVerify: true},
		},
	})
}

func testAccNetworkDNSDomainConfig(dnsDomainLine string) string {
	return fmt.Sprintf(`
resource "pcd_networking_network" "dns" {
  name = "tf-acc-dns-net"
  %s
}

data "pcd_networking_network" "dns" {
  network_id = pcd_networking_network.dns.id
}
`, dnsDomainLine)
}
```

Run it against the lab (Task 8 explains `lab_env.py`; it needs Task 0 done):
`python3 <scratchpad>/lab_env.py -C "$(pwd)" env TF_ACC=1 go test ./internal/services/networking/ -run TestAccNetworkingNetworkDNSDomain -v -count=1 -timeout 20m`
Expected: PASS (about a minute). Without a lab it skips.

- [ ] **Step 6: Example and generated docs**

Append to `examples/resources/pcd_networking_network/resource.tf`:

```hcl
# A network whose ports publish DNS records into a Designate zone. dns_domain
# is the zone's name (it ends in a dot) and the zone must already exist. Which
# fixed IPs get records depends on the subnets' dns_publish_fixed_ip; see the
# DNS guide. Omit dns_domain to remove the association.
resource "pcd_dns_zone" "app" {
  name  = "app.example.com."
  email = "dns-admin@example.com"
}

resource "pcd_networking_network" "app" {
  name       = "app-net"
  dns_domain = pcd_dns_zone.app.name
}
```

Run: `terraform fmt -recursive ./examples && make generate && git diff --stat docs/`
Expected: `docs/resources/networking_network.md` and `docs/data-sources/networking_network.md` change; nothing else.

- [ ] **Step 7: Commit**

```bash
git add internal/services/networking/network_resource.go internal/services/networking/network_data_source.go internal/services/networking/network_internal_test.go internal/services/networking/network_subnet_test.go examples/resources/pcd_networking_network/resource.tf docs/
git commit -m "networking: dns_domain on pcd_networking_network (PCD-9926)"
```

---

### Task 2: `dns_publish_fixed_ip` on `pcd_networking_subnet` (PCD-9945)

**Files:**
- Modify: `internal/services/networking/subnet_resource.go`
- Modify: `internal/services/networking/subnet_data_source.go`
- Create: `internal/services/networking/subnet_internal_test.go`
- Modify: `internal/services/networking/network_subnet_test.go`
- Modify: `examples/resources/pcd_networking_subnet/resource.tf`

**Interfaces:**
- Produces: `func subnetCreateOpts(ctx context.Context, plan *subnetModel, diags *diag.Diagnostics) subnets.CreateOpts` and `func subnetUpdateOpts(ctx context.Context, plan, state *subnetModel, diags *diag.Diagnostics) subnets.UpdateOpts`, and the `dns_publish_fixed_ip` attribute on the resource and data source.

- [ ] **Step 1: Write the failing unit tests**

Create `internal/services/networking/subnet_internal_test.go`:

```go
// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package networking

import (
	"context"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

func dnsSubnetModel(publish types.Bool) *subnetModel {
	return &subnetModel{
		NetworkID:         types.StringValue("net-1"),
		Name:              types.StringValue("sub"),
		Description:       types.StringValue(""),
		CIDR:              types.StringValue("10.0.0.0/24"),
		IPVersion:         types.Int64Value(4),
		GatewayIP:         types.StringValue(""),
		EnableDHCP:        types.BoolValue(true),
		DNSNameservers:    types.ListNull(types.StringType),
		AllocationPools:   types.ListNull(poolObjType),
		Tags:              types.SetNull(types.StringType),
		DNSPublishFixedIP: publish,
	}
}

// dns_publish_fixed_ip is sent on create only when true: false is the server
// default, and a Neutron without the subnet-dns-publish-fixed-ip extension
// rejects the key.
func TestSubnetCreateOptsDNSPublishFixedIP(t *testing.T) {
	var diags diag.Diagnostics
	opts := subnetCreateOpts(context.Background(), dnsSubnetModel(types.BoolValue(true)), &diags)
	if diags.HasError() {
		t.Fatal(diags)
	}
	if opts.DNSPublishFixedIP == nil || !*opts.DNSPublishFixedIP {
		t.Fatalf("dns_publish_fixed_ip = %v; records for this subnet's fixed IPs would never be published", opts.DNSPublishFixedIP)
	}
	if opts.NetworkID != "net-1" || opts.CIDR != "10.0.0.0/24" || opts.EnableDHCP == nil || !*opts.EnableDHCP {
		t.Fatalf("base options changed while moving them into subnetCreateOpts: %+v", opts)
	}
	opts = subnetCreateOpts(context.Background(), dnsSubnetModel(types.BoolValue(false)), &diags)
	if opts.DNSPublishFixedIP != nil {
		t.Fatalf("dns_publish_fixed_ip sent on create although false")
	}
}

// An update sends the flag exactly when it changed, in both directions.
func TestSubnetUpdateOptsDNSPublishFixedIP(t *testing.T) {
	for _, tc := range []struct {
		name        string
		plan, state bool
		want        *bool
	}{
		{name: "unchanged false", plan: false, state: false},
		{name: "unchanged true", plan: true, state: true},
		{name: "enable", plan: true, state: false, want: boolPtr(true)},
		{name: "disable", plan: false, state: true, want: boolPtr(false)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var diags diag.Diagnostics
			opts := subnetUpdateOpts(context.Background(), dnsSubnetModel(types.BoolValue(tc.plan)), dnsSubnetModel(types.BoolValue(tc.state)), &diags)
			if diags.HasError() {
				t.Fatal(diags)
			}
			switch {
			case tc.want == nil && opts.DNSPublishFixedIP != nil:
				t.Fatalf("dns_publish_fixed_ip sent (%v) although unchanged", *opts.DNSPublishFixedIP)
			case tc.want != nil && (opts.DNSPublishFixedIP == nil || *opts.DNSPublishFixedIP != *tc.want):
				t.Fatalf("dns_publish_fixed_ip = %v, want %v", opts.DNSPublishFixedIP, *tc.want)
			}
		})
	}
}

func boolPtr(b bool) *bool { return &b }
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/services/networking/ -run 'TestSubnetCreateOptsDNSPublishFixedIP|TestSubnetUpdateOptsDNSPublishFixedIP' -count=1`
Expected: build failure, `undefined: subnetCreateOpts`.

- [ ] **Step 3: Implement the attribute**

In `internal/services/networking/subnet_resource.go`:

Add to `subnetModel` after `AllocationPools`:
```go
	DNSPublishFixedIP types.Bool   `tfsdk:"dns_publish_fixed_ip"`
```

Add to the schema after `dns_nameservers`:
```go
			"dns_publish_fixed_ip": schema.BoolAttribute{
				Optional: true, Computed: true, Default: booldefault.StaticBool(false),
				MarkdownDescription: "Publish a DNS record for every fixed IP allocated from this subnet, in the zone named " +
					"by the network's `dns_domain`. Defaults to `false`. A network with `external = true` publishes nothing " +
					"until at least one of its subnets sets this; on other networks it is the per-subnet opt-in (on a " +
					"dual-stack network, for example, to publish only the routable family). Records are created when a port " +
					"is created or updated, never retroactively, so set this before booting instances.",
			},
```

Replace the option-building code in `Create` (from `enableDHCP := ...` through the `if resp.Diagnostics.HasError() { return }` that follows `createOpts.GatewayIP`) with:
```go
	createOpts := subnetCreateOpts(ctx, &plan, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}
```
and the corresponding block in `Update` (from `name := ...` through the same check) with:
```go
	updateOpts := subnetUpdateOpts(ctx, &plan, &state, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}
```

Add after `ImportState`:

```go
// subnetCreateOpts builds the create body. dns_publish_fixed_ip is sent only
// when true: false is the server default, and a Neutron without the extension
// rejects the key. Kept apart from Create so the body is unit-testable.
func subnetCreateOpts(ctx context.Context, plan *subnetModel, diags *diag.Diagnostics) subnets.CreateOpts {
	enableDHCP := plan.EnableDHCP.ValueBool()
	createOpts := subnets.CreateOpts{
		NetworkID:       plan.NetworkID.ValueString(),
		CIDR:            plan.CIDR.ValueString(),
		Name:            plan.Name.ValueString(),
		Description:     plan.Description.ValueString(),
		IPVersion:       gophercloud.IPVersion(plan.IPVersion.ValueInt64()),
		EnableDHCP:      &enableDHCP,
		TenantID:        plan.TenantID.ValueString(),
		DNSNameservers:  listToStrings(ctx, plan.DNSNameservers, diags),
		AllocationPools: poolsFromList(ctx, plan.AllocationPools, diags),
	}
	if v := plan.GatewayIP.ValueString(); v != "" {
		createOpts.GatewayIP = &v
	}
	if plan.DNSPublishFixedIP.ValueBool() {
		publish := true
		createOpts.DNSPublishFixedIP = &publish
	}
	return createOpts
}

// subnetUpdateOpts builds the update body from what changed between plan and
// state; dns_publish_fixed_ip is sent in either direction when it differs.
func subnetUpdateOpts(ctx context.Context, plan, state *subnetModel, diags *diag.Diagnostics) subnets.UpdateOpts {
	name := plan.Name.ValueString()
	description := plan.Description.ValueString()
	enableDHCP := plan.EnableDHCP.ValueBool()
	updateOpts := subnets.UpdateOpts{Name: &name, Description: &description, EnableDHCP: &enableDHCP}
	if v := plan.GatewayIP.ValueString(); v != "" {
		updateOpts.GatewayIP = &v
	}
	if !plan.DNSNameservers.Equal(state.DNSNameservers) {
		dns := listToStrings(ctx, plan.DNSNameservers, diags)
		updateOpts.DNSNameservers = &dns
	}
	if !plan.AllocationPools.Equal(state.AllocationPools) {
		updateOpts.AllocationPools = poolsFromList(ctx, plan.AllocationPools, diags)
	}
	if !plan.DNSPublishFixedIP.Equal(state.DNSPublishFixedIP) && !plan.DNSPublishFixedIP.IsUnknown() {
		publish := plan.DNSPublishFixedIP.ValueBool()
		updateOpts.DNSPublishFixedIP = &publish
	}
	return updateOpts
}
```

In `readInto`, after `m.EnableDHCP = ...`: `m.DNSPublishFixedIP = types.BoolValue(sub.DNSPublishFixedIP)`.

In `internal/services/networking/subnet_data_source.go`: add `DNSPublishFixedIP types.Bool \`tfsdk:"dns_publish_fixed_ip"\`` to the model after `EnableDHCP`; add to the schema after `enable_dhcp`:
```go
			"dns_publish_fixed_ip": schema.BoolAttribute{Computed: true, MarkdownDescription: "Whether fixed IPs from this subnet are published to the network's DNS zone."},
```
and in `Read`, after `data.EnableDHCP = ...`: `data.DNSPublishFixedIP = types.BoolValue(sub.DNSPublishFixedIP)`.

- [ ] **Step 4: Run the unit tests and the full checks**

Run: `go test ./internal/services/networking/ -run 'TestSubnet' -count=1 && gofmt -l . && go vet ./... && golangci-lint run ./... && go test ./internal/... -timeout 120s`
Expected: PASS, clean.

- [ ] **Step 5: Add the acceptance test**

Append to `internal/services/networking/network_subnet_test.go`:

```go
// TestAccNetworkingSubnetDNSPublishFixedIP turns publishing on at create,
// off by update, leaves it off when the attribute is omitted, and imports.
func TestAccNetworkingSubnetDNSPublishFixedIP(t *testing.T) {
	const rn = "pcd_networking_subnet.dns"
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { acctest.PreCheck(t) },
		ProtoV6ProviderFactories: acctest.ProtoV6ProviderFactories,
		CheckDestroy: resource.ComposeAggregateTestCheckFunc(
			testAccCheckSubnetDestroy(t),
			testAccCheckNetworkDestroy(t),
		),
		Steps: []resource.TestStep{
			{
				Config: testAccSubnetDNSPublishConfig(`dns_publish_fixed_ip = true`),
				Check: resource.ComposeAggregateTestCheckFunc(
					testAccCheckSubnetExists(t, rn),
					resource.TestCheckResourceAttr(rn, "dns_publish_fixed_ip", "true"),
					resource.TestCheckResourceAttrPair("data.pcd_networking_subnet.dns", "dns_publish_fixed_ip", rn, "dns_publish_fixed_ip"),
				),
			},
			{
				Config: testAccSubnetDNSPublishConfig(`dns_publish_fixed_ip = false`),
				Check:  resource.TestCheckResourceAttr(rn, "dns_publish_fixed_ip", "false"),
			},
			{
				Config: testAccSubnetDNSPublishConfig(""),
				Check:  resource.TestCheckResourceAttr(rn, "dns_publish_fixed_ip", "false"),
			},
			{ResourceName: rn, ImportState: true, ImportStateVerify: true},
		},
	})
}

func testAccSubnetDNSPublishConfig(publishLine string) string {
	return fmt.Sprintf(`
resource "pcd_networking_network" "dns" {
  name       = "tf-acc-dns-subnet-net"
  dns_domain = "tf-acc-subnet.example.com."
}

resource "pcd_networking_subnet" "dns" {
  name       = "tf-acc-dns-subnet"
  network_id = pcd_networking_network.dns.id
  cidr       = "10.103.0.0/24"
  %s
}

data "pcd_networking_subnet" "dns" {
  subnet_id = pcd_networking_subnet.dns.id
}
`, publishLine)
}
```

Run: `python3 <scratchpad>/lab_env.py -C "$(pwd)" env TF_ACC=1 go test ./internal/services/networking/ -run 'TestAccNetworkingSubnetDNSPublishFixedIP|TestAccNetworkingNetworkAndSubnet_basic|TestAccNetworkingDataSources_byName' -v -count=1 -timeout 30m`
Expected: PASS (the two existing tests prove the moved option code still behaves).

- [ ] **Step 6: Example and generated docs**

Append to `examples/resources/pcd_networking_subnet/resource.tf`:

```hcl
# A subnet whose fixed IPs are published to the network's DNS zone. The
# network names the zone (dns_domain); the subnet opts in. On an external
# network this is required before any record is created; see the DNS guide.
resource "pcd_networking_network" "app" {
  name       = "app-net"
  dns_domain = "app.example.com."
}

resource "pcd_networking_subnet" "app" {
  network_id           = pcd_networking_network.app.id
  name                 = "app-subnet"
  cidr                 = "10.1.0.0/24"
  dns_publish_fixed_ip = true
}
```

Run: `terraform fmt -recursive ./examples && make generate && git diff --stat docs/`
Expected: `docs/resources/networking_subnet.md` and `docs/data-sources/networking_subnet.md` change.

- [ ] **Step 7: Commit**

```bash
git add internal/services/networking/subnet_resource.go internal/services/networking/subnet_data_source.go internal/services/networking/subnet_internal_test.go internal/services/networking/network_subnet_test.go examples/resources/pcd_networking_subnet/resource.tf docs/
git commit -m "networking: dns_publish_fixed_ip on pcd_networking_subnet (PCD-9945)"
```

---

### Task 3: `pcd_dns_pools_config` data source (PCD-9943, templating and validation)

**Files:**
- Create: `internal/services/dns/pools_config_data_source.go`
- Create: `internal/services/dns/pools_config_internal_test.go`
- Create: `examples/data-sources/pcd_dns_pools_config/data-source.tf`
- Modify: `internal/provider/provider.go`

**Interfaces:**
- Produces: `func NewPoolsConfigDataSource() datasource.DataSource`; package-private `type poolConfig` (with `nsRecord`, `hostPort`, `poolTarget`, `targetOptions`), `func validatePools(pools []poolConfig) (errs, warns []poolIssue)`, `func renderPoolsYAML(pools []poolConfig) (string, error)`, `func decodePools(ctx context.Context, list types.List) ([]poolConfig, diag.Diagnostics)`. Task 4 consumes the data source's `yaml` and `id`.

- [ ] **Step 1: Write the failing unit tests**

Create `internal/services/dns/pools_config_internal_test.go`:

```go
// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package dns

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// bind9Pool is the CE lab pool from the 2026-07-13 DNS validation runbook.
func bind9Pool() poolConfig {
	return poolConfig{
		Name:        "default",
		Description: "BIND9 on hyp1",
		NSRecords:   []nsRecord{{Hostname: "ns1.pcd.local.", Priority: 1}},
		Nameservers: []hostPort{{Host: "172.16.122.251", Port: 53}},
		Targets: []poolTarget{{
			Type:    "bind9",
			Masters: []hostPort{{Host: "172.16.122.251", Port: 5354}},
			Options: targetOptions{Host: "172.16.122.251", Port: 53, RNDCHost: "172.16.122.251", RNDCPort: 953, RNDCKeyFile: "/etc/designate/rndc.key"},
		}},
	}
}

// pdns4Pool is the reporter's configuration from PCD-9943.
func pdns4Pool() poolConfig {
	return poolConfig{
		Name:        "default",
		Description: "Cloudflare via pdns4-shim and external-dns",
		Attributes:  map[string]string{},
		NSRecords:   []nsRecord{{Hostname: "ns1.pcd-ce-lab.usmnblm01.rye.ninja.", Priority: 1}},
		Nameservers: []hostPort{{Host: "fd97:45c2:b3a1:f00::9280", Port: 53}},
		Targets: []poolTarget{{
			Type:        "pdns4",
			Description: "pdns4-shim",
			Masters:     []hostPort{{Host: "10.45.0.1", Port: 53}},
			Options:     targetOptions{Host: "10.45.60.1", Port: 53, APIEndpoint: "https://pdns4-shim.rye.ninja:443", APIToken: "example_token"},
		}},
	}
}

func issuePaths(issues []poolIssue) []string {
	out := make([]string, 0, len(issues))
	for _, i := range issues {
		out = append(out, i.Path)
	}
	return out
}

func TestValidatePoolsAcceptsBothBackends(t *testing.T) {
	for _, p := range []poolConfig{bind9Pool(), pdns4Pool()} {
		if errs, warns := validatePools([]poolConfig{p}); len(errs) != 0 || len(warns) != 0 {
			t.Fatalf("%s: unexpected issues: errors %v warnings %v", p.Targets[0].Type, errs, warns)
		}
	}
	// Designate's bind9 backend defaults rndc_host to 127.0.0.1 and rndc_port
	// to 953, and takes either an rndc key file or an rndc config file, so a
	// target that names only the config file is complete.
	p := bind9Pool()
	p.Targets[0].Options.RNDCHost, p.Targets[0].Options.RNDCPort, p.Targets[0].Options.RNDCKeyFile = "", 0, ""
	p.Targets[0].Options.RNDCConfigFile = "/etc/designate/rndc.conf"
	if errs, _ := validatePools([]poolConfig{p}); len(errs) != 0 {
		t.Fatalf("a bind9 target with rndc_config_file alone was rejected; designate-manage accepts it: %v", errs)
	}
}

// Every rule the reporter had to write as a Terraform variable validation
// (PCD-9943) has one case here; the path tells the user where to look.
func TestValidatePoolsRejects(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(p *poolConfig)
		path   string
	}{
		{"empty name", func(p *poolConfig) { p.Name = "" }, "pools[0].name"},
		{"ns_record without trailing dot", func(p *poolConfig) { p.NSRecords[0].Hostname = "ns1.pcd.local" }, "pools[0].ns_records[0].hostname"},
		{"ns_record priority 0", func(p *poolConfig) { p.NSRecords[0].Priority = 0 }, "pools[0].ns_records[0].priority"},
		{"no ns_records", func(p *poolConfig) { p.NSRecords = nil }, "pools[0].ns_records"},
		{"nameserver host not an IP", func(p *poolConfig) { p.Nameservers[0].Host = "ns1.pcd.local" }, "pools[0].nameservers[0].host"},
		{"nameserver port out of range", func(p *poolConfig) { p.Nameservers[0].Port = 70000 }, "pools[0].nameservers[0].port"},
		{"no nameservers", func(p *poolConfig) { p.Nameservers = nil }, "pools[0].nameservers"},
		{"no targets", func(p *poolConfig) { p.Targets = nil }, "pools[0].targets"},
		{"unknown target type", func(p *poolConfig) { p.Targets[0].Type = "powerdns" }, "pools[0].targets[0].type"},
		{"master host not an IP", func(p *poolConfig) { p.Targets[0].Masters[0].Host = "mdns.local" }, "pools[0].targets[0].masters[0].host"},
		{"master port 0", func(p *poolConfig) { p.Targets[0].Masters[0].Port = 0 }, "pools[0].targets[0].masters[0].port"},
		{"no masters", func(p *poolConfig) { p.Targets[0].Masters = nil }, "pools[0].targets[0].masters"},
		{"options host not an IP", func(p *poolConfig) { p.Targets[0].Options.Host = "bind.local" }, "pools[0].targets[0].options.host"},
		{"options port out of range", func(p *poolConfig) { p.Targets[0].Options.Port = 0 }, "pools[0].targets[0].options.port"},
		{"bind9 without rndc key or config file", func(p *poolConfig) { p.Targets[0].Options.RNDCKeyFile = "" }, "pools[0].targets[0].options"},
		{"bind9 rndc_port out of range", func(p *poolConfig) { p.Targets[0].Options.RNDCPort = 65536 }, "pools[0].targets[0].options.rndc_port"},
		{"bind9 with api options", func(p *poolConfig) { p.Targets[0].Options.APIToken = "x" }, "pools[0].targets[0].options"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := bind9Pool()
			tc.mutate(&p)
			errs, _ := validatePools([]poolConfig{p})
			for _, e := range errs {
				if e.Path == tc.path {
					return
				}
			}
			t.Fatalf("no error at %s; designate-manage would have been the first to complain. got %v", tc.path, issuePaths(errs))
		})
	}
	for _, tc := range []struct {
		name   string
		mutate func(p *poolConfig)
		path   string
	}{
		{"pdns4 without api_token", func(p *poolConfig) { p.Targets[0].Options.APIToken = "" }, "pools[0].targets[0].options"},
		{"pdns4 with rndc options", func(p *poolConfig) { p.Targets[0].Options.RNDCHost = "10.45.60.1" }, "pools[0].targets[0].options"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := pdns4Pool()
			tc.mutate(&p)
			errs, _ := validatePools([]poolConfig{p})
			for _, e := range errs {
				if e.Path == tc.path {
					return
				}
			}
			t.Fatalf("no error at %s; got %v", tc.path, issuePaths(errs))
		})
	}
	errs, _ := validatePools([]poolConfig{bind9Pool(), bind9Pool()})
	if len(errs) == 0 || errs[0].Path != "pools[1].name" {
		t.Fatalf("duplicate pool names not rejected: %v", issuePaths(errs))
	}
	if errs, _ := validatePools(nil); len(errs) == 0 {
		t.Fatalf("an empty pool list produced no error")
	}
}

// A full-length IPv6 master is valid YAML and valid for the pool tables, but
// designate-manage copies it into zone_masters.host, VARCHAR(32) (PCD-9946).
// That is a warning, not an error: a Designate with the column widened is fine.
func TestValidatePoolsWarnsOnLongMaster(t *testing.T) {
	p := pdns4Pool()
	p.Targets[0].Masters[0].Host = "fd97:45c2:b3a1:100:e481:3fff:fec5:249f" // 38 characters
	errs, warns := validatePools([]poolConfig{p})
	if len(errs) != 0 {
		t.Fatalf("a long master must not be an error: %v", errs)
	}
	if len(warns) != 1 || warns[0].Path != "pools[0].targets[0].masters[0].host" || !strings.Contains(warns[0].Msg, "PCD-9946") {
		t.Fatalf("expected one warning naming PCD-9946 at the master host, got %v", warns)
	}
	p.Targets[0].Masters[0].Host = "fd97:45c2:b3a1:100::5354" // 24 characters, the reporter's workaround
	if _, warns := validatePools([]poolConfig{p}); len(warns) != 0 {
		t.Fatalf("a 24-character master must not warn: %v", warns)
	}
}

// The rendered file must be what designate-manage pool update reads: one YAML
// document holding a list of pools, with the keys the upstream sample uses and
// nothing that was not configured.
func TestRenderPoolsYAML(t *testing.T) {
	out, err := renderPoolsYAML([]poolConfig{bind9Pool(), pdns4Pool()})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out, "---\n- name: default\n") {
		t.Fatalf("unexpected document start:\n%s", out)
	}
	var back []map[string]any
	if err := yaml.Unmarshal([]byte(out), &back); err != nil {
		t.Fatalf("rendered YAML does not parse back: %v\n%s", err, out)
	}
	if len(back) != 2 {
		t.Fatalf("rendered %d pools, want 2", len(back))
	}
	bind := back[0]
	if _, ok := bind["id"]; ok {
		t.Fatalf("id rendered although unset; designate-manage would look up a pool by an empty id")
	}
	if attrs, ok := bind["attributes"].(map[string]any); !ok || len(attrs) != 0 {
		t.Fatalf("attributes = %v, want an empty mapping (the upstream sample always writes attributes:)", bind["attributes"])
	}
	if _, ok := bind["also_notifies"]; ok {
		t.Fatalf("also_notifies rendered although unset")
	}
	target := bind["targets"].([]any)[0].(map[string]any)
	opts := target["options"].(map[string]any)
	for _, k := range []string{"host", "port", "rndc_host", "rndc_port", "rndc_key_file"} {
		if _, ok := opts[k]; !ok {
			t.Fatalf("bind9 options lack %s: %v", k, opts)
		}
	}
	for _, k := range []string{"api_endpoint", "api_token", "rndc_config_file"} {
		if _, ok := opts[k]; ok {
			t.Fatalf("bind9 options carry %s although unset", k)
		}
	}
	if got := opts["rndc_port"]; got != 953 {
		t.Fatalf("rndc_port = %v (%T), want the integer 953", got, got)
	}
	pdns := back[1]["targets"].([]any)[0].(map[string]any)["options"].(map[string]any)
	if pdns["api_endpoint"] != "https://pdns4-shim.rye.ninja:443" || pdns["api_token"] != "example_token" {
		t.Fatalf("pdns4 options = %v", pdns)
	}
	if _, ok := pdns["rndc_host"]; ok {
		t.Fatalf("pdns4 options carry rndc_host")
	}
	withID := bind9Pool()
	withID.ID = "794ccc2c-d751-44fe-b57f-8894c9f5c842"
	out, _ = renderPoolsYAML([]poolConfig{withID})
	if !strings.Contains(out, "\n  id: 794ccc2c-d751-44fe-b57f-8894c9f5c842\n") {
		t.Fatalf("pool id not rendered:\n%s", out)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/services/dns/ -run 'TestValidatePools|TestRenderPoolsYAML' -count=1`
Expected: build failure, `undefined: poolConfig`.

- [ ] **Step 3: Write the data source**

Create `internal/services/dns/pools_config_data_source.go`:

```go
// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package dns

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-framework/types/basetypes"
	"gopkg.in/yaml.v3"
)

var _ datasource.DataSource = (*poolsConfigDataSource)(nil)

// NewPoolsConfigDataSource is the factory registered with the provider.
func NewPoolsConfigDataSource() datasource.DataSource {
	return &poolsConfigDataSource{}
}

// poolsConfigDataSource renders the pools.yaml that `designate-manage pool
// update` reads on a host carrying PCD's dns role. It never talks to an API:
// Designate's own pools API carries name, description, attributes and
// ns_records only, never targets or nameservers, so the file is the only
// complete description of a pool and the host is the only place it can go.
type poolsConfigDataSource struct{}

type poolsConfigModel struct {
	ID    types.String `tfsdk:"id"`
	Pools types.List   `tfsdk:"pools"`
	YAML  types.String `tfsdk:"yaml"`
}

type poolModel struct {
	ID           types.String `tfsdk:"id"`
	Name         types.String `tfsdk:"name"`
	Description  types.String `tfsdk:"description"`
	Attributes   types.Map    `tfsdk:"attributes"`
	NSRecords    types.List   `tfsdk:"ns_records"`
	Nameservers  types.List   `tfsdk:"nameservers"`
	AlsoNotifies types.List   `tfsdk:"also_notifies"`
	Targets      types.List   `tfsdk:"targets"`
}

type nsRecordModel struct {
	Hostname types.String `tfsdk:"hostname"`
	Priority types.Int64  `tfsdk:"priority"`
}

type hostPortModel struct {
	Host types.String `tfsdk:"host"`
	Port types.Int64  `tfsdk:"port"`
}

type targetModel struct {
	Type        types.String `tfsdk:"type"`
	Description types.String `tfsdk:"description"`
	Masters     types.List   `tfsdk:"masters"`
	Options     types.Object `tfsdk:"options"`
}

type targetOptionsModel struct {
	Host           types.String `tfsdk:"host"`
	Port           types.Int64  `tfsdk:"port"`
	RNDCHost       types.String `tfsdk:"rndc_host"`
	RNDCPort       types.Int64  `tfsdk:"rndc_port"`
	RNDCKeyFile    types.String `tfsdk:"rndc_key_file"`
	RNDCConfigFile types.String `tfsdk:"rndc_config_file"`
	APIEndpoint    types.String `tfsdk:"api_endpoint"`
	APIToken       types.String `tfsdk:"api_token"`
}

// poolConfig is one pool as designate-manage reads it. Struct field order is
// the order the YAML is written in, which follows the upstream pools.yaml sample.
type poolConfig struct {
	Name         string            `yaml:"name"`
	ID           string            `yaml:"id,omitempty"`
	Description  string            `yaml:"description,omitempty"`
	Attributes   map[string]string `yaml:"attributes"`
	NSRecords    []nsRecord        `yaml:"ns_records"`
	Nameservers  []hostPort        `yaml:"nameservers"`
	AlsoNotifies []hostPort        `yaml:"also_notifies,omitempty"`
	Targets      []poolTarget      `yaml:"targets"`
}

type nsRecord struct {
	Hostname string `yaml:"hostname"`
	Priority int    `yaml:"priority"`
}

type hostPort struct {
	Host string `yaml:"host"`
	Port int    `yaml:"port"`
}

type poolTarget struct {
	Type        string        `yaml:"type"`
	Description string        `yaml:"description,omitempty"`
	Masters     []hostPort    `yaml:"masters"`
	Options     targetOptions `yaml:"options"`
}

type targetOptions struct {
	Host           string `yaml:"host"`
	Port           int    `yaml:"port"`
	RNDCHost       string `yaml:"rndc_host,omitempty"`
	RNDCPort       int    `yaml:"rndc_port,omitempty"`
	RNDCKeyFile    string `yaml:"rndc_key_file,omitempty"`
	RNDCConfigFile string `yaml:"rndc_config_file,omitempty"`
	APIEndpoint    string `yaml:"api_endpoint,omitempty"`
	APIToken       string `yaml:"api_token,omitempty"`
}

func (d *poolsConfigDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_dns_pools_config"
}

func (d *poolsConfigDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	hostPortObject := func(hostDesc string) schema.NestedAttributeObject {
		return schema.NestedAttributeObject{Attributes: map[string]schema.Attribute{
			"host": schema.StringAttribute{Required: true, MarkdownDescription: hostDesc},
			"port": schema.Int64Attribute{Required: true, MarkdownDescription: "The port, 1 to 65535."},
		}}
	}
	resp.Schema = schema.Schema{
		MarkdownDescription: "Renders and validates the Designate `pools.yaml` for the hosts that carry PCD's `dns` cluster " +
			"role. PCD offers no API for a pool's targets and nameservers (Designate's pools API stops at name, " +
			"description, attributes and NS records), so the file still has to reach each DNS host and be applied " +
			"with `designate-manage pool update --file`; the DNS guide shows one way to deliver it. What this data " +
			"source adds is the typed schema and the checks that otherwise need a page of variable validation: " +
			"NS record names end in a dot, hosts are IP literals, ports are in range, a target's options match its " +
			"type, and master addresses fit the width Designate can store for zones.",
		Attributes: map[string]schema.Attribute{
			"id":   schema.StringAttribute{Computed: true, MarkdownDescription: "SHA-256 of `yaml`. It changes exactly when the rendered file changes, so it works as the trigger for whatever delivers the file."},
			"yaml": schema.StringAttribute{Computed: true, Sensitive: true, MarkdownDescription: "The rendered `pools.yaml`: one document holding the list of pools. Sensitive because pdns4 targets carry an API token."},
			"pools": schema.ListNestedAttribute{
				Required:            true,
				MarkdownDescription: "The pools, in file order. Designate matches each by `id` when given, else by `name`.",
				NestedObject: schema.NestedAttributeObject{Attributes: map[string]schema.Attribute{
					"name":        schema.StringAttribute{Required: true, MarkdownDescription: "The pool name; `default` is the pool PCD creates. A pool's name cannot change after creation."},
					"id":          schema.StringAttribute{Optional: true, MarkdownDescription: "The UUID of an existing pool to update in place, as `GET /designate/v2/pools` reports it. Omit to match by name."},
					"description": schema.StringAttribute{Optional: true, MarkdownDescription: "A description of the pool."},
					"attributes":  schema.MapAttribute{Optional: true, ElementType: types.StringType, MarkdownDescription: "Pool attributes used for scheduling zones onto pools (for example `service_tier`)."},
					"ns_records": schema.ListNestedAttribute{
						Required:            true,
						MarkdownDescription: "The NS records that every zone hosted in this pool advertises. Their hostnames are names resolvable outside PCD that point at the pool's nameservers.",
						NestedObject: schema.NestedAttributeObject{Attributes: map[string]schema.Attribute{
							"hostname": schema.StringAttribute{Required: true, MarkdownDescription: "A fully qualified name ending in a dot."},
							"priority": schema.Int64Attribute{Required: true, MarkdownDescription: "The record's priority, 1 or greater."},
						}},
					},
					"nameservers": schema.ListNestedAttribute{
						Required:            true,
						MarkdownDescription: "The DNS servers Designate queries to confirm a change has propagated.",
						NestedObject:        hostPortObject("The nameserver's IP address."),
					},
					"also_notifies": schema.ListNestedAttribute{
						Optional:            true,
						MarkdownDescription: "Extra servers that receive a NOTIFY on every zone change.",
						NestedObject:        hostPortObject("The server's IP address."),
					},
					"targets": schema.ListNestedAttribute{
						Required:            true,
						MarkdownDescription: "The backend servers Designate pushes zones to: one `bind9` target per BIND server, or a `pdns4` target per PowerDNS API endpoint.",
						NestedObject: schema.NestedAttributeObject{Attributes: map[string]schema.Attribute{
							"type":        schema.StringAttribute{Required: true, MarkdownDescription: "`bind9` or `pdns4`."},
							"description": schema.StringAttribute{Optional: true, MarkdownDescription: "A description of the target."},
							"masters": schema.ListNestedAttribute{
								Required:            true,
								MarkdownDescription: "The `designate-mdns` servers the backend transfers zones from: the address of each host carrying the `dns` role, port 5354. Keep each address at most 32 characters long: Designate copies them into every zone's masters, whose column is that wide (PCD-9946).",
								NestedObject:        hostPortObject("The mdns IP address."),
							},
							"options": schema.SingleNestedAttribute{
								Required:            true,
								MarkdownDescription: "How Designate reaches the backend. `host` and `port` always; `rndc_*` for `bind9`; `api_endpoint` and `api_token` for `pdns4`.",
								Attributes: map[string]schema.Attribute{
									"host":             schema.StringAttribute{Required: true, MarkdownDescription: "The backend's IP address."},
									"port":             schema.Int64Attribute{Required: true, MarkdownDescription: "The backend's DNS port, 1 to 65535."},
									"rndc_host":        schema.StringAttribute{Optional: true, MarkdownDescription: "bind9: the address rndc connects to. Designate defaults to `127.0.0.1`."},
									"rndc_port":        schema.Int64Attribute{Optional: true, MarkdownDescription: "bind9: the rndc control port. Designate defaults to 953."},
									"rndc_key_file":    schema.StringAttribute{Optional: true, MarkdownDescription: "bind9: the path, on the DNS host, of the rndc key file. One of `rndc_key_file` and `rndc_config_file` is required."},
									"rndc_config_file": schema.StringAttribute{Optional: true, MarkdownDescription: "bind9: the path, on the DNS host, of an rndc configuration file that carries the key."},
									"api_endpoint":     schema.StringAttribute{Optional: true, MarkdownDescription: "pdns4: the PowerDNS API URL."},
									"api_token":        schema.StringAttribute{Optional: true, Sensitive: true, MarkdownDescription: "pdns4: the PowerDNS API key."},
								},
							},
						}},
					},
				}},
			},
		},
	}
}

func (d *poolsConfigDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var data poolsConfigModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}
	pools, diags := decodePools(ctx, data.Pools)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	errs, warns := validatePools(pools)
	for _, w := range warns {
		resp.Diagnostics.AddAttributeWarning(path.Root("pools"), "Pool configuration will fail on this PCD release", w.Path+": "+w.Msg)
	}
	for _, e := range errs {
		resp.Diagnostics.AddAttributeError(path.Root("pools"), "Invalid pool configuration", e.Path+": "+e.Msg)
	}
	if resp.Diagnostics.HasError() {
		return
	}
	out, err := renderPoolsYAML(pools)
	if err != nil {
		resp.Diagnostics.AddError("dns: rendering pools.yaml", err.Error())
		return
	}
	sum := sha256.Sum256([]byte(out))
	data.YAML = types.StringValue(out)
	data.ID = types.StringValue(hex.EncodeToString(sum[:]))
	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}

// decodePools converts the framework values into plain pool configurations.
// Every level is decoded through the framework types so null optional blocks
// (also_notifies, rndc_* on a pdns4 target) come through as zero values.
func decodePools(ctx context.Context, list types.List) ([]poolConfig, diag.Diagnostics) {
	var diags diag.Diagnostics
	var pools []poolModel
	diags.Append(list.ElementsAs(ctx, &pools, false)...)
	if diags.HasError() {
		return nil, diags
	}
	out := make([]poolConfig, 0, len(pools))
	for _, p := range pools {
		pc := poolConfig{
			Name:        p.Name.ValueString(),
			ID:          p.ID.ValueString(),
			Description: p.Description.ValueString(),
			Attributes:  map[string]string{},
		}
		if !p.Attributes.IsNull() && !p.Attributes.IsUnknown() {
			diags.Append(p.Attributes.ElementsAs(ctx, &pc.Attributes, false)...)
		}
		var records []nsRecordModel
		diags.Append(p.NSRecords.ElementsAs(ctx, &records, false)...)
		for _, r := range records {
			pc.NSRecords = append(pc.NSRecords, nsRecord{Hostname: r.Hostname.ValueString(), Priority: int(r.Priority.ValueInt64())})
		}
		pc.Nameservers = hostPorts(ctx, p.Nameservers, &diags)
		pc.AlsoNotifies = hostPorts(ctx, p.AlsoNotifies, &diags)
		var targets []targetModel
		diags.Append(p.Targets.ElementsAs(ctx, &targets, false)...)
		for _, t := range targets {
			var o targetOptionsModel
			diags.Append(t.Options.As(ctx, &o, basetypes.ObjectAsOptions{})...)
			pc.Targets = append(pc.Targets, poolTarget{
				Type:        t.Type.ValueString(),
				Description: t.Description.ValueString(),
				Masters:     hostPorts(ctx, t.Masters, &diags),
				Options: targetOptions{
					Host:           o.Host.ValueString(),
					Port:           int(o.Port.ValueInt64()),
					RNDCHost:       o.RNDCHost.ValueString(),
					RNDCPort:       int(o.RNDCPort.ValueInt64()),
					RNDCKeyFile:    o.RNDCKeyFile.ValueString(),
					RNDCConfigFile: o.RNDCConfigFile.ValueString(),
					APIEndpoint:    o.APIEndpoint.ValueString(),
					APIToken:       o.APIToken.ValueString(),
				},
			})
		}
		out = append(out, pc)
	}
	return out, diags
}

func hostPorts(ctx context.Context, l types.List, diags *diag.Diagnostics) []hostPort {
	if l.IsNull() || l.IsUnknown() {
		return nil
	}
	var models []hostPortModel
	diags.Append(l.ElementsAs(ctx, &models, false)...)
	out := make([]hostPort, 0, len(models))
	for _, m := range models {
		out = append(out, hostPort{Host: m.Host.ValueString(), Port: int(m.Port.ValueInt64())})
	}
	return out
}

// poolIssue is one thing wrong with the configuration: where, and what.
type poolIssue struct {
	Path, Msg string
}

var poolTargetTypes = map[string]bool{"bind9": true, "pdns4": true}

// zoneMasterHostMax is the width of Designate's zone_masters.host column
// (String(32) upstream through at least 2024.1, the release PCD 2026.4 ships).
// designate-manage pool update copies every target master into every existing
// zone's masters, so a longer literal fails with "Data too long for column
// 'host'" (PCD-9946). The pool tables themselves are 255 wide, which is why
// the same file applies cleanly until the first zone exists.
const zoneMasterHostMax = 32

// validatePools checks what designate-manage would otherwise reject at apply
// time, or accept and then break on. Errors are malformed input; warnings are
// values that are valid YAML but known to fail on the PCD release this
// provider targets.
func validatePools(pools []poolConfig) (errs, warns []poolIssue) {
	if len(pools) == 0 {
		return []poolIssue{{"pools", "at least one pool is required"}}, nil
	}
	names := map[string]int{}
	for i, p := range pools {
		at := fmt.Sprintf("pools[%d]", i)
		if p.Name == "" {
			errs = append(errs, poolIssue{at + ".name", "name is required"})
		} else if j, dup := names[p.Name]; dup {
			errs = append(errs, poolIssue{at + ".name", fmt.Sprintf("duplicates pools[%d].name %q; designate-manage matches pools by name", j, p.Name)})
		}
		names[p.Name] = i
		if len(p.NSRecords) == 0 {
			errs = append(errs, poolIssue{at + ".ns_records", "at least one NS record is required; Designate refuses to create zones in a pool without one (no_servers_configured)"})
		}
		for j, r := range p.NSRecords {
			rat := fmt.Sprintf("%s.ns_records[%d]", at, j)
			if !strings.HasSuffix(r.Hostname, ".") || len(r.Hostname) < 2 {
				errs = append(errs, poolIssue{rat + ".hostname", fmt.Sprintf("%q must be a fully qualified name ending in a dot", r.Hostname)})
			}
			if r.Priority < 1 {
				errs = append(errs, poolIssue{rat + ".priority", "must be 1 or greater"})
			}
		}
		if len(p.Nameservers) == 0 {
			errs = append(errs, poolIssue{at + ".nameservers", "at least one nameserver is required"})
		}
		errs = append(errs, checkHostPorts(at+".nameservers", p.Nameservers)...)
		errs = append(errs, checkHostPorts(at+".also_notifies", p.AlsoNotifies)...)
		if len(p.Targets) == 0 {
			errs = append(errs, poolIssue{at + ".targets", "at least one target is required"})
		}
		for j, t := range p.Targets {
			tat := fmt.Sprintf("%s.targets[%d]", at, j)
			if !poolTargetTypes[t.Type] {
				errs = append(errs, poolIssue{tat + ".type", fmt.Sprintf("%q is not supported; use bind9 or pdns4", t.Type)})
			}
			if len(t.Masters) == 0 {
				errs = append(errs, poolIssue{tat + ".masters", "at least one master (the address of a host with the dns role, port 5354) is required"})
			}
			errs = append(errs, checkHostPorts(tat+".masters", t.Masters)...)
			for k, m := range t.Masters {
				if len(m.Host) > zoneMasterHostMax {
					warns = append(warns, poolIssue{fmt.Sprintf("%s.masters[%d].host", tat, k), fmt.Sprintf(
						"%q is %d characters, and Designate stores zone masters in a %d-character column: designate-manage pool update "+
							"fails with \"Data too long for column 'host'\" as soon as the pool has a zone (PCD-9946). Give designate-mdns a "+
							"shorter static address, for example one with a compressible run of zeros.", m.Host, len(m.Host), zoneMasterHostMax)})
				}
			}
			o, oat := t.Options, tat+".options"
			if net.ParseIP(o.Host) == nil {
				errs = append(errs, poolIssue{oat + ".host", fmt.Sprintf("%q is not an IP address", o.Host)})
			}
			if !validPort(o.Port) {
				errs = append(errs, poolIssue{oat + ".port", "must be between 1 and 65535"})
			}
			switch t.Type {
			case "bind9":
				// Designate defaults rndc_host (127.0.0.1) and rndc_port (953) and
				// passes -k or -c to rndc only for the file options that are set.
				if o.RNDCKeyFile == "" && o.RNDCConfigFile == "" {
					errs = append(errs, poolIssue{oat, "a bind9 target needs rndc_key_file or rndc_config_file"})
				}
				if o.RNDCPort != 0 && !validPort(o.RNDCPort) {
					errs = append(errs, poolIssue{oat + ".rndc_port", "must be between 1 and 65535"})
				}
				if o.APIEndpoint != "" || o.APIToken != "" {
					errs = append(errs, poolIssue{oat, "api_endpoint and api_token are pdns4 options; a bind9 target does not take them"})
				}
			case "pdns4":
				if o.APIEndpoint == "" || o.APIToken == "" {
					errs = append(errs, poolIssue{oat, "a pdns4 target needs api_endpoint and api_token"})
				}
				if o.RNDCHost != "" || o.RNDCPort != 0 || o.RNDCKeyFile != "" || o.RNDCConfigFile != "" {
					errs = append(errs, poolIssue{oat, "rndc_host, rndc_port, rndc_key_file and rndc_config_file are bind9 options; a pdns4 target does not take them"})
				}
			}
		}
	}
	return errs, warns
}

func checkHostPorts(at string, hps []hostPort) []poolIssue {
	var errs []poolIssue
	for i, hp := range hps {
		if net.ParseIP(hp.Host) == nil {
			errs = append(errs, poolIssue{fmt.Sprintf("%s[%d].host", at, i), fmt.Sprintf("%q is not an IP address", hp.Host)})
		}
		if !validPort(hp.Port) {
			errs = append(errs, poolIssue{fmt.Sprintf("%s[%d].port", at, i), "must be between 1 and 65535"})
		}
	}
	return errs
}

func validPort(p int) bool { return p >= 1 && p <= 65535 }

// renderPoolsYAML writes the pools the way designate-manage pool update reads
// them: one document holding a list of pools.
func renderPoolsYAML(pools []poolConfig) (string, error) {
	for i := range pools {
		if pools[i].Attributes == nil {
			pools[i].Attributes = map[string]string{}
		}
	}
	var b strings.Builder
	b.WriteString("---\n")
	enc := yaml.NewEncoder(&b)
	enc.SetIndent(2)
	if err := enc.Encode(pools); err != nil {
		return "", err
	}
	if err := enc.Close(); err != nil {
		return "", err
	}
	return b.String(), nil
}
```

Register it in `internal/provider/provider.go`, in `DataSources` after `dns.NewZoneDataSource`:
```go
		dns.NewPoolsConfigDataSource,
```

- [ ] **Step 4: Run the tests and the full checks**

Run: `go test ./internal/services/dns/ -run 'TestValidatePools|TestRenderPoolsYAML' -count=1 -v && gofmt -l . && go vet ./... && golangci-lint run ./... && go test ./internal/... -timeout 120s`
Expected: PASS, clean. If `TestRenderPoolsYAML` fails on the document prefix, print `out` and adjust only the test's expectation to yaml.v3's actual indentation; the parse-back assertions are the contract.

- [ ] **Step 5: Example and generated docs**

Create `examples/data-sources/pcd_dns_pools_config/data-source.tf`:

```hcl
# The pool file for a host carrying the dns cluster role, backed by BIND9 on
# the same host. designate-mdns (port 5354) on the DNS host is the master the
# BIND server transfers zones from; rndc lets Designate add and remove zones.
data "pcd_dns_pools_config" "default" {
  pools = [{
    name        = "default"
    description = "BIND9 on the DNS host"

    ns_records = [{
      hostname = "ns1.pcd.example.com."
      priority = 1
    }]

    nameservers = [{
      host = "10.0.0.5"
      port = 53
    }]

    targets = [{
      type = "bind9"
      masters = [{
        host = "10.0.0.5"
        port = 5354
      }]
      options = {
        host          = "10.0.0.5"
        port          = 53
        rndc_host     = "10.0.0.5"
        rndc_port     = 953
        rndc_key_file = "/etc/designate/rndc.key"
      }
    }]
  }]
}

# yaml is the file to place at /etc/designate/pools.yaml on the DNS host and
# apply with `designate-manage pool update --file /etc/designate/pools.yaml`;
# id changes with the content, so it can trigger the delivery. The DNS guide
# shows a complete delivery with the ssh provider.
output "pools_yaml" {
  value     = data.pcd_dns_pools_config.default.yaml
  sensitive = true
}
```

Run: `terraform fmt -recursive ./examples && make generate && git diff --stat docs/`
Expected: a new `docs/data-sources/dns_pools_config.md`.

- [ ] **Step 6: Commit**

```bash
git add internal/services/dns/pools_config_data_source.go internal/services/dns/pools_config_internal_test.go internal/provider/provider.go examples/data-sources/pcd_dns_pools_config/data-source.tf docs/
git commit -m "dns: pcd_dns_pools_config renders and validates pools.yaml (PCD-9943)"
```

---

### Task 4: DNS guide and runnable example (PCD-9943 delivery, PCD-9926/9945 end to end)

**Files:**
- Create: `templates/guides/dns.md.tmpl`
- Create: `examples/complete/dns/README.md`, `provider.tf`, `variables.tf`, `terraform.tfvars.example`, `role.tf`, `pool.tf`, `zone.tf`, `network.tf`, `app.tf`, `outputs.tf`
- Modify: `README.md` (one sentence pointing at the guide, next to the Community Edition guide link)

**Interfaces:**
- Consumes: `pcd_dns_pools_config.yaml` / `.id` (Task 3), `pcd_networking_network.dns_domain` (Task 1), `pcd_networking_subnet.dns_publish_fixed_ip` (Task 2). The `settings` block on the `dns` role is added to `role.tf` by Task 5.
- Produces: the configuration Task 8's end-to-end tier applies as is.

- [ ] **Step 1: The example**

`examples/complete/dns/provider.tf`:
```hcl
terraform {
  required_providers {
    pcd = {
      source  = "platform9/pcd"
      version = "~> 0.1"
    }
    # Copies pools.yaml to the DNS host and runs designate-manage there. PCD
    # has no API that carries a pool's targets, so the file has to travel.
    ssh = {
      source  = "loafoe/ssh"
      version = "~> 2.7"
    }
  }
}

# Credentials come from the pcdctl RC file: in the PCD UI open Settings > API
# Access, copy it, set OS_PASSWORD, and `source pcdctlrc` before running
# Terraform. The provider reads OS_AUTH_URL, OS_USERNAME, OS_PASSWORD,
# OS_PROJECT_NAME, and OS_REGION_NAME from the environment.
provider "pcd" {
  user_domain_id    = "default"
  project_domain_id = "default"

  # Community Edition ships a self-signed certificate.
  insecure = true
}
```

`examples/complete/dns/variables.tf`:
```hcl
variable "dns_host_id" {
  type        = string
  description = "The resource-manager UUID of the host that takes the dns role (pcdctl hypervisor show <id> -c service_host, or /etc/pf9/host_id.conf on the host)."
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

variable "bind_port" {
  type        = number
  description = "The port BIND listens on. 53 unless something else already owns it on the host."
  default     = 53
}

variable "rndc_key_file" {
  type        = string
  description = "Path, on the DNS host, of the rndc key the Designate worker can read."
  default     = "/etc/designate/rndc.key"
}

# PCD packages Designate in its own virtualenv with its own configuration
# file, outside sudo's default PATH, and runs it as the pf9 user. These
# defaults are what a 2026.4 host has; the DNS guide says how to confirm them.
variable "designate_manage" {
  type        = string
  description = "Path, on the DNS host, of the designate-manage binary."
  default     = "/opt/pf9/pf9-designate/bin/designate-manage"
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
  description = "The instance name; it becomes the hostname part of the DNS record."
  default     = "dns-demo"
}
```

`examples/complete/dns/terraform.tfvars.example`:
```hcl
dns_host_id      = "136fc11a-ec5a-4699-b097-f75796134f8d"
dns_host_ip      = "172.16.122.251"
dns_host_ssh_key = "~/.ssh/pcd_automation"
ns_hostname      = "ns1.pcd.local."
zone_name        = "app.pcd.local."
zone_email       = "dns-admin@pcd.local"
```

`examples/complete/dns/role.tf`:
```hcl
# The dns cluster role installs designate-worker and designate-mdns on the
# host. It converges in a few minutes; everything below needs it running, so
# the wait is on.
resource "pcd_host_cluster_role" "dns" {
  host_id              = var.dns_host_id
  role                 = "dns"
  wait_until_converged = true
}
```

`examples/complete/dns/pool.tf`:
```hcl
# The pool: what Designate needs to know about the BIND server it pushes zones
# to. pcd_dns_pools_config validates it at plan time and renders pools.yaml.
data "pcd_dns_pools_config" "default" {
  pools = [{
    name        = "default"
    description = "BIND9 on the DNS host"

    ns_records = [{
      hostname = var.ns_hostname
      priority = 1
    }]

    nameservers = [{
      host = var.dns_host_ip
      port = var.bind_port
    }]

    targets = [{
      type = "bind9"
      masters = [{
        host = var.dns_host_ip
        port = 5354
      }]
      options = {
        host          = var.dns_host_ip
        port          = var.bind_port
        rndc_host     = var.dns_host_ip
        rndc_port     = 953
        rndc_key_file = var.rndc_key_file
      }
    }]
  }]
}

# Delivery: copy the file to the DNS host and apply it. The trigger is the
# file's hash, so this re-runs exactly when the pool changes. Without --delete
# a pool removed from the configuration stays in Designate; delete it by hand.
resource "ssh_resource" "pools" {
  host        = var.dns_host_ip
  user        = var.dns_host_ssh_user
  private_key = file(var.dns_host_ssh_key)
  agent       = false
  timeout     = "5m"
  retry_delay = "10s"

  triggers = {
    pools = data.pcd_dns_pools_config.default.id
  }

  file {
    destination = "/tmp/pools.yaml"
    content     = data.pcd_dns_pools_config.default.yaml
    permissions = "0600"
  }

  # /etc/designate may not exist yet (-D creates it); the file is owned by the
  # Designate user because designate-manage runs as that user and reads its own
  # configuration file to reach the database.
  commands = [
    "sudo install -D -o ${var.designate_user} -m 0600 /tmp/pools.yaml /etc/designate/pools.yaml && rm -f /tmp/pools.yaml",
    "sudo -u ${var.designate_user} ${var.designate_manage} --config-file ${var.designate_conf} pool update --file /etc/designate/pools.yaml",
  ]

  depends_on = [pcd_host_cluster_role.dns]
}
```

`examples/complete/dns/zone.tf`:
```hcl
# The zone. Creating it needs the pool above: Designate schedules the zone
# onto the pool and pushes it to BIND before reporting ACTIVE.
resource "pcd_dns_zone" "app" {
  name  = var.zone_name
  email = var.zone_email
  ttl   = 300

  depends_on = [ssh_resource.pools]
}
```

`examples/complete/dns/network.tf`:
```hcl
# A tenant network bound to the zone, and a subnet that publishes its fixed
# IPs. Both are set before the instance exists: Neutron creates records when a
# port is created, never for ports that already exist.
resource "pcd_networking_network" "app" {
  name       = "dns-demo-net"
  dns_domain = pcd_dns_zone.app.name
}

resource "pcd_networking_subnet" "app" {
  network_id           = pcd_networking_network.app.id
  name                 = "dns-demo-subnet"
  cidr                 = var.network_cidr
  ip_version           = 4
  dns_publish_fixed_ip = true
}
```

`examples/complete/dns/app.tf`:
```hcl
data "pcd_images_image" "app" {
  name = var.image_name
}

data "pcd_compute_flavor" "app" {
  name = var.flavor_name
}

# The instance name becomes the port's dns_name, so the record is
# <instance_name>.<zone>. The subnet dependency is explicit: the port must be
# created after the subnet has dns_publish_fixed_ip set.
resource "pcd_compute_instance" "app" {
  name        = var.instance_name
  image_id    = data.pcd_images_image.app.id
  flavor_id   = data.pcd_compute_flavor.app.id

  network {
    uuid = pcd_networking_network.app.id
  }

  depends_on = [pcd_networking_subnet.app]
}
```
(Check the attribute names against `docs/resources/compute_instance.md` before committing: the CE example's `app.tf` is the reference for how this provider's instance takes its image, flavor and network.)

`examples/complete/dns/outputs.tf`:
```hcl
output "record_name" {
  description = "The name Neutron publishes for the instance; resolve it against the BIND server to confirm."
  value       = "${var.instance_name}.${var.zone_name}"
}

output "instance_ip" {
  value = pcd_compute_instance.app.access_ip_v4
}
```

`examples/complete/dns/README.md`: title, one paragraph of what it builds, "Prerequisites" (a region with a hypervisor, an image and a flavor; the CE example builds one; BIND9 and an rndc key on the DNS host, set up as the guide describes), "Run it" (the same `source pcdctlrc` block as the CE README, plus `dns_host_ssh_key`), "Confirm" (`pcdctl recordset list <zone>` shows an A record for the instance; `dig @<dns_host_ip> -p <bind_port> <instance>.<zone>`), "Take it down" (`terraform destroy`; the pool stays in Designate; the dns role deauthorizes, and a second destroy a few minutes later may be needed, as with the CE example).

- [ ] **Step 2: The guide**

Create `templates/guides/dns.md.tmpl` with this outline and prose (the implementer writes the paragraphs; every claim below is established above):

```markdown
---
page_title: "DNS: publish instance records with Designate - PCD Provider"
subcategory: ""
description: |-
  Assign the dns role, configure the Designate pool, bind a network to a zone, and have instances get DNS records when they boot.
---

# DNS: publish instance records with Designate

Intro: what PCD's DNS as a Service is (Designate; API/central on the management plane, worker and mdns on the host with the `dns` role, a BIND9 or PowerDNS backend you run), and what this guide builds: the files under `examples/complete/dns/`.

## How the pieces fit
- The pool: Designate's description of the backend. It lives in `pools.yaml` on the DNS host and is applied with `designate-manage pool update`. Designate's API shows a pool's name, description, attributes and NS records only, so Terraform cannot read the targets back: `pcd_dns_pools_config` validates and renders the file, and `ssh_resource` delivers it. Say plainly that `terraform plan` diffs the rendered file, not what Designate holds.
- The zone: `pcd_dns_zone`. Needs the pool first (otherwise `500 no_servers_configured`).
- The network: `dns_domain` names the zone; one zone per network; `""` removes it; a zone set outside Terraform must be added to the configuration before applying with this provider version, or the apply clears it.
- The subnet: `dns_publish_fixed_ip`. Neutron's rules: external networks publish nothing without it on at least one subnet; other networks use it as the per-subnet opt-in; provider networks that are not external publish directly; floating IPs publish on association. Records are written on port create/update, never retroactively.

## Prepare the DNS host
BIND9 install, `rndc-confgen`, the key copied to `/etc/designate/rndc.key` mode 0644, `named.conf.options` with `allow-new-zones yes`, listen on the host address, `recursion no`; check with `ss -lntup | grep ':53 '` which port is free (PCD's own `dnsmasq` owns 53 on the host address; then use 5353 and set `bind_port`). Embed the `named.conf.options` block from `~/Documents/PCD-CE/DNS-validation-lab-setup.md`, with the address as a placeholder. Say where `designate-manage` lives on a PCD host and how to confirm the three `designate_*` variables once the role is on: `sudo find /opt/pf9 -maxdepth 4 -name designate-manage -o -name designate.conf` and `systemctl cat 'pf9-designate*' | grep -E 'User=|ExecStart='`.

## The configuration
{{ codefile "hcl" "examples/complete/dns/terraform.tfvars.example" }}
{{ tffile "examples/complete/dns/provider.tf" }}
{{ tffile "examples/complete/dns/role.tf" }}
{{ tffile "examples/complete/dns/pool.tf" }}
{{ tffile "examples/complete/dns/zone.tf" }}
{{ tffile "examples/complete/dns/network.tf" }}
{{ tffile "examples/complete/dns/app.tf" }}
{{ tffile "examples/complete/dns/outputs.tf" }}

## Confirm it worked
`pcdctl recordset list <zone>` / `openstack recordset list`, and `dig`.

## Remove the association
Omit `dns_domain` (or set `dns_publish_fixed_ip = false`). Existing records stay until the port goes away.

## IPv6
- `designate-mdns` listens on `0.0.0.0:5354` by default. To serve IPv6 backends, set `settings = { listen = "[::]:5354" }` on the `dns` role (added by Task 5; the guide names it once Task 5 lands) and make sure `net.ipv6.bindv6only` is `0` on the host (the Linux default).
- Master addresses longer than 32 characters break `designate-manage pool update` once a zone exists (Designate's `zone_masters.host` is 32 wide; PCD-9946). Use a short static address on the DNS host. The data source warns about this.

## Upgrades and limits
- PCD's Designate chart carries a placeholder pool; a PCD upgrade may re-run it. Keep the configuration and re-apply after an upgrade if `pcdctl dns pool list` (or `GET /designate/v2/pools`) shows the NS records reverted.
- `designate-manage pool update --delete` removes pools missing from the file; the example does not pass it.
```

- [ ] **Step 3: README pointer and generated docs**

In `README.md`, after the sentence about the Importing guide, add: "The [DNS guide](https://registry.terraform.io/providers/platform9/pcd/latest/docs/guides/dns) assigns the `dns` role, configures the Designate pool, and binds a network to a zone so instances get records when they boot."

Build the dev provider and the override file first (Task 8 reuses both), then validate the example against it; a bare `terraform init` would install the released provider, which has none of the new attributes, and `validate` would fail on `dns_domain`:

```bash
mkdir -p <scratchpad>/bin && go build -o <scratchpad>/bin/terraform-provider-pcd . && printf 'provider_installation {\n  dev_overrides { "platform9/pcd" = "%s" }\n  direct {}\n}\n' "<scratchpad>/bin" > <scratchpad>/dev.tfrc
terraform fmt -recursive ./examples && make generate && git diff --stat docs/
cd examples/complete/dns && TF_CLI_CONFIG_FILE=<scratchpad>/dev.tfrc terraform init -backend=false && TF_CLI_CONFIG_FILE=<scratchpad>/dev.tfrc terraform validate; cd -
```
Expected: `docs/guides/dns.md` appears; `init` warns that `platform9/pcd` is overridden and installs `loafoe/ssh`; `validate` reports `Success!`. Both Terraform commands need the registry for the ssh provider; without network access, run `terraform fmt -check -recursive ./examples` alone and say so in the execution notes.

- [ ] **Step 4: Commit**

```bash
git add templates/guides/dns.md.tmpl examples/complete/dns README.md docs/
git commit -m "docs: DNS guide and a runnable Designate example (PCD-9943, PCD-9926, PCD-9945)"
```

---

### Task 5: `settings` on `pcd_host_cluster_role` for the `dns` role (PCD-9948)

**Files:**
- Modify: `internal/services/resmgr/host_cluster_role_resource.go`
- Modify: `internal/services/resmgr/host_cluster_role_internal_test.go`
- Modify: `internal/services/resmgr/resmgr_test.go`
- Modify: `examples/resources/pcd_host_cluster_role/resource.tf`
- Modify: `examples/complete/dns/role.tf` (after Task 4)

**Interfaces:**
- Consumes: `getJSON`, `putJSON`, `isNotFound` (`resmgr.go`), `clusterRoleMarkers`, `roleOptionsChanged`.
- Produces: `func settingsChanged(plan, state *hostClusterRoleModel) bool`, `func mergeSettings(current map[string]any, managed map[string]string) map[string]any`, `func readSettings(current map[string]any, managed map[string]string) map[string]string`, `func settingString(v any) string`, `func (r *hostClusterRoleResource) applySettings(ctx, hostID, role string, managed map[string]string) error`, `func waitRoleSettings(ctx, client, url) (map[string]any, error)`, and the `settings` attribute.

Assumptions Task 0 checks (adapt if the probe disagrees): `GET /resmgr/v1/hosts/<id>/roles/pf9-designate` returns the role's settings as a JSON object; `PUT` with a full settings object replaces it (so the body is the merge of what resmgr holds and the managed keys); the settings include `listen` (default `0.0.0.0:5354`) and `debug`.

- [ ] **Step 1: Write the failing unit tests**

Append to `internal/services/resmgr/host_cluster_role_internal_test.go`:

```go
func settingsMap(kv ...string) types.Map {
	elems := map[string]attr.Value{}
	for i := 0; i+1 < len(kv); i += 2 {
		elems[kv[i]] = types.StringValue(kv[i+1])
	}
	return types.MapValueMust(types.StringType, elems)
}

// settings is written through resmgr v1, separately from the v2 assignment,
// so a change to it must trigger that write and nothing else.
func TestSettingsChanged(t *testing.T) {
	base := func(s types.Map) *hostClusterRoleModel {
		m := roleModel(types.StringNull(), types.ListNull(types.StringType))
		m.Role = types.StringValue("dns")
		m.Settings = s
		return m
	}
	for _, tc := range []struct {
		name        string
		plan, state types.Map
		want        bool
	}{
		{name: "both unset", plan: types.MapNull(types.StringType), state: types.MapNull(types.StringType)},
		{name: "same", plan: settingsMap("listen", "[::]:5354"), state: settingsMap("listen", "[::]:5354")},
		{name: "set", plan: settingsMap("listen", "[::]:5354"), state: types.MapNull(types.StringType), want: true},
		{name: "value changed", plan: settingsMap("listen", "[::]:5354"), state: settingsMap("listen", "0.0.0.0:5354"), want: true},
		{name: "key added", plan: settingsMap("listen", "[::]:5354", "debug", "True"), state: settingsMap("listen", "[::]:5354"), want: true},
		{name: "removed entirely", plan: types.MapNull(types.StringType), state: settingsMap("listen", "[::]:5354"), want: true},
		{name: "unknown plan", plan: types.MapUnknown(types.StringType), state: types.MapNull(types.StringType)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := settingsChanged(base(tc.plan), base(tc.state)); got != tc.want {
				t.Fatalf("settingsChanged = %v, want %v", got, tc.want)
			}
		})
	}
	// settings never makes the v2 assignment look changed.
	if roleOptionsChanged(base(settingsMap("listen", "[::]:5354")), base(types.MapNull(types.StringType))) {
		t.Fatalf("roleOptionsChanged = true for a settings-only change; the cluster role would be re-PUT")
	}
}

// The v1 PUT replaces the whole settings object, and the uber-role expansion
// computed most of it; the body must carry everything resmgr holds with only
// the managed keys overwritten.
func TestMergeSettings(t *testing.T) {
	current := map[string]any{"listen": "0.0.0.0:5354", "debug": "False", "db_host": "10.0.0.1", "workers": float64(2)}
	got := mergeSettings(current, map[string]string{"listen": "[::]:5354"})
	if got["listen"] != "[::]:5354" {
		t.Fatalf("listen not overwritten: %v", got)
	}
	if got["debug"] != "False" || got["db_host"] != "10.0.0.1" || got["workers"] != float64(2) {
		t.Fatalf("unmanaged settings not preserved; designate would lose its configuration: %v", got)
	}
	if current["listen"] != "0.0.0.0:5354" {
		t.Fatalf("mergeSettings mutated its input")
	}
}

// State holds exactly the managed keys, as strings, so plan compares like
// with like; a managed key resmgr dropped is absent, which plans a re-apply.
func TestReadSettings(t *testing.T) {
	current := map[string]any{"listen": "[::]:5354", "debug": true, "workers": float64(2), "db_host": "10.0.0.1"}
	got := readSettings(current, map[string]string{"listen": "[::]:5354", "debug": "true", "workers": "2", "gone": "x"})
	want := map[string]string{"listen": "[::]:5354", "debug": "true", "workers": "2"}
	if len(got) != len(want) {
		t.Fatalf("readSettings = %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("readSettings[%s] = %q, want %q", k, got[k], v)
		}
	}
	if _, ok := got["db_host"]; ok {
		t.Fatalf("an unmanaged key leaked into state; every plan would show it as a change")
	}
}

func TestSettingString(t *testing.T) {
	for _, tc := range []struct {
		in   any
		want string
	}{
		{"[::]:5354", "[::]:5354"}, {true, "true"}, {float64(5354), "5354"}, {float64(1.5), "1.5"}, {nil, ""},
		{[]any{"a"}, `["a"]`},
	} {
		if got := settingString(tc.in); got != tc.want {
			t.Errorf("settingString(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
```
Add `"github.com/hashicorp/terraform-plugin-framework/attr"` to the test file's imports if it is not already there (it is, for `backendsList`).

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/services/resmgr/ -run 'TestSettingsChanged|TestMergeSettings|TestReadSettings|TestSettingString' -count=1`
Expected: build failure, `m.Settings undefined`.

- [ ] **Step 3: Implement**

First, in `internal/services/resmgr/host_cluster_role_internal_test.go`, change the existing `roleModel` helper so the model it builds carries a typed null for the new attribute:

```go
	return &hostClusterRoleModel{
		ID:          types.StringValue("host-a/hypervisor"),
		HostID:      types.StringValue("host-a"),
		Role:        types.StringValue("hypervisor"),
		HostCluster: hostCluster,
		Backends:    backends,
		Settings:    types.MapNull(types.StringType),
	}
```
Without this, `TestUpdateSkipsResmgrForClientSideChanges` fails at `roleState`'s `st.Set`: the zero value of `types.Map` is null with no element type, and `tfsdk.State.Set` rejects it against the schema (`types.MapType[!!! MISSING TYPE !!!]`). The review panel reproduced this; it compiles and does not run.

Then, in `internal/services/resmgr/host_cluster_role_resource.go`:

Add imports `"encoding/json"`, `"strconv"`, and `"github.com/hashicorp/terraform-plugin-framework/diag"`.

Add to `hostClusterRoleModel`:
```go
	Settings           types.Map    `tfsdk:"settings"`
```

Add to the schema after `host_cluster`:
```go
			"settings": schema.MapAttribute{Optional: true, ElementType: types.StringType,
				MarkdownDescription: "For `dns` only: overrides for the settings of the granular `pf9-designate` role the cluster " +
					"role expands into, written through the resource-manager v1 role API once the cluster role is assigned " +
					"and again whenever it is re-assigned. Keys are the role's setting names as the PCD API reports them; " +
					"`listen = \"[::]:5354\"` makes designate-mdns serve zone transfers over IPv6 as well as IPv4 (the host's " +
					"`net.ipv6.bindv6only` must be `0`, the Linux default). Only the keys listed here are managed: the rest " +
					"keep the values PCD computes, and a key removed from this map keeps its last value until set again. " +
					"The write happens after the host converges when `wait_until_converged` is set, and otherwise retries " +
					"while the resource manager refuses role changes during convergence; the host agent then restarts " +
					"designate-mdns, which takes a few minutes more."},
```

Add to `ValidateConfig`, after the `host_cluster` check:
```go
	if !cfg.Settings.IsNull() && role != "dns" && !cfg.Role.IsUnknown() {
		resp.Diagnostics.AddAttributeError(path.Root("settings"), "settings requires role = \"dns\"",
			fmt.Sprintf("settings overrides pf9-designate's role settings and does not apply to %q.", role))
	}
```

Add after `backendsOption`:

```go
// settingsOption is settings as the resource manages them: null and unknown
// both mean "nothing managed".
func settingsOption(m *hostClusterRoleModel) types.Map {
	if m.Settings.IsNull() || m.Settings.IsUnknown() {
		return types.MapNull(types.StringType)
	}
	return m.Settings
}

// settingsChanged reports whether the managed settings differ between plan
// and state. They are written through resmgr v1, apart from the v2 assignment
// roleOptionsChanged guards, so the two are kept separate: a settings-only
// change must not re-PUT the cluster role.
func settingsChanged(plan, state *hostClusterRoleModel) bool {
	return !settingsOption(plan).Equal(settingsOption(state))
}

// managedSettings is the plan's settings as a Go map; nil when none.
func managedSettings(ctx context.Context, m *hostClusterRoleModel, diags *diag.Diagnostics) map[string]string {
	if m.Settings.IsNull() || m.Settings.IsUnknown() {
		return nil
	}
	out := map[string]string{}
	diags.Append(m.Settings.ElementsAs(ctx, &out, false)...)
	return out
}

// mergeSettings returns the body for a v1 role PUT: everything resmgr holds
// with the managed keys overwritten. The PUT replaces the whole settings
// object, and the uber-role expansion computed most of it (database,
// transport, credentials), so a body holding only the overrides would wipe it.
func mergeSettings(current map[string]any, managed map[string]string) map[string]any {
	out := make(map[string]any, len(current)+len(managed))
	for k, v := range current {
		out[k] = v
	}
	for k, v := range managed {
		out[k] = v
	}
	return out
}

// readSettings narrows the role's current settings to the managed keys, as
// strings, so state compares against exactly what the configuration set. A
// managed key resmgr no longer reports is left out, which plans a re-apply.
func readSettings(current map[string]any, managed map[string]string) map[string]string {
	out := make(map[string]string, len(managed))
	for k := range managed {
		if v, ok := current[k]; ok {
			out[k] = settingString(v)
		}
	}
	return out
}

// settingString renders a JSON scalar the way a user writes it in HCL.
func settingString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case bool:
		return strconv.FormatBool(t)
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case nil:
		return ""
	default:
		b, _ := json.Marshal(t)
		return string(b)
	}
}

// waitRoleSettings polls the v1 per-host role until resmgr reports it, and
// returns its settings. The v2 assignment expands into granular roles on the
// server, but the v1 view can lag; a PUT before the role shows up would
// assign the granular role directly, with only these settings.
func waitRoleSettings(ctx context.Context, client *gophercloud.ServiceClient, url string) (map[string]any, error) {
	deadline := time.Now().Add(2 * time.Minute)
	for {
		var current map[string]any
		err := getJSON(ctx, client, url, &current)
		if err == nil {
			return current, nil
		}
		if !isNotFound(err) {
			return nil, err
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("resmgr did not report the granular role at %s within 2 minutes of the cluster role being assigned", url)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
}

// applySettings writes the managed settings onto the cluster role's granular
// marker role through resmgr v1, merged over what resmgr currently holds. The
// write goes through putRole, which retries 409 RoleUpdateConflict for up to
// ten minutes: resmgr refuses role writes while the host converges, and the
// dns role converges for several minutes after it is assigned. Callers that
// wait for convergence call this afterward, so the retry is the fallback for
// callers that do not.
func (r *hostClusterRoleResource) applySettings(ctx context.Context, hostID, role string, managed map[string]string) error {
	if len(managed) == 0 {
		return nil
	}
	clientV1, err := r.config.ResmgrV1Client()
	if err != nil {
		return err
	}
	url := clientV1.ServiceURL("hosts", hostID, "roles", clusterRoleMarkers[role])
	current, err := waitRoleSettings(ctx, clientV1, url)
	if err != nil {
		return err
	}
	return r.putRole(ctx, clientV1, url, mergeSettings(current, managed))
}
```

In `Create`, replace everything after `plan.ID = types.StringValue(hostID + "/" + role)` (the `wait_until_converged` block and the final `State.Set`) with:
```go
	managed := managedSettings(ctx, &plan, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}
	if plan.WaitUntilConverged.ValueBool() {
		if err := r.waitConverged(ctx, hostID, role); err != nil {
			// The assignment itself succeeded: keep the resource in state so a
			// re-apply retries the wait instead of duplicating the assignment.
			// Settings were not written yet, so they stay unset in state too.
			plan.Settings = types.MapNull(types.StringType)
			resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
			resp.Diagnostics.AddError("resmgr: waiting for host convergence", err.Error())
			return
		}
	}
	// Settings go on last: after convergence when the caller waits for it,
	// and otherwise through the 409-retrying PUT, since resmgr refuses role
	// writes while the host converges.
	if err := r.applySettings(ctx, hostID, role, managed); err != nil {
		// The assignment succeeded; keep it in state with settings unset so the
		// next apply retries the settings write rather than the assignment.
		plan.Settings = types.MapNull(types.StringType)
		resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
		resp.Diagnostics.AddError("resmgr: applying role settings", err.Error())
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
```

In `Read`, after the membership check sets `found` and before `state.ID = ...`:
```go
	if managed := managedSettings(ctx, &state, &resp.Diagnostics); len(managed) > 0 {
		clientV1, err := r.config.ResmgrV1Client()
		if err != nil {
			resp.Diagnostics.AddError("resmgr: building client", err.Error())
			return
		}
		var current map[string]any
		err = getJSON(ctx, clientV1, clientV1.ServiceURL("hosts", state.HostID.ValueString(), "roles", clusterRoleMarkers[state.Role.ValueString()]), &current)
		switch {
		case isNotFound(err):
			// The granular role is not visible (mid-expansion or the deauth
			// window); keep the last known settings rather than plan a rewrite.
		case err != nil:
			resp.Diagnostics.AddError("resmgr: reading role settings", err.Error())
			return
		default:
			m, d := types.MapValueFrom(ctx, types.StringType, readSettings(current, managed))
			resp.Diagnostics.Append(d...)
			state.Settings = m
		}
	}
```

In `Update`, replace the short-circuit and the body with:
```go
	optionsChanged := roleOptionsChanged(&plan, &state)
	if !optionsChanged && !settingsChanged(&plan, &state) {
		plan.ID = types.StringValue(plan.HostID.ValueString() + "/" + plan.Role.ValueString())
		resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
		return
	}

	hostID, role := plan.HostID.ValueString(), plan.Role.ValueString()
	if optionsChanged {
		client, err := r.config.ResmgrV2Client()
		if err != nil {
			resp.Diagnostics.AddError("resmgr: building client", err.Error())
			return
		}
		body := map[string]any{}
		if role == "hypervisor" && !plan.HostCluster.IsNull() && plan.HostCluster.ValueString() != "" {
			body["hostcluster"] = plan.HostCluster.ValueString()
		}
		if role == "persistent-storage" && !plan.Backends.IsNull() && !plan.Backends.IsUnknown() {
			var backends []string
			resp.Diagnostics.Append(plan.Backends.ElementsAs(ctx, &backends, false)...)
			body["backends"] = backends
		}
		if resp.Diagnostics.HasError() {
			return
		}
		if err := r.putRole(ctx, client, client.ServiceURL("hosts", hostID, "roles", role), body); err != nil {
			resp.Diagnostics.AddError("resmgr: updating cluster role", err.Error())
			return
		}
	}
	plan.ID = types.StringValue(hostID + "/" + role)

	if plan.WaitUntilConverged.ValueBool() && optionsChanged {
		if err := r.waitConverged(ctx, hostID, role); err != nil {
			plan.Settings = state.Settings
			resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
			resp.Diagnostics.AddError("resmgr: waiting for host convergence", err.Error())
			return
		}
	}
	// Settings go on last, after any re-assignment has converged: the
	// expansion may have reset them, and resmgr refuses the write meanwhile.
	managed := managedSettings(ctx, &plan, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}
	if err := r.applySettings(ctx, hostID, role, managed); err != nil {
		plan.Settings = state.Settings
		resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
		resp.Diagnostics.AddError("resmgr: applying role settings", err.Error())
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
```

Update the `roleOptionsChanged` doc comment's last sentence to: "A new server-side option sent on the v2 PUT must be added here too, or changes to it would be skipped; `settings` is written through v1 and has its own check, `settingsChanged`."

`ImportState` needs nothing: `settings` stays null on import (the provider cannot know which keys the user manages), which is documented by the schema text.

- [ ] **Step 4: Run the unit tests and the full checks**

Run: `go test ./internal/services/resmgr/ -count=1 && gofmt -l . && go vet ./... && golangci-lint run ./... && go test ./internal/... -timeout 120s`
Expected: PASS, clean. The existing `TestUpdate*` tests pass only because Step 3 changed `roleModel` in the same commit; `TestRoleOptionsChanged` and `TestImportState*` never route a model through `State.Set` and are unaffected either way.

- [ ] **Step 5: Acceptance test (opt-in, mutates the lab)**

Append to `internal/services/resmgr/resmgr_test.go`:

```go
// TestAccResmgrHostClusterRoleDNSSettings assigns the dns cluster role with a
// listen override, checks resmgr's pf9-designate settings carry it while the
// rest of the settings survive, imports (settings are not importable, by
// design), and removes the role. Opt-in: PCD_ACC_RESMGR=1 and PCD_ACC_HOST_ID.
// The role installs Designate services on the host and removing it
// deauthorizes them, so expect several minutes, and run it on a lab host.
func TestAccResmgrHostClusterRoleDNSSettings(t *testing.T) {
	if os.Getenv("PCD_ACC_RESMGR") == "" {
		t.Skip("PCD_ACC_RESMGR not set; skipping resmgr mutation test")
	}
	hostID := os.Getenv("PCD_ACC_HOST_ID")
	if hostID == "" {
		t.Skip("PCD_ACC_HOST_ID not set; skipping dns settings test")
	}
	const rn = "pcd_host_cluster_role.dns"
	cfg := func(listen string) string {
		return fmt.Sprintf(`
resource "pcd_host_cluster_role" "dns" {
  host_id = %q
  role    = "dns"
  settings = {
    listen = %q
  }
}
`, hostID, listen)
	}
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { acctest.PreCheck(t) },
		ProtoV6ProviderFactories: acctest.ProtoV6ProviderFactories,
		CheckDestroy:             testAccCheckHostClusterRoleDestroy(t, hostID, "dns"),
		Steps: []resource.TestStep{
			{
				Config: cfg("[::]:5354"),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(rn, "settings.listen", "[::]:5354"),
					testAccCheckRoleSetting(t, hostID, "pf9-designate", "listen", "[::]:5354"),
					testAccCheckRoleSettingsCount(t, hostID, "pf9-designate", 2),
				),
			},
			{
				Config: cfg("0.0.0.0:5354"),
				Check:  testAccCheckRoleSetting(t, hostID, "pf9-designate", "listen", "0.0.0.0:5354"),
			},
			{ResourceName: rn, ImportState: true, ImportStateId: hostID + "/dns", ImportStateVerify: true, ImportStateVerifyIgnore: []string{"settings"}},
		},
	})
}

// testAccCheckRoleSetting reads the granular role's settings through resmgr v1.
func testAccCheckRoleSetting(t *testing.T, hostID, role, key, want string) resource.TestCheckFunc {
	return func(_ *terraform.State) error {
		client, err := acctest.LabConfig(t).ResmgrV1Client()
		if err != nil {
			return err
		}
		var settings map[string]any
		if _, err := client.Get(context.Background(), client.ServiceURL("hosts", hostID, "roles", role), &settings, &gophercloud.RequestOpts{OkCodes: []int{200}}); err != nil {
			return fmt.Errorf("reading %s settings: %w", role, err)
		}
		if got := fmt.Sprint(settings[key]); got != want {
			return fmt.Errorf("%s.%s = %q, want %q (all: %v)", role, key, got, want, settings)
		}
		return nil
	}
}

// testAccCheckRoleSettingsCount guards the merge: a PUT that carried only the
// override would leave the role with one setting.
func testAccCheckRoleSettingsCount(t *testing.T, hostID, role string, atLeast int) resource.TestCheckFunc {
	return func(_ *terraform.State) error {
		client, err := acctest.LabConfig(t).ResmgrV1Client()
		if err != nil {
			return err
		}
		var settings map[string]any
		if _, err := client.Get(context.Background(), client.ServiceURL("hosts", hostID, "roles", role), &settings, &gophercloud.RequestOpts{OkCodes: []int{200}}); err != nil {
			return fmt.Errorf("reading %s settings: %w", role, err)
		}
		if len(settings) < atLeast {
			return fmt.Errorf("%s has %d settings after the override; the merge dropped what resmgr computed: %v", role, len(settings), settings)
		}
		return nil
	}
}
```
(If Task 0 shows the per-host role GET wraps the settings, adjust both helpers and `waitRoleSettings` together.)

Run it only in Task 8 tier 6, after the end-to-end configuration that owns the `dns` role has been destroyed: the test assigns the role itself and removes it in `CheckDestroy`, so running it while another state holds the role would strand that state.

- [ ] **Step 6: Examples and generated docs**

Append to `examples/resources/pcd_host_cluster_role/resource.tf`:
```hcl
# DNS as a Service on a host: Designate's worker and mdns. settings overrides
# the pf9-designate role's settings after assignment; listen on [::]:5354
# serves zone transfers to IPv6 backends as well as IPv4 ones.
resource "pcd_host_cluster_role" "dns" {
  host_id = "04575315-80ce-4617-9b96-6611d00c9942"
  role    = "dns"
  settings = {
    listen = "[::]:5354"
  }
}
```
In `examples/complete/dns/role.tf`, add a commented `settings` block with the same `listen` value and a comment "uncomment to serve IPv6 backends", and in `templates/guides/dns.md.tmpl` reference the attribute in the IPv6 section.

Run: `terraform fmt -recursive ./examples && make generate && git diff --stat docs/`
Expected: `docs/resources/host_cluster_role.md` and `docs/guides/dns.md` change.

- [ ] **Step 7: Commit**

```bash
git add internal/services/resmgr/host_cluster_role_resource.go internal/services/resmgr/host_cluster_role_internal_test.go internal/services/resmgr/resmgr_test.go examples/resources/pcd_host_cluster_role/resource.tf examples/complete/dns/role.tf templates/guides/dns.md.tmpl docs/
git commit -m "resmgr: settings on the dns cluster role for pf9-designate overrides (PCD-9948)"
```

---

### Task 6: Changelog, generated docs, PRs

**Files:**
- Modify: `CHANGELOG.md`

- [ ] **Step 1: Changelog**

Insert above `## [0.1.11] - 2026-09-04`:

```markdown
## [Unreleased]

### Added

- `pcd_networking_network` gains `dns_domain` (PCD-9926): the Designate zone that ports on the
  network publish records to. It defaults to `""`, so omitting it removes an association: add the
  attribute to the configuration of any network whose zone was set outside Terraform before the
  first apply with this version, or that apply clears it. The `pcd_networking_network` data source
  reports it.
- `pcd_networking_subnet` gains `dns_publish_fixed_ip` (PCD-9945), the per-subnet switch that
  publishes fixed IPs into the network's zone; a network with `external = true` publishes nothing
  until at least one subnet sets it. Defaults to `false`. The data source reports it.
- New `pcd_dns_pools_config` data source (PCD-9943): renders a Designate `pools.yaml` from typed
  attributes and validates it at plan time (NS names end in a dot, hosts are IP literals, ports are
  in range, bind9 and pdns4 options match the target type), replacing the page of variable
  validation a pool configuration otherwise needs. `yaml` is sensitive (pdns4 API tokens) and `id`
  changes with the content, so it can trigger delivery to the hosts that carry the `dns` role. It
  warns about master addresses longer than 32 characters, which Designate 2024.1 cannot store for
  zones (PCD-9946). PCD offers no API for pool targets, so the file still has to reach the host;
  the DNS guide shows a delivery.
- `pcd_host_cluster_role` gains `settings` for the `dns` role (PCD-9948): overrides for
  `pf9-designate`'s settings, such as `listen = "[::]:5354"` to serve zone transfers over IPv6,
  applied through the resource-manager v1 role API after the cluster role is assigned and again
  whenever it is re-assigned. Only the listed keys are managed.

### Documentation

- A [DNS guide](docs/guides/dns.md) and a runnable example under `examples/complete/dns/`: the
  `dns` role, a BIND9 pool rendered by `pcd_dns_pools_config` and delivered to the host, a zone, a
  network bound to it, a subnet that publishes, and an instance whose record appears in the zone.
  Validated end to end on a Community Edition 2026.4 lab.
```

Each PR carries only its own bullets; the second and third PRs merge `origin/main` and keep one `## [Unreleased]` heading (see the memory `release-from-worktree`).

- [ ] **Step 2: Final checks per PR**

Run: `gofmt -l . && go vet ./... && golangci-lint run ./... && go test ./internal/... -timeout 120s && make generate && git status --short docs/ && terraform fmt -check -recursive ./examples && goreleaser check`
Expected: all clean; `git status` shows no uncommitted generated docs.

- [ ] **Step 3: Branches and PRs**

```bash
git branch -m pushkar/networking-dns-attributes      # Tasks 1–2 (rename from claude/...)
git push -u origin pushkar/networking-dns-attributes
gh pr create --title "networking: dns_domain and dns_publish_fixed_ip (PCD-9926, PCD-9945)" --body-file <scratchpad>/pr-1.md
```
The PR body: what changed, the default-clears caveat, how it was tested (unit, acceptance, the lab tier from Task 8), no attribution footer. Repeat for `pushkar/dns-pools-config` (Tasks 3–4) and `pushkar/dns-role-settings` (Task 5), each branched from the previous once merged, or from `main` with a note that they stack.

---

### Task 7: Opus adversarial review panel

Run after Task 5 (whole branch) and again after the fixes it produces, until a round surfaces nothing new. The user has opted into multi-agent orchestration for this plan; the script goes to the Workflow tool inline.

**Files:**
- Create (scratch): `<scratchpad>/review.js`

- [ ] **Step 1: The workflow**

```js
export const meta = {
  name: 'dns-integration-review',
  description: 'Opus adversarial review of the DNS integration branch: six lenses find, three refuters vote, one synthesis',
  phases: [
    { title: 'Find', detail: 'one Opus reviewer per lens over the branch diff', model: 'opus' },
    { title: 'Refute', detail: 'three Opus skeptics per finding; majority refutation kills it', model: 'opus' },
    { title: 'Synthesize', detail: 'ranked report of what survived', model: 'opus' },
  ],
}

const RANGE = args?.range ?? 'origin/main...HEAD'
const CONTEXT = `You are reviewing the terraform-provider-pcd branch \`${RANGE}\` (run \`git diff ${RANGE}\` and read the touched files in full). The plan is docs/superpowers/plans/2026-09-17-dns-integration.md; its "Established facts" section is verified and you may rely on it. Report only defects you can point at with file:line and a concrete failure scenario. No style remarks.`

const LENSES = [
  { key: 'framework', prompt: `${CONTEXT}\nLens: terraform-plugin-framework semantics. Defaults on Optional+Computed attributes, UseStateForUnknown, null vs unknown vs "" handling, ValidateConfig with unknown values, ImportState and ImportStateVerify, refresh drift, state written on error paths, plan stability on the second apply. Reason through plan → apply → refresh → plan for each new attribute.` },
  { key: 'apis', prompt: `${CONTEXT}\nLens: API contracts. Neutron dns_domain / dns_publish_fixed_ip request and response shapes on a deployment with dns_domain_keywords; gophercloud extension wrapping order; resmgr v1 per-host role GET/PUT semantics assumed by settings (whole-object replace, merge, wait for the marker role); Designate pools.yaml as designate-manage pool update parses it (key names, types, id vs name matching).` },
  { key: 'tests', prompt: `${CONTEXT}\nLens: test adequacy. For each behavior the changelog claims, name the test that proves it or report the gap. Check the acceptance steps really exercise the change (a step that would pass on the old code is a gap). Check unit tests would fail on the bug they describe.` },
  { key: 'docs', prompt: `${CONTEXT}\nLens: documentation accuracy. Every sentence in schema descriptions, the DNS guide, examples, README and CHANGELOG: is it true of the code and of PCD 2026.4? Flag claims about Neutron's publishing rules, Designate's API, default values, and the clearing behavior that the code does not implement exactly as written.` },
  { key: 'secrets', prompt: `${CONTEXT}\nLens: secrets and safety. api_token and the rendered yaml must be sensitive everywhere they surface (state, plan output, diagnostics, logs, examples). The ssh delivery must not leave the token readable on the host or in Terraform logs. Nothing may log resmgr role settings that carry credentials.` },
  { key: 'jira-fit', prompt: `${CONTEXT}\nLens: acceptance criteria. Read the six tickets' acceptance criteria as quoted in the plan's Triage table. For each provider-side criterion, does the branch meet it exactly (attribute names, defaults, "empty string removes the association", "eliminate the variable validation", "documented path for overriding designate mdns configuration")? Report every deviation.` },
]

const FINDINGS = {
  type: 'object',
  properties: {
    findings: {
      type: 'array',
      items: {
        type: 'object',
        properties: {
          title: { type: 'string' },
          file: { type: 'string' },
          line: { type: 'integer' },
          severity: { type: 'string', enum: ['blocker', 'major', 'minor'] },
          claim: { type: 'string', description: 'the defect, one paragraph' },
          scenario: { type: 'string', description: 'inputs and state that show it' },
          fix: { type: 'string' },
        },
        required: ['title', 'file', 'severity', 'claim', 'scenario'],
      },
    },
  },
  required: ['findings'],
}

const VERDICT = {
  type: 'object',
  properties: { refuted: { type: 'boolean' }, reason: { type: 'string' } },
  required: ['refuted', 'reason'],
}

const VIEWS = [
  'as the engineer who wrote the code: reproduce the scenario from the source, line by line',
  'as a Terraform user running plan, apply, plan again, import: does the scenario actually surface',
  'as the PCD API (Neutron, Designate, resmgr): would the request or response really behave as the finding assumes',
]

const results = await pipeline(
  LENSES,
  l => agent(l.prompt, { label: `find:${l.key}`, phase: 'Find', model: 'opus', effort: 'high', schema: FINDINGS }),
  (found, l) => parallel((found?.findings ?? []).map(f => () =>
    parallel(VIEWS.map(view => () =>
      agent(`${CONTEXT}\nA reviewer claims:\n${JSON.stringify(f, null, 2)}\n\nTry to refute it ${view}. Default to refuted=true unless you can confirm the defect from the code or the plan's established facts.`,
        { label: `refute:${l.key}:${f.file}`, phase: 'Refute', model: 'opus', effort: 'high', schema: VERDICT })))
      .then(votes => {
        const v = votes.filter(Boolean)
        return { ...f, lens: l.key, votes: v, survives: v.filter(x => !x.refuted).length >= 2 }
      }))),
)

const all = results.flat().filter(Boolean)
const confirmed = all.filter(f => f.survives)
log(`${all.length} findings, ${confirmed.length} survived refutation`)

const report = await agent(
  `${CONTEXT}\nThese findings survived a three-vote refutation:\n${JSON.stringify(confirmed, null, 2)}\n\nWrite the review report: findings ranked by severity, deduplicated, each with file:line, the scenario, and the fix; then a short "what was checked and found sound" paragraph per lens, and the list of refuted findings with one line each on why they fell.`,
  { label: 'synthesize', phase: 'Synthesize', model: 'opus', effort: 'high' })

return { confirmed, refuted: all.filter(f => !f.survives).map(f => ({ title: f.title, lens: f.lens })), report }
```

- [ ] **Step 2: Run it, fix, run again**

Invoke the Workflow tool with `scriptPath: <scratchpad>/review.js` and `args: { range: "origin/main...HEAD" }` (about 6 finders + up to 3 refuters per finding + 1 synthesis; expect 20 to 40 Opus agents per round). Fix every confirmed blocker and major in the code with a commit each; note refused minors in the PR body with the reason. Re-run until a round confirms nothing new (two consecutive empty rounds is the stop condition, per the loop-until-dry pattern).

---

### Task 8: CE lab test plan

Six tiers, cheapest first; each says what it needs, what it runs, what "pass" is, and how to clean up. Tiers 0 to 2 are safe on the standing region. Tiers 3 to 5 assign the `dns` role to hyp1 and install BIND9 on it: ask the user before starting tier 3, and tell them the lab keeps the `dns` role and BIND9 until tier 6's cleanup.

**Preconditions (once):**
- Task 0 done and recorded; Keystone answering 200.
- `cp ~/Documents/PCD-CE/ce-run-2026-09-04/lab_env.py <scratchpad>/lab_env.py` (the runner that puts `OS_*` in the environment through the testsuite loader).
- Dev build: `<scratchpad>/bin/terraform-provider-pcd` and `<scratchpad>/dev.tfrc` from Task 4 Step 3, rebuilt from the branch head (`go build -o <scratchpad>/bin/terraform-provider-pcd .`) before each tier.
- Host id: `HYP1=$(grep ^HYP1_ID "<testsuite>/lab.env" | cut -d= -f2)` (136fc11a-… on the current lab).
- Lab quirks to expect (memories): resmgr answers 404 for per-host endpoints for minutes after a role is removed while the host list still shows the host; `pf9-hostagent` can hit systemd's start limit after a burst of restarts (reset-failed + start, not a rebuild); Glance is reachable from the Mac only through the 9494 tunnel; after removing `persistent-storage`, cinder-volume needs `reset.preenable_cinder`.

**Tier 0: Read-only probes (Task 0).** Pass: the five recorded answers, no surprises.

**Tier 1: Unit (no lab).**
Run: `go test ./internal/... -count=1 -timeout 120s -v -run 'TestNetwork|TestSubnet|TestInvalidDNSDomain|TestValidatePools|TestRenderPoolsYAML|TestSettings|TestMergeSettings|TestReadSettings|TestSettingString|TestRoleOptionsChanged|TestUpdate|TestImportState'`
Pass: every listed test runs and passes; `go test ./internal/...` overall green; `golangci-lint` 0 issues.

**Tier 2: Acceptance, attribute round-trips (lab, no DNS backend needed, no role changes).**
Run, one at a time so failures are attributable:
```bash
python3 <scratchpad>/lab_env.py -C "$(pwd)" env TF_ACC=1 go test ./internal/services/networking/ -run TestAccNetworkingNetworkDNSDomain -v -count=1 -timeout 20m
python3 <scratchpad>/lab_env.py -C "$(pwd)" env TF_ACC=1 go test ./internal/services/networking/ -run TestAccNetworkingSubnetDNSPublishFixedIP -v -count=1 -timeout 20m
python3 <scratchpad>/lab_env.py -C "$(pwd)" env TF_ACC=1 go test ./internal/services/networking/ -run 'TestAccNetworkingNetworkAndSubnet_basic|TestAccNetworkingDataSources_byName' -v -count=1 -timeout 30m
```
Pass: all PASS; `python3 run.py api leftovers` (testsuite) reports nothing named `tf-acc-`. Also apply the data source alone: a scratch directory with `examples/data-sources/pcd_dns_pools_config/data-source.tf` and `provider.tf` from the testsuite's `cases/`, run through `lab_env.py` with `TF_CLI_CONFIG_FILE=<scratchpad>/dev.tfrc`; `terraform plan` shows the sensitive output, `terraform apply` prints the hash; then change one port to 70000 and confirm `terraform plan` fails with the path `pools[0].nameservers[0].port`; set a 38-character IPv6 master and confirm the warning names PCD-9946.

**Tier 3: End to end (the `dns` role, BIND9, the example; needs the user's go-ahead).**
1. Prepare hyp1 as the guide says: `ssh pk@cannon-ubuntu 'ssh -i ~/.ssh/pcd_automation ubuntu@172.16.122.251 "sudo ss -lntup | grep -E \":53 |:5353 |:953 |:5354 \""'` to pick `bind_port` (53 if free, else 5353), then install BIND9, generate the rndc key, copy it to `/etc/designate/rndc.key` (0644), write `named.conf.options` (listen on 172.16.122.251, `allow-new-zones yes`, `recursion no`), `named-checkconf`, restart, `ss` again to see `named` on the chosen port and 953.
2. Fetch the automation key for the ssh provider: `scp pk@cannon-ubuntu:~/.ssh/pcd_automation <scratchpad>/pcd_automation && chmod 600 <scratchpad>/pcd_automation`. The Mac reaches 172.16.122.251 only through cannon: add `bastion_host = "cannon-ubuntu"`, `bastion_user = "pk"`, `bastion_private_key = file("~/.ssh/id_ed25519")` (whichever key the Mac uses for cannon) to a lab-only copy of `pool.tf` in `<scratchpad>/dns-run/` (the example itself stays bastion-free; note the override in the execution notes).
3. Copy `examples/complete/dns/` to `<scratchpad>/dns-run/`, write `terraform.tfvars` with `dns_host_id = "$HYP1"`, `dns_host_ip = "172.16.122.251"`, `dns_host_ssh_key = "<scratchpad>/pcd_automation"`, `bind_port = 5353` (PCD's `dnsmasq` holds 53 on the host address; step 1 confirms), `ns_hostname = "ns1.pcd.local."`, `zone_name = "tf-e2e.pcd.local."`, `zone_email = "dns-admin@pcd.local"`, `image_name` and `flavor_name` from what the standing region has (`python3 run.py api images`-style probes via the testsuite, or the CE example's names `cirros` / `small`), `network_cidr = "10.90.0.0/24"`.
4. Open the Glance tunnel if the instance boot needs it (`nc -z 127.0.0.1 9494 || ssh -f -N -L 9494:172.16.122.251:9494 pk@cannon-ubuntu`) and use `provider.tf` with the `endpoint_overrides` block the ce-run directory uses.
5. Apply in two stages, so the Designate layout is confirmed before the pool delivery runs against it:
   - `python3 <scratchpad>/lab_env.py -C <scratchpad>/dns-run env TF_CLI_CONFIG_FILE=<scratchpad>/dev.tfrc terraform init`, then `... terraform apply -target=pcd_host_cluster_role.dns -auto-approve -no-color 2>&1 | tee apply-role.log` (background it; the role converges in 5 to 10 minutes and `wait_until_converged` blocks until then).
   - On hyp1, record the real layout: `sudo find /opt/pf9 -maxdepth 4 -name designate-manage -o -name designate.conf; systemctl cat 'pf9-designate*' | grep -E 'User=|ExecStart='; command -v designate-manage`. If the binary, the config file or the user differ from the `designate_manage` / `designate_conf` / `designate_user` defaults, set them in `terraform.tfvars` and correct the defaults in `examples/complete/dns/variables.tf` (a plan deviation to record).
   - `... terraform apply -auto-approve -no-color 2>&1 | tee apply.log` for the rest.
6. Judge, each recorded in the execution notes:
   - resmgr: `GET /resmgr/v1/hosts/$HYP1` shows `pf9-designate` `ok`/`applied`; on hyp1 `systemctl is-active pf9-designate-worker pf9-designate-mdns` (unit names from `systemctl list-units 'pf9-designate*'`) and `ss -lntup | grep 5354` shows `0.0.0.0:5354`.
   - pool: `GET /designate/v2/pools` shows `ns_records = [ns1.pcd.local.]`; on hyp1 `/etc/designate/pools.yaml` is the rendered file (mode 0600, owned by the Designate user, `pf9`), and `apply.log` shows `designate-manage` reporting the pool updated rather than created (a second pool would mean the name did not match; set `id` in the data source).
   - zone: `pcd_dns_zone.app` is `ACTIVE`; on hyp1 `sudo rndc -s 172.16.122.251 -p 953 -k /etc/bind/rndc.key zonestatus tf-e2e.pcd.local.` succeeds.
   - network and subnet: `GET /neutron/v2.0/networks/<id>` has `dns_domain = tf-e2e.pcd.local.`; the subnet has `dns_publish_fixed_ip = true`.
   - record: `GET /designate/v2/zones/<zone>/recordsets` contains an `A` record `dns-demo.tf-e2e.pcd.local.` with the instance's fixed IP; `dig @172.16.122.251 -p <bind_port> dns-demo.tf-e2e.pcd.local. +short` (from cannon) returns it.
   - idempotence: `terraform plan -no-color` reports `No changes.`
   - negative, then positive (PCD-9945's exact claim): add to the scratch configuration a second network that is bound to the same zone **and** external, so the subnet flag is the only variable: `dns_domain = pcd_dns_zone.app.name`, `external = true`, `segments = [{ network_type = "vlan", physical_network = "physnet1", segmentation_id = 100 }]` (a provider VLAN outside the blueprint's 1000–2000 tenant range; the instance needs no connectivity for a record to be written), a subnet `10.91.0.0/24` **without** `dns_publish_fixed_ip`, and an instance `dns-neg` on it. Apply; record `GET /designate/v2/zones/<zone>/recordsets` and confirm no `dns-neg` A record exists, and that neutron-server's log (`kubectl -n pcd logs deploy/neutron-server -c neutron-server | grep -i dns`) shows nothing about a missing zone (that message would mean the negative half is misconfigured, not that the flag worked). Then set `dns_publish_fixed_ip = true` on that subnet, apply, replace the instance (`terraform apply -replace=pcd_compute_instance.neg`), and confirm the `dns-neg.<zone>` A record appears with the new fixed IP.
   - clearing: set `dns_domain` to `""` on the tenant network (omit it), apply, confirm Neutron shows `""`, confirm the subnet's `dns_publish_fixed_ip` stays `true` (Neutron does not cascade; the UI does), and confirm `terraform plan` is clean.
7. Tear down everything except the role, which tiers 4, 5 and 6 still need: `terraform destroy -target=pcd_compute_instance.app -target=pcd_compute_instance.neg -target=pcd_networking_subnet.app -target=pcd_networking_subnet.neg -target=pcd_networking_network.app -target=pcd_networking_network.neg -target=pcd_dns_zone.app -auto-approve -no-color` (Terraform warns that targeted destroys are for exceptional cases; this is one). Then `GET /designate/v2/zones` has no `tf-e2e` zone, `run.py api leftovers` reports nothing, `GET /resmgr/v2/hosts` still lists `dns` on hyp1, and `terraform plan` in `<scratchpad>/dns-run` shows exactly the destroyed resources to be re-created and nothing for the role or the pool. The pool and its file stay (documented); the role and BIND9 stay until tier 6.

**Tier 4: IPv6 override on the host (PCD-9948, Task 5).** The role from tier 3 is still assigned and stays assigned; the acceptance test that creates and removes a role of its own runs in tier 6.
1. Before: dump `GET /resmgr/v1/hosts/$HYP1/roles/pf9-designate` (expect `{"debug": "True", "listen": "0.0.0.0:5354"}`) and `ss -lntup | grep 5354` on hyp1 (expect `0.0.0.0:5354`).
2. In `<scratchpad>/dns-run/role.tf`, uncomment `settings = { listen = "[::]:5354" }` (keep `wait_until_converged = true`) and apply. Pass, provider side: the apply succeeds, the resource shows `settings.listen = "[::]:5354"`, and the v1 GET now returns `listen = "[::]:5354"` with `debug` still present (the merge kept it).
3. Pass, host side, with the apply left in place: poll `ss -lntup | grep 5354` on hyp1 every 30 s for up to ten minutes until it shows `[::]:5354` (the host agent rewrites the mdns configuration and restarts it on its own cycle), and record `sysctl net.ipv6.bindv6only` (`0`, so the wildcard IPv6 socket also serves IPv4). The lab has no global IPv6 address, so this proves the listener, not an IPv6 backend; say so in the notes. The defaults (`listen`, `bindv6only`, a `br-tun` address from the blueprint) are Task 9 product work, not a provider failure.
4. Idempotence: `terraform plan` shows `No changes.` Then set `listen = "0.0.0.0:5354"`, apply, and confirm the v1 GET follows; set it back to `[::]:5354` and leave it for tier 5.
5. Drift handling (only if the user agrees to a UI action): re-save the `dns` role on hyp1 in the UI (a v2 PUT `{}`), then `terraform plan`: if PCD reset `listen`, the plan shows the one-key update and apply restores it; if PCD kept it, the plan is clean. Record which.

**Tier 5: PCD-9946 reproduction (evidence, then revert).**
Precondition: hyp1 still carries the `dns` role (`GET /resmgr/v2/hosts`), `systemctl is-active` reports the designate worker and mdns units, and `/etc/designate/pools.yaml` holds tier 3's file. Re-create the zone (`terraform apply -target=pcd_dns_zone.app` in `<scratchpad>/dns-run`), copy a `pools.yaml` variant whose master is `fd97:45c2:b3a1:100:e481:3fff:fec5:249f` to `/tmp/pools-ipv6.yaml` on hyp1, and run it the way the example does: `sudo -u <designate_user> <designate_manage> --config-file <designate_conf> pool update --dry-run --file /tmp/pools-ipv6.yaml`, then without `--dry-run`. Expected: the `DBDataError (pymysql.err.DataError) (1406, "Data too long for column 'host' at row 1")` traceback from the ticket. Capture `designate-manage`'s output and `SELECT COLUMN_NAME, CHARACTER_MAXIMUM_LENGTH FROM information_schema.COLUMNS WHERE TABLE_SCHEMA='designate' AND COLUMN_NAME='host'` from the management-plane database (via `kubectl -n pcd exec` into the mariadb pod with the credentials from the `designate-db-user` secret; read-only). Re-apply the tier 3 `pools.yaml` to restore the pool. Attach both outputs to the Task 9 comment. Note: the data source refuses nothing here; it warns, so the reproduction uses a hand-written file.

**Tier 6: Regression and cleanup.**
1. Existing suites that touch changed packages, while hyp1 still carries the `dns` role and the tier 3 pool (the DNS zone test needs a working worker and mdns to reach `ACTIVE`; without them it times out after ten minutes and leaves a `tf-acc-example.com.` zone behind): `python3 <scratchpad>/lab_env.py -C "$(pwd)" env TF_ACC=1 go test ./internal/services/networking/ ./internal/services/dns/ -v -count=1 -timeout 60m`. Pass: all PASS.
2. The Community Edition example still applies and destroys cleanly on a build of this branch: only if the standing region has been torn down or the user asks; otherwise `terraform plan` from `~/Documents/PCD-CE/ce-run-2026-09-04/` against the new build must show `No changes.` (that configuration uses `data "pcd_host"`, so this needs the branch merged with `pushkar/pcd-9799-9803` or a build of that branch with these changes; say which was used).
3. Remove the role once, through Terraform: `terraform destroy -auto-approve -no-color` in `<scratchpad>/dns-run` (the zone and the role; on `403 HostInAuthState` wait until `GET /resmgr/v2/hosts` lists hyp1 without `dns` plus five minutes, and run it again). Confirm `GET /resmgr/v1/hosts/$HYP1/roles/pf9-designate` answers 404 and `python3 run.py api hosts` shows hyp1 `responding` with its three original roles.
4. Now the acceptance test that owns a role of its own, `TestAccResmgrHostClusterRoleDNSSettings` (Task 5 Step 5): `python3 <scratchpad>/lab_env.py -C "$(pwd)" env TF_ACC=1 PCD_ACC_RESMGR=1 PCD_ACC_HOST_ID=$HYP1 go test ./internal/services/resmgr/ -run 'TestAccResmgrHostClusterRoleDNSSettings|TestAccResmgrHostClusterRoleImport' -v -count=1 -timeout 60m`. It assigns `dns` with `listen = "[::]:5354"`, checks the v1 settings (the override and the surviving `debug` key), flips the value, imports, and removes the role again in `CheckDestroy` (the deauth poll takes minutes). Pass: PASS. Expect 20 to 30 minutes for the two convergences.
5. Cleanup: uninstall BIND9 and delete `/etc/designate/rndc.key` and `/etc/designate/pools.yaml` on hyp1 if the user wants the host back as it was (the `default` pool in Designate keeps its NS records either way; `pcdctl` or the UI can reset them), close the 9494 tunnel, delete `<scratchpad>/pcd_automation`. Restore `suite.env`'s `REPO_DIR` if it was pointed at the worktree. Record the lab's final state in the execution notes and in memory if it differs from `ce-lab-standing-region`.

---

### Task 9: Product handoffs (PCD-9946, PCD-9948, PCD-9972)

Draft three Jira comments as files under `<scratchpad>/jira/`; the user reads and posts them (posting is outward-facing; do not post without an explicit go-ahead). Each comment: what was verified, where, and the recommended fix. American English, no Terraform-provider internals.

**PCD-9946 (Designate, `zone_masters.host`):**
- Verified on Community Edition 2026.4.2 (Designate 18.0.1.dev9): from the running `designate-api` pod, `designate.storage.sqlalchemy.tables` declares `zone_masters.host` as `String(32)` while `pool_target_masters.host`, `pool_nameservers.host` and `pool_also_notifies.host` are `String(255)`. Upstream master carries the same widths; no Launchpad bug exists for it as of 2026-09-17. `designate-manage pool update` copies target masters into every zone's masters (`_update_zones`), which is where the insert fails.
- Recommended fix: an Alembic migration in `pf9-designate` widening `zone_masters.host` to 255 (the width of its siblings), plus the upstream report. Until then, the provider's pool data source warns about masters over 32 characters, and the workaround is a short static address on the DNS host.
- Attach tier 5's outputs.

**PCD-9948 (Designate mdns IPv6):**
- Verified: the lab's `designate-mdns` runs on the host with the `dns` role (the chart deploys API, central, producer only; `deployment_mdns: false`), and its `listen` is one of the two customizable settings of the `pf9-designate` role (`2026.4.2-847`: `listen = "0.0.0.0:5354"`, `debug = "True"`), reachable through `PUT /resmgr/v1/hosts/<id>/roles/pf9-designate`. `net.ipv6.bindv6only` already reads `0` on an Ubuntu 24.04 host, so that criterion is met by the OS default; the `listen` default and the `br-tun` address are the open items.
- What the provider now offers: `settings = { listen = "[::]:5354" }` on the `dns` cluster role, re-applied when the role is re-assigned. The product asks remain: `[::]:5354` and `net.ipv6.bindv6only = 0` as defaults, and a blueprint-level way to give `br-tun` a static address. Note that a full-length IPv6 address on `br-tun` also trips PCD-9946.

**PCD-9972 (Neutron extension points):**
- Verified on the lab (chart `neutron-2026.4.2-156`, image `quay.io/platform9/pf9-neutron:2026.4.2-1605`): rendered `ml2_conf.ini` has `mechanism_drivers = openvswitch,ovn` and `extension_drivers = port_security,qos,dns_domain_keywords`; rendered `neutron.conf` has `[oslo_messaging_notifications] driver = noop`; the Helm values expose `conf.neutron.ml2_conf.ml2.mechanism_drivers` (rendered from a `null` value) and no init-container or package-install hook.
- Recommendation from the provider's side: nothing in Terraform; the ask is documentation of what survives an upgrade and a supported way to add a driver package. Suggest the docs team's page and the chart owners; offer the rendered values as attachment.

---

## Self-Review

- **Spec coverage.** PCD-9926 → Task 1 (attribute, `""` default clears, data source, docs). PCD-9945 → Task 2 (attribute, default false, data source; UI already done). PCD-9943(a) → Task 3 (typed schema replaces the validation block; every rule in the reporter's 154 lines has a table case in `TestValidatePoolsRejects`); PCD-9943(b) → Task 4 (delivery on the host, trigger on content hash) with the API limit stated in the changelog, the guide and Task 9. PCD-9948 → Task 5 (declarative override) and Task 9 (defaults, sysctl, br-tun). PCD-9946 → Task 3's warning, tier 5's reproduction, Task 9. PCD-9972 → Task 9 with the evidence from the lab dump. Opus panel → Task 7. Lab test plan → Task 8, six tiers with commands and pass criteria.
- **Placeholder scan.** Task 4 asks the implementer to write guide prose from an outline whose every claim is established above, and to check `pcd_compute_instance`'s attribute names against the generated docs; both are named checks, not gaps. Task 0 records five answers that Tasks 3 and 5 depend on; each task says what to change if the answer differs. No "TBD".
- **Type consistency.** `networkCreateOpts` / `networkUpdateOpts` (Task 1) match their test callers; `subnetCreateOpts` / `subnetUpdateOpts` (Task 2) return value types the tests read fields from; `poolConfig` and friends (Task 3) are used by both tests and the data source with the same field names (`RNDCKeyFile`, `APIToken`); `settingsChanged`, `mergeSettings`, `readSettings`, `settingString`, `managedSettings` (Task 5) match their tests; `clusterRoleMarkers["dns"] == "pf9-designate"` already exists. `hostClusterRoleModel.Settings` is a `types.Map`; the existing `roleModel` test helper gets an explicit `types.MapNull(types.StringType)` in Task 5 Step 3 because a zero-value map compiles but fails `State.Set`.
- **Decisions.** Listed at the top; each has a default and the affected task.

## Execution notes

**2026-09-17, planning.** Task 0's probes ran after the lab recovered (`<scratchpad>/probe.out`; answers folded into "Established facts"). An Opus adversarial panel (Task 7's shape, pointed at this document: six finder lenses, one merge, three refuters per finding, one synthesis; 80 agents) produced 43 raw findings, 24 after merging, 12 confirmed after refutation, all folded in: the `roleModel` test-helper null (Task 5), the `router:external` pointer assertion and the `ExpectError` summary match (Task 1), Neutron's lower-casing and label rules in `invalidDNSDomain` (Task 1), the bind9 `rndc_config_file` alternative (Task 3), `designate-manage`'s virtualenv path, config file and user (Tasks 4 and 8), `terraform validate` needing the dev override (Task 4), the settings PUT going through the 409 retry and after convergence (Task 5), and the tier ordering that had removed the `dns` role before tiers 5 and 6 needed it, the negative PCD-9945 check that lacked `dns_domain`, and the listen check that the test reverted before the host agent could act (Task 8). The twelve refuted findings were stated decisions or documented caveats. Round 2 of the panel is Task 7, on the implementation.

(Filled in during execution: the tier judgments, deviations from the plan and why.)

**2026-09-19, execution.** Tasks 1 to 5 and 9 are done, and Task 8's tiers ran on the Community Edition lab (PCD 2026.4.2) on 2026-09-18 and 2026-09-19. Commit SHAs change with each rebase of the stack, so commits are named by subject below.

Corrections to "Established facts" found during execution:
- `designate-manage`: the virtualenv's binary, `/opt/pf9/pf9-designate/bin/designate-manage`, cannot run on its own (`ImportError: libssl.so.10`). The `pf9-designate` package's wrapper, `/usr/sbin/designate-manage`, sets the library paths and runs it; it is on sudo's `secure_path`. The example's `designate_manage` defaults to the wrapper. The units are LSB wrappers (`ExecStart=/etc/init.d/pf9-designate-worker start`) with no `User=` line, so the guide checks the user and the configuration file with `ps` and `pool show_config` instead of `systemctl cat`.
- The `dnsmasq` that holds port 53 on a PCD host is installed by `pcdctl prep-node` (`apt-get install -y dnsmasq ...`), not by a PCD role. BIND runs on 5353 on such a host.
- The Designate worker caches the pool. It loads the pool once per process, the first time it needs it ("Lazily loading pool"), and keeps using those targets: after `designate-manage pool update` changed the bind9 target port, the worker kept sending NOTIFY to the old port. A pool change needs `systemctl restart pf9-designate-worker` on the DNS host; a first delivery works without it only because the new role's worker has not loaded the pool yet. The example's delivery now restarts the worker.
- resmgr keeps a host's per-role settings after the role is removed. `GET /resmgr/v1/hosts/<id>/roles/pf9-designate` answered 200 with the last settings for 11 minutes and more after the `dns` role left the host, while the v1 host record's `roles` no longer listed `pf9-designate`; only a role never assigned to the host answers 404. After a later assignment the endpoint briefly returned the old values before it went back to the role's defaults, so `pcd_host_cluster_role` now always writes `settings` on create.
- Neutron creates reverse zones. Publishing a fixed IP also creates a `<c>.<b>.<a>.in-addr.arpa.` zone per /24 (for example `0.90.10.in-addr.arpa.`) with PTR records, in Neutron's own service project and the same pool. The project-scoped zone list does not show them (`openstack zone list --all-projects` does). The PTR records go with the ports; the zones stay after `terraform destroy` until an admin deletes them.
- Tenant networks on the lab's region do not bind. Neutron gives a network with no `segments` the tenant type `vlan` on `physnet9`, which hyp1 does not map (`ovn-bridge-mappings physnet1:br-tun`), so an instance on it fails with `PortBindingFailed`. `segments = [{ network_type = "geneve" }]`, with no physical network or segmentation ID, binds at once on the OVN host with Geneve encapsulation; Neutron takes the VNI from its range (1000:2000), and the network stays a tenant network for the publishing rules. Tier 3's PCD-9945 check used a Geneve network with `external = true` instead of the plan's provider VLAN.

Lab tier results:
- Tier 0: the read-only probes, recorded above on 2026-09-17.
- Tier 1: PASS. Unit tests green in every package, `golangci-lint` 0 issues.
- Tier 2: PASS. `TestAccNetworkingNetworkDNSDomain`, `TestAccNetworkingSubnetDNSPublishFixedIP`, and the existing network and subnet tests pass. The `pcd_dns_pools_config` data source planned and applied (a pdns4 pool with `also_notifies` included), refused a port of 70000 at the right path, and warned about a 38-character IPv6 master with PCD-9946's number. No leftovers.
- Tier 3, first run: PARTIAL. The `dns` role converged in about a minute, the pool was delivered, the zone went ACTIVE, the network took `dns_domain`, the subnet took `dns_publish_fixed_ip`, and BIND served the zone. The example's `designate_manage` default failed (corrected above), and the instance failed with `PortBindingFailed` (corrected above), so steps 5 to 7 waited for the second run.
- Tier 3, second run, with a Geneve segment: PASS. The instance's A record showed in the API, in `dig` against BIND, and in `pcdctl recordset list`, and `terraform plan` was clean. PCD-9945: on an external network with no flag, no record after more than two minutes and nothing in neutron-server's log about a missing zone; with the flag set and the instance replaced, the record appeared with the new fixed IP. Clearing `dns_domain` left the subnet's flag and the existing record in place. The pool-cache check found the cached pool (corrected above).
- Tier 4: PASS. `settings = { listen = "[::]:5354" }` merged over the role's settings with `debug` kept, `designate-mdns` listened on `*:5354` 5 to 14 seconds after the write, plans were clean, and flipping to `0.0.0.0:5354` and back worked. A v2 re-save of the role (`PUT {}`) kept `listen`, so there is no drift to handle.
- Tier 5: PASS. PCD-9946 reproduced: `designate-manage pool update` with a 38-character IPv6 master failed with `Data too long for column 'host'` on `INSERT INTO zone_masters` once a zone existed; `zone_masters.host` is `VARCHAR(32)` and `pool_target_masters.host` is `VARCHAR(255)`. `--dry-run` passes with the long master, and the failed run leaves the pool's target master updated while the zones keep the old one; running `pool update` again with the previous file restored both.
- Tier 6: networking and dns acceptance suites PASS with the `dns` role and the pool in place (the two floating IP tests skip on their own environment gate). The upgrade check, `terraform plan` of the standing region's configuration against a build of this stack, reported `No changes.` One `terraform destroy` removed the `dns` role: it left the host in 40 seconds, with no `403 HostInAuthState` and no second destroy needed. `TestAccResmgrHostClusterRoleImport` with `PCD_ACC_CLUSTER_ROLE=dns` passed. Every step of `TestAccResmgrHostClusterRoleDNSSettings` passed, but its `CheckDestroy` failed on the per-role endpoint that resmgr keeps answering (corrected above); the check now reads the v1 host record's `roles`.

Stacked branches and fix waves:
- The work sits on three stacked branches, as Decision 6 set out: `pushkar/networking-dns-attributes` (Tasks 1 and 2), `pushkar/dns-pools-config` on it (Tasks 3 and 4), and `pushkar/dns-role-settings` on that (Task 5). Each branch carries its own changelog commit. After each fix wave, the later branches were rebased onto the new heads of the earlier ones, and each rebase was checked for dropped commits.
- Fix wave 1 followed round 1 of the Opus panel (20 findings, 9 confirmed). It made `dns_domain` accept Neutron's keyword labels, staged `pools.yaml` in a private directory, made the data source's `id` sensitive, had the example resolve the DNS host by name with `data "pcd_host"`, and on the third branch skipped an unchanged settings write and reused `assignBody` in Update.
- Fix wave 2 followed tiers 3 to 5. It corrected the changelog's `dns_publish_fixed_ip` wording, switched the example to the `designate-manage` wrapper with guide checks that exercise it and the tenant-network prerequisite, and documented how fast `designate-mdns` moves to a new listen address.
- Fix wave 3 followed tier 3's second run and tier 6. On the second branch, the example's delivery restarts `pf9-designate-worker` and runs `designate-manage` with `--nodebug`; the guide and the example README describe the worker restart, Neutron's reverse zones, and the Geneve segment that binds on an OVN region; and the README no longer says a second destroy may be needed. On the third branch, Create always writes `settings`, the `settings` description says resmgr keeps a role's settings after removal, the acceptance destroy check reads the host's roles, and these notes were added.
- Still open: round 2 of the Opus panel, on the final stack; the PCD-9946, PCD-9948, and PCD-9972 comments that Task 9 drafted, which the user posts; and a short lab check of this wave (the worker restart, `--nodebug`, and the resmgr acceptance tests).
