---
page_title: "Authentication - PCD Provider"
subcategory: ""
description: |-
  How to authenticate the Platform9 Private Cloud Director (PCD) provider to Keystone.
---

# Authentication

The PCD provider authenticates to Keystone v3 and then talks to the OpenStack
services PCD exposes (Nova, Neutron, Cinder, Glance, Keystone). It supports the
same authentication methods and `OS_*` environment variables as the upstream
OpenStack tooling, so an existing openrc file or `clouds.yaml` works unchanged.

## Configuration precedence

Every setting can be supplied three ways. When more than one is present, the
provider uses the first that is set, in this order:

1. An explicit argument in the `provider` block.
2. The corresponding `OS_*` environment variable.
3. A `clouds.yaml` entry selected with `cloud` (see [clouds.yaml](#cloudsyaml)).

An explicitly-empty argument (for example a variable that defaults to `""`) is
treated as unset, so the environment/`clouds.yaml` fallback still applies.

## Password authentication

The most common method. Scope to a project with `tenant_name`/`tenant_id` and the
relevant Keystone v3 domains.

```terraform
provider "pcd" {
  auth_url    = "https://pcd.example.com/keystone/v3"
  region      = "RegionOne"
  user_name   = "admin"
  password    = var.pcd_password
  tenant_name = "service"

  user_domain_name    = "Default"
  project_domain_name = "Default"
}
```

The equivalent environment variables are `OS_AUTH_URL`, `OS_REGION_NAME`,
`OS_USERNAME`, `OS_PASSWORD`, `OS_PROJECT_NAME`, `OS_USER_DOMAIN_NAME`, and
`OS_PROJECT_DOMAIN_NAME`. With those exported, the provider block can be empty:

```terraform
provider "pcd" {}
```

## Token authentication

Use a pre-issued Keystone token (for example from `openstack token issue`).

```terraform
provider "pcd" {
  auth_url = "https://pcd.example.com/keystone/v3"
  token    = var.pcd_token

  tenant_name         = "service"
  project_domain_name = "Default"
}
```

Falls back to `OS_TOKEN` / `OS_AUTH_TOKEN`.

## Application credential authentication

Application credentials are already scoped, so do not set a project or domain.

```terraform
provider "pcd" {
  auth_url                      = "https://pcd.example.com/keystone/v3"
  application_credential_id     = var.appcred_id
  application_credential_secret = var.appcred_secret
}
```

Falls back to `OS_APPLICATION_CREDENTIAL_ID` /
`OS_APPLICATION_CREDENTIAL_NAME` / `OS_APPLICATION_CREDENTIAL_SECRET`. You can
create one with the [`pcd_identity_application_credential`](../resources/identity_application_credential.md)
resource.

## clouds.yaml

Set `cloud` (or `OS_CLOUD`) to source the auth settings above from a named
`clouds.yaml` entry. The file is searched at `$OS_CLIENT_CONFIG_FILE`,
`./clouds.yaml`, `~/.config/openstack/clouds.yaml`, then
`/etc/openstack/clouds.yaml`.

```terraform
provider "pcd" {
  cloud = "pcd"
}
```

```yaml
# clouds.yaml
clouds:
  pcd:
    auth:
      auth_url: https://pcd.example.com/keystone/v3
      username: admin
      password: super-secret
      project_name: service
      user_domain_name: Default
      project_domain_name: Default
    region_name: RegionOne
    verify: false
```

Explicit provider arguments and `OS_*` variables still override individual values
from the file. `verify: false` maps to skipping TLS verification.

## TLS

PCD Community Edition ships a self-signed certificate. Either trust it with a CA
bundle or, for a lab, skip verification.

```terraform
provider "pcd" {
  auth_url = "https://pcd.example.com/keystone/v3"
  # ... credentials ...

  insecure = true # skip verification (labs / self-signed); OS_INSECURE
  # or, preferred:
  # cacert_file = "/etc/pki/pcd-ca.pem"  # OS_CACERT
}
```

For mutual TLS, set `cert` and `key` (PEM paths; `OS_CERT` / `OS_KEY`).

## Endpoint overrides

When the service catalog advertises an endpoint that is not reachable as-is (a
common situation in labs behind a proxy or tunnel), override it per service type.

```terraform
provider "pcd" {
  auth_url = "https://pcd.example.com/keystone/v3"
  # ... credentials ...

  endpoint_overrides = {
    image    = "https://pcd.example.com/glance/"
    volumev3 = "https://pcd.example.com/cinder/v3/"
  }
}
```

Keys are Keystone service types (`identity`, `image`, `network`, `compute`,
`volumev3`).
