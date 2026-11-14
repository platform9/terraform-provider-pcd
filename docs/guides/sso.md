---
page_title: "Single sign-on with a SAML identity provider - PCD Provider"
subcategory: ""
description: |-
  Configure PCD single sign-on with a SAML identity provider: the SSO configuration, the Keystone identity provider, the mapping, the saml2 protocol, and the group users join.
---

# Single sign-on with a SAML identity provider

PCD signs users in through a SAML identity provider such as Okta, OneLogin,
Google Workspace or Microsoft Entra ID. In the PCD UI this is the Single
Sign-On page: **SAML Config** on one tab, **SAML Groups** on the other. Behind
them are five kinds of object, and this guide builds all of them with the files
under `examples/complete/sso/`:

- **The SSO configuration** (`pcd_identity_sso_configuration`): the identity
  provider's entity ID, its metadata URL, and the attribute map. PCD keeps it
  in hagrid, its SSO configuration service, which writes it into Keystone's
  Shibboleth service provider.
- **The identity provider** (`pcd_identity_federation_identity_provider`): the
  Keystone object federated users sign in through.
- **The mapping** (`pcd_identity_federation_mapping`): the rules that turn the
  attributes the identity provider asserts into a user name, an email address
  and a group.
- **The protocol** (`pcd_identity_federation_protocol`): `saml2`, which ties
  the identity provider to the mapping.
- **The group and its roles** (`pcd_identity_group` and
  `pcd_identity_role_assignment`): what a user who signs in may do. The
  example grants the role on one project. The PCD UI's SAML Groups page grants
  it on every project in the domain instead, as an inherited grant: for that,
  use `pcd_identity_inherit_role_assignment` with `group_id` and `domain_id`
  (the commented block in `groups.tf`).

## Names that must match

Nothing checks these, and a mismatch does not fail any apply: users simply
cannot sign in.

- The identity provider's `name` must equal the SSO configuration's
  `identity_provider`. PCD only routes SAML sign-in for identity providers that
  have an SSO configuration of the same name. The PCD UI uses `IDP1` for the
  default domain, and for another domain the domain's name with every
  character other than a letter, digit, `-` or `_` replaced by `_`. Renaming
  that domain later does not rename the identity provider, as the `name`
  description of `pcd_identity_domain` also warns. For a domain this
  configuration manages, set the identity provider's `domain_id` to
  `pcd_identity_domain.<name>.id`; for one it does not, look it up with the
  `pcd_identity_domain` data source. Destroying a domain also deletes the
  identity providers registered to it.
- The identity provider's `remote_ids` must hold the SSO configuration's
  `entity_id`.
- The protocol must be named `saml2`.
- The mapping's remote types must be ids the attribute map gives the SAML
  attributes.

The example takes the identity provider's name and remote IDs from the SSO
configuration's attributes, so the first two cannot drift apart. It names the
mapping `<identity provider, in lowercase>_mapping`, as the PCD UI does.

## Register PCD with the identity provider

Create a SAML application for PCD in the identity provider first; it gives you
the entity ID and the metadata URL the configuration needs. The application
asks for two values from PCD, which depend only on PCD's host name and the
identity provider name:

- **Entity ID (identifier):** `https://<PCD host>/keystone`
- **Reply URL (assertion consumer service URL):**
  `https://<PCD host>/sso/<identity provider>/Shibboleth.sso/SAML2/POST`, for
  example `https://pcd.example.com/sso/IDP1/Shibboleth.sso/SAML2/POST`

