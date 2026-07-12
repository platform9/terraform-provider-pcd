# Releasing `terraform-provider-pcd`

This provider is distributed through the [Terraform Registry](https://registry.terraform.io)
at `platform9/pcd`. Releases are cut by pushing a semver tag; the
[`release` workflow](.github/workflows/release.yml) builds every platform binary
with [GoReleaser](https://goreleaser.com), signs the checksums with the org GPG
key, and publishes a GitHub Release. The registry ingests that release
automatically via its webhook.

The steps below are split into **one-time setup** (done once for the repo) and
**per-release** (done for every version).

---

## One-time setup

These are account/organisation actions that require repo-admin and a
registry-connected GitHub account. They cannot be automated from CI and are
**not** performed by any tooling in this repo — a human runs them once.

### 1. Generate the signing GPG key

The registry verifies every release's `SHA256SUMS` signature against a public
key registered to the `platform9` namespace. Generate a dedicated key (RSA 4096,
no expiry is fine for a service key):

```sh
gpg --full-generate-key            # choose RSA/RSA, 4096 bits
gpg --list-secret-keys --keyid-format=long   # note the key ID / fingerprint
```

Export both halves:

```sh
# Public key — this is uploaded to the Terraform Registry (step 3).
gpg --armor --export "<KEY_ID>" > pcd-signing-key.pub.asc

# Private key — this becomes the GPG_PRIVATE_KEY GitHub secret (step 2).
gpg --armor --export-secret-keys "<KEY_ID>" > pcd-signing-key.priv.asc
```

Keep the private key and its passphrase in the org secret manager. Do **not**
commit either file (both are covered by the `.gitignore` `*.asc` rule).

### 2. Add the GitHub Actions secrets

In **Settings → Secrets and variables → Actions**, add:

| Secret | Value |
| --- | --- |
| `GPG_PRIVATE_KEY` | contents of `pcd-signing-key.priv.asc` (the full ASCII-armored block) |
| `PASSPHRASE` | the passphrase for that key |

`GITHUB_TOKEN` is provided automatically by Actions — no need to add it.

### 3. Register the provider on the Terraform Registry

1. The repository must be **public** (registry requirement). If it is still
   private, change it in **Settings → General → Danger Zone → Change
   visibility**. Confirm with the code owners before doing this — it exposes the
   full history.
2. Sign in to <https://registry.terraform.io> with a GitHub account that is a
   member of the `platform9` org.
3. **Publish → Provider**, authorize the registry GitHub app for the
   `platform9` org, and select `terraform-provider-pcd`. The registry naming
   convention (`terraform-provider-<name>`) yields the address `platform9/pcd`,
   matching `main.go`'s `registry.terraform.io/platform9/pcd`.
4. Under **Settings → GPG Keys** for the namespace, paste the public key from
   step 1 (`pcd-signing-key.pub.asc`).

Once connected, the registry installs a webhook so future GitHub Releases are
ingested automatically.

### 4. Sanity-check the release config (optional but recommended)

With [GoReleaser installed](https://goreleaser.com/install/) locally:

```sh
goreleaser check                    # validates .goreleaser.yml
goreleaser release --snapshot --clean --skip=sign   # dry-run a full build (no publish)
```

The snapshot build drops artifacts in `dist/`; confirm it produces one zip per
`goos/goarch` plus a `..._SHA256SUMS` file and the `..._manifest.json`.

---

## Per-release

### 1. Prepare the changelog

Move the accumulated notes under `## [Unreleased]` in
[`CHANGELOG.md`](CHANGELOG.md) into a new `## [X.Y.Z] - <date>` section and open
a PR. Merge it to `main` before tagging.

### 2. Tag and push

From an up-to-date `main`:

```sh
git checkout main && git pull --ff-only
git tag v0.1.0                      # semver, MUST start with 'v'
git push origin v0.1.0
```

Pushing the tag triggers the `release` workflow.

### 3. Watch the release workflow

```sh
gh run watch      # or: gh run list --workflow=release.yml
```

On success there is a new **GitHub Release** for the tag containing:

- `terraform-provider-pcd_X.Y.Z_<os>_<arch>.zip` for every platform
- `terraform-provider-pcd_X.Y.Z_SHA256SUMS` and its `.sig`
- `terraform-provider-pcd_X.Y.Z_manifest.json`

### 4. Verify on the registry

The registry usually ingests within a few minutes. Confirm the new version
appears at <https://registry.terraform.io/providers/platform9/pcd>, then smoke-test
consumption:

```hcl
terraform {
  required_providers {
    pcd = {
      source  = "platform9/pcd"
      version = "X.Y.Z"
    }
  }
}
```

```sh
terraform init      # should download platform9/pcd X.Y.Z and verify its signature
```

---

## Versioning

Follow [Semantic Versioning](https://semver.org): patch for fixes, minor for
new (backward-compatible) resources/data sources, major for breaking schema or
behavior changes. Pre-1.0 (`v0.y.z`), breaking changes bump the minor.

## Notes

- The GitHub Actions used here are pinned to major-version tags to match
  [`test.yml`](.github/workflows/test.yml). Pin to commit SHAs if the org's
  supply-chain policy requires it.
- Only tags matching `v*` release; branch pushes never publish.
- `terraform-registry-manifest.json` declares Terraform **protocol 6.0**
  (the provider is built on `terraform-plugin-framework`); it is bundled into
  every release by GoReleaser so the registry records the protocol version.
