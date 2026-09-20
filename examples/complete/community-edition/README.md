# Community Edition: from an empty host to a running VM

A complete, runnable configuration that takes a freshly installed PCD Community
Edition with one prepared host and builds a working region on it: a volume type
and cluster blueprint with an NFS storage backend, host networking, a cluster,
the hypervisor, image-library, and persistent-storage roles, then a provider
network, a CirrOS image, a flavor, and an instance with a volume attached.

The walkthrough is the
[Community Edition guide](https://registry.terraform.io/providers/platform9/pcd/latest/docs/guides/community-edition)
on the Terraform Registry; the files here are the ones it renders.

## Run it

```shell
source pcdctlrc                      # the RC file from Settings > API Access
export OS_INSECURE=true              # Community Edition's self-signed certificate
cp terraform.tfvars.example terraform.tfvars
# edit terraform.tfvars
terraform init
terraform apply
```

Role convergence takes several minutes per role; the whole apply is about
twenty minutes on a small host.

## Take it down

`terraform destroy` removes everything, including the blueprint, but expect to
run it three times a few minutes apart: PCD refuses the storage role while the
deleted image's backing volume (`image-<id>`, in the service project) still
exists, so delete that volume with `pcdctl volume delete` between runs; and it
refuses the cluster and the host-configuration unassignment while a role's
deauthorization is still landing (`HostClusterDeleteFailed`,
`HostInAuthState`). The guide's Destroy section walks through it.

The host stays authorized, but it is not back to the state the first apply
found: two things a destroy leaves on the host refuse the next onboarding.
Delete the OVS bridges the roles left behind and re-apply the host's network
configuration (`sudo ovs-vsctl --if-exists del-br <bridge>` for each bridge
`sudo ovs-vsctl list-br` prints, then `sudo netplan apply`), so the management
address returns from `br-tun` to the interface; without it the next
`pcd_host_config_assignment` is refused with `404 HostIntfIpNotFound: Interface
enp1s0 is missing an IP`. Then re-enable the volume service the storage
deauthorization disabled, `pcdctl volume service set --enable
<host-uuid>@nfs-primary cinder-volume`; without it every volume lands in `error`
and the apply hangs on `pcd_images_image.cirros` for the provider's
thirty-minute image timeout. The guide's "Before onboarding the host again"
section has the checks and the cleanup a hung apply needs.
