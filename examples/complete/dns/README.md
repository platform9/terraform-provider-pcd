# DNS: publish instance records with Designate

A runnable configuration that turns on DNS records for instances in a PCD
region. It assigns the `dns` role to a host, renders a Designate pool for a
BIND9 server on that host and applies it there, creates a zone, binds a tenant
network to the zone with a subnet that publishes its fixed IPs, and boots an
instance whose A record appears in the zone.

The walkthrough is the
[DNS guide](https://registry.terraform.io/providers/platform9/pcd/latest/docs/guides/dns)
on the Terraform Registry; the files here are the ones it renders.

## Prerequisites

- A PCD region with a hypervisor, an image, and a flavor. The
  [Community Edition example](../community-edition/) builds one.
- A region that can boot an instance on a tenant network. The example's
  network has no `segments`, so Neutron gives it the region's tenant network
  type. If tenant networks cannot bind on the host, give
  `pcd_networking_network.app` `segments` that your region can bind. On an OVN
  region with Geneve encapsulation,
  `segments = [{ network_type = "geneve" }]`, with no physical network or
  segmentation ID, binds and keeps it a tenant network. Other regions need
  segments that their hosts map.
- A host for the `dns` role that has been authorized and has reported in, so
  `pcd_host` can find it by the hostname it reports.
- BIND9 and an rndc key on that host, set up as the guide's "Prepare the DNS
  host" section describes.
- SSH access from where Terraform runs to that host, as a user with
  passwordless sudo. `loafoe/ssh` stores the private key in the Terraform
  state, so keep the state in an encrypted backend, or load the key into
  `ssh-agent`, then set `agent = true` and remove `private_key` in `pool.tf`.

## Run it

```shell
source pcdctlrc                      # the RC file from Settings > API Access
export OS_INSECURE=true              # Community Edition's self-signed certificate
cp terraform.tfvars.example terraform.tfvars
# edit terraform.tfvars: the host's name and address, and dns_host_ssh_key,
# the private key Terraform connects with. bind_port is 5353 because dnsmasq
# holds 53 on a PCD host; remove it if BIND runs on 53.
terraform init
terraform apply -target=pcd_host_cluster_role.dns   # the dns role on its own first
```

The `dns` role takes a few minutes to converge, and the apply waits for it.
Terraform warns that resource targeting is in effect; that is expected in this
first stage. Then, on the DNS host, check that Designate's services run as
`designate_user` with `designate_conf`, and that `designate_manage` runs with
both; the guide's "Apply" section says what each output should show:

```shell
ps -eo user,args | grep -E 'designate-(worker|mdns)'
sudo -u pf9 /usr/sbin/designate-manage --config-file /opt/pf9/etc/pf9-designate/designate.conf pool show_config
```

Set any value that differs in `terraform.tfvars`, then apply the rest, which
delivers the pool and creates the zone, the network, and the instance:

```shell
terraform apply
```

## Confirm

```shell
pcdctl recordset list <zone_name>    # an A record for the instance, with its fixed IP
dig @<dns_host_ip> -p <bind_port> "$(terraform output -raw record_name)" +short
```

## Take it down

`terraform destroy` removes the instance, the network and subnet, the zone, and
the `dns` role. The pool stays in Designate, and BIND9 and the files under
`/etc/designate/` stay on the host. Neutron's reverse zone for the subnet
(`0.90.10.in-addr.arpa.` for the default `network_cidr`) stays too, in
Neutron's own project, where `openstack zone list --all-projects` shows it and
an admin deletes it with `openstack zone delete <name> --all-projects`.
