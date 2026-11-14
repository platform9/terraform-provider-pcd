# Single sign-on with a SAML identity provider

A runnable configuration that lets users sign in to PCD through a SAML identity
provider. It saves PCD's SSO configuration, creates the Keystone identity
provider, a group with a role on a project, the mapping that puts users who
sign in into that group, and the `saml2` protocol that ties them together.

The walkthrough is the
[single sign-on guide](https://registry.terraform.io/providers/platform9/pcd/latest/docs/guides/sso)
on the Terraform Registry; the files here are the ones it renders.

## Prerequisites

- Credentials with the admin role.
- A SAML application for PCD in your identity provider, with PCD's entity ID
  `https://<PCD host>/keystone` and reply URL
  `https://<PCD host>/sso/IDP1/Shibboleth.sso/SAML2/POST`. It sends the user's
  email address, and the groups they belong to.
- The application's metadata URL, reachable from PCD, and its entity ID.
- An existing project for the group's role.
- Nothing else working against PCD while you apply: saving the SSO
  configuration restarts Keystone for about half a minute, sometimes longer.

## Run it

```shell
source pcdctlrc                      # the RC file from Settings > API Access
cp terraform.tfvars.example terraform.tfvars
# edit terraform.tfvars: entity_id, metadata_url, idp_group, and project_name
terraform init
terraform apply
```

The apply pauses for about a minute after the SSO configuration while
Keystone restarts, then creates the rest.

## Confirm it worked

- `pcdctl identity provider show IDP1` lists the entity ID under `remote_ids`.
- `pcdctl federation protocol list --identity-provider IDP1` lists `saml2`
  with the mapping.
- The PCD sign-in page offers single sign-on. Sign in as a member of
  `idp_group`: the user lands in the project with the role, and
  `pcdctl user list --domain default` lists them afterwards.

## Take it down

`terraform destroy` removes everything, the SSO configuration last, which
restarts Keystone once more. It also deletes the link between the identity
provider and every user who signed in, and leaves those users in Keystone
without a name: before the destroy, note their IDs from
`pcdctl user list --domain default`, and delete them afterward with
`pcdctl user delete <id>`, or their next sign-in through a new identity
provider in the same domain fails with a conflict.