After the apply, the `sp_entity_id` and `acs_url` outputs show both values,
built from the host the provider reaches the SSO service on (see
[Reaching the SSO service](#reaching-the-sso-service)). Have the identity
provider send the user's email address, and their groups if group membership
should decide who may sign in.

PCD downloads the metadata from `metadata_url` itself, from its own network and
without verifying the certificate, when the configuration is saved. The URL
must be reachable from PCD, and an unreachable one fails the apply.

## The configuration

`project_name` has no default: name an existing project.

```hcl
identity_provider = "IDP1"
sso_provider      = "okta"
entity_id         = "http://www.okta.com/exk1a2b3c4d5e6f7g8h9"
metadata_url      = "https://example.okta.com/app/exk1a2b3c4d5e6f7g8h9/sso/saml/metadata"
idp_group         = "pcd-users"
# Name an existing project: users who sign in get role_name on it.
project_name = "sso-project"
role_name    = "member"
```

```terraform
terraform {
  required_providers {
    pcd = {
      source  = "platform9/pcd"
      version = "~> 0.1.22"
    }
  }
}

# Credentials come from the pcdctl RC file: in the PCD UI open Settings > API
# Access, copy it, set OS_PASSWORD, and `source pcdctlrc` before running
# Terraform. Configuring single sign-on needs the admin role.
provider "pcd" {
  user_domain_id    = "default"
  project_domain_id = "default"

  # For a PCD with a self-signed certificate. Remove it, or set cacert_file,
  # when the certificate is one this machine trusts.
  insecure = true
}
```

The attribute map names the SAML attributes the mapping matches. Each
`Attribute` gives one name the identity provider may send an id; the mapping's
`remote` entries refer to these ids.

```xml
<Attributes xmlns="urn:mace:shibboleth:2.0:attribute-map" xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance">
    <!-- Each SAML attribute the mapping rules match, and the id they match it by.
         Adjust the names to what your identity provider sends. -->
    <Attribute name="urn:oid:0.9.2342.19200300.100.1.3" id="email"/>
    <Attribute name="email" nameFormat="urn:oasis:names:tc:SAML:2.0:attrname-format:unspecified" id="email"/>
    <Attribute name="groups" nameFormat="urn:oasis:names:tc:SAML:2.0:attrname-format:unspecified" id="groups"/>
</Attributes>
```

```terraform
# PCD's SAML configuration. Saving it restarts Keystone and the Shibboleth
# daemon for a minute or two; the resource waits until both answer again.
resource "pcd_identity_sso_configuration" "this" {
  identity_provider = var.identity_provider
  sso_provider      = var.sso_provider
  entity_id         = var.entity_id
  metadata_url      = var.metadata_url
  attribute_map_xml = file("${path.module}/attribute-map.xml")
}
```

The group depends on the SSO configuration, so it, and everything built on it,
is created after the Keystone restart the configuration causes (see below).

```terraform
data "pcd_identity_project" "sso" {
  name = var.project_name
}

data "pcd_identity_role" "sso" {
  name = var.role_name
}

# The group users join when they sign in. depends_on puts it, and everything
# built on it, after the Keystone restart the SSO configuration causes.
resource "pcd_identity_group" "sso_users" {
  name        = var.group_name
  description = "Users who sign in through ${var.sso_provider}"
  domain_id   = var.domain_id

  depends_on = [pcd_identity_sso_configuration.this]
}

resource "pcd_identity_role_assignment" "sso_users" {
  group_id   = pcd_identity_group.sso_users.id
  project_id = data.pcd_identity_project.sso.id
  role_id    = data.pcd_identity_role.sso.id
}

# The PCD UI's SAML Groups page grants the role on every project in the
# domain instead, as an inherited grant. To do the same, use this in place of
# the assignment above:
#
# resource "pcd_identity_inherit_role_assignment" "sso_users" {
#   group_id  = pcd_identity_group.sso_users.id
#   domain_id = var.domain_id
#   role_id   = data.pcd_identity_role.sso.id
# }
```

```terraform
# One rule per group, in the shape the PCD UI writes: the user's email address
# becomes their user name and email, and members of idp_group join the group.
# The remote types are the ids attribute-map.xml gives the SAML attributes.
resource "pcd_identity_federation_mapping" "this" {
  name = "${lower(var.identity_provider)}_mapping"
  rules = jsonencode([{
    local = [{
      user  = { name = "{0}", email = "{1}" }
      group = { id = pcd_identity_group.sso_users.id }
    }]
    remote = [
      { type = "email" },
      { type = "email" },
      { type = "groups", any_one_of = [var.idp_group] },
    ]
  }])
}

# The identity provider must have the SSO configuration's name and list its
# entity ID; taking both from the SSO configuration keeps them in step.
resource "pcd_identity_federation_identity_provider" "this" {
  name       = pcd_identity_sso_configuration.this.identity_provider
  domain_id  = var.domain_id
  remote_ids = [pcd_identity_sso_configuration.this.entity_id]
}

# PCD signs users in through the saml2 protocol and no other.
resource "pcd_identity_federation_protocol" "saml2" {
  identity_provider = pcd_identity_federation_identity_provider.this.name
  name              = "saml2"
  mapping_id        = pcd_identity_federation_mapping.this.name
}
```

```terraform
output "acs_url" {
  description = "PCD's reply (assertion consumer service) URL, as the identity provider must have it."
  value       = pcd_identity_sso_configuration.this.acs_url
}

output "sp_entity_id" {
  description = "PCD's SAML entity ID (identifier), as the identity provider must have it."
  value       = pcd_identity_sso_configuration.this.sp_entity_id
}

output "login_url" {
  description = "The path on PCD's host that starts a sign-in through the identity provider."
  value       = pcd_identity_sso_configuration.this.login_url
}
```

To let more groups in, add a rule per group to the mapping, each with its own
`any_one_of` and `group`. The resource owns the whole rule list: editing SAML
groups in the PCD UI rewrites the same mapping, and the next apply puts the
configured rules back.

## The restart

Creating, changing or deleting the SSO configuration restarts Keystone's web
server and the Shibboleth daemon. The restart starts within seconds and usually
lasts one to two minutes, sometimes up to five. In that time the PCD API and UI
do not answer, and every Terraform operation against PCD fails.

`pcd_identity_sso_configuration` waits it out: after every write it waits 30
seconds, then checks every 10 seconds until Keystone and the SSO service answer
twice in a row, for up to 10 more minutes. Resources that depend on it,
directly or through `depends_on`, run after that. Resources that do not may run
during the restart and fail, which is why the example's group depends on it.
Keystone itself does not need the identity provider, the mapping or the
protocol to wait for the SSO configuration; the example orders them after it
only to keep them out of the restart. Do not run other Terraform applies, or
anything else, against PCD while one that changes the SSO configuration is
running.

Each change to `sso_provider`, `entity_id`, `metadata_url` or
`attribute_map_xml` is one in-place update and one restart, however many of the
four it changes. Changing `identity_provider` replaces the configuration, which
is two restarts. The PCD UI does not let you change the entity ID once it is
saved; the provider does.

If the wait times out, the write has still been made, and the error says what
to do next:

- After a create, the apply fails and Terraform records the SSO configuration
  as tainted. The error reads: "The SSO configuration was saved in hagrid, and
  Terraform records it as tainted. Once PCD answers again, run
  `terraform untaint <resource address>` and then `terraform apply` to create
  what depends on it. Without the untaint, the next apply replaces the
  configuration, which restarts Keystone twice more." In the example the
  address is `pcd_identity_sso_configuration.this`.
- After an update, the apply fails with the new values already in state. The
  error reads: "The new values are saved in hagrid and in Terraform's state, so
  the next plan shows no change for this SSO configuration. Once PCD answers
  again, run `terraform apply` to create what depends on it."
- After a destroy, the provider only warns: "The SSO configuration is deleted
  and gone from Terraform's state; nothing more needs doing for it."

## Reaching the SSO service

The SSO configuration lives in hagrid, which is not in PCD's service catalog.
The provider reaches it on the host of the catalog's identity endpoint for the
`Infra` region (or for the provider's region, if the catalog has none for
`Infra`), at `/hagrid/`, which is where the PCD UI finds it. If that host is not
reachable from where Terraform runs, name hagrid's URL in the provider:

```terraform
provider "pcd" {
  endpoint_overrides = {
    hagrid = "https://pcd.example.com/hagrid/"
  }
}
```

The SSO service only answers on PCD's own host name: a URL that reaches PCD by
IP address does not work.

## Bring an existing setup under Terraform

An SSO setup made in the PCD UI already has all five kinds of object. Write the
configuration, then import each object instead of creating it. Creating the SSO
configuration, the identity provider, the mapping or the protocol when PCD
already has one by that name fails with a `terraform import` hint rather than
overwriting it.

```shell
terraform import pcd_identity_sso_configuration.this IDP1
terraform import pcd_identity_federation_identity_provider.this IDP1
terraform import pcd_identity_federation_mapping.this idp1_mapping
terraform import pcd_identity_federation_protocol.saml2 IDP1/saml2
terraform import pcd_identity_group.sso_users <group_id>
terraform plan
```

`pcdctl group list` prints the group ID. The PCD UI's SAML Groups grant is an
inherited domain grant, not the project assignment the example makes: to keep
it, use the commented `pcd_identity_inherit_role_assignment` block in place of
`pcd_identity_role_assignment.sso_users` and import it with
`terraform import pcd_identity_inherit_role_assignment.sso_users <domain_id>//<group_id>//<role_id>`
(`pcdctl role assignment list --names --inherited` shows the grant); otherwise
expect the plan to add the project assignment. The plan shows any other
difference between the configuration and what the PCD UI saved; the mapping's
rules are the usual one.

## Remove single sign-on

`terraform destroy` removes the objects in the reverse of the order they were
created: the protocol and the role assignment first, then the identity provider
and the mapping, then the group, and the SSO configuration last, which restarts
Keystone once more. Users who signed in through the identity provider stay in
Keystone as federated users; the PCD UI's Disable SSO deletes them, Terraform
does not. Find them with `pcdctl user list --domain default` and delete those
you no longer want with `pcdctl user delete`.

Keystone does not check whether a protocol still uses a mapping before it
deletes the mapping. Destroying a mapping that another protocol names breaks
sign-in through that protocol.

Write group IDs in the mapping's `rules` as `pcd_identity_group.<name>.id`, as
the example does, never as literal IDs: the reference makes Terraform destroy
the mapping before the groups it names, so no mapping is left naming a deleted
group.

## Limits

- The metadata comes from `metadata_url` only. Raw metadata XML and the SAML
  signing setting are not supported, as in the PCD UI.
- Keystone stores a `schema_version` for each mapping, which the mapping
  resource does not manage: every create and update gets Keystone's default.
- The PCD UI's System-Level Access for an SSO group (a system-scope role) has no
  resource yet.
