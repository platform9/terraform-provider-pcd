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

`terraform destroy` removes everything, including the blueprint. PCD refuses the
host-configuration unassignment while the host's deauthorization is still
landing (`HostInAuthState`); wait a few minutes and run `terraform destroy`
again. The host stays authorized and can be onboarded again.
