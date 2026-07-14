# PCD supports a single blueprint per region. The usual workflow is to import
# the existing blueprint (see import.sh) and then manage it here.
resource "pcd_cluster_blueprint" "example" {
  name            = "cluster-1"
  networking_type = "ovn"
  dns_domain_name = "example.local."

  virtual_networking = {
    enabled       = true
    underlay_type = "vlan"
    vnid_range    = "1000:2000"
  }

  # storage_backends_json is omitted here so the imported Cinder backends are
  # preserved. It is required only when creating a brand-new blueprint, and it
  # carries driver credentials (sensitive).
}
