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
`HostInAuthState`). The guide's Destroy section walks through it. The host
stays authorized and can be onboarded again.
