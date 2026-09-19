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
- A host for the `dns` role that has been authorized and has reported in, so
  `pcd_host` can find it by the hostname it reports.
- BIND9 and an rndc key on that host, set up as the guide's "Prepare the DNS
  host" section describes.
- SSH access from where Terraform runs to that host, as a user with
  passwordless sudo.

## Run it

```shell
source pcdctlrc                      # the RC file from Settings > API Access
export OS_INSECURE=true              # Community Edition's self-signed certificate
cp terraform.tfvars.example terraform.tfvars
# edit terraform.tfvars: the host's name and address, and dns_host_ssh_key,
# the private key Terraform connects with. bind_port is 5353 because dnsmasq
# holds 53 on a PCD host; remove it if BIND runs on 53.
terraform init
terraform apply
```

The `dns` role takes a few minutes to converge, and the apply waits for it
before it delivers the pool.

## Confirm

```shell
pcdctl recordset list <zone_name>    # an A record for the instance, with its fixed IP
dig @<dns_host_ip> -p <bind_port> "$(terraform output -raw record_name)" +short
```

## Take it down

`terraform destroy` removes the instance, the network and subnet, the zone, and
the `dns` role. The pool stays in Designate, and BIND9 and the files under
`/etc/designate/` stay on the host. PCD acknowledges the role removal before
the host has finished deauthorizing it, so, as with the Community Edition
example, a second `terraform destroy` a few minutes later may be needed.
