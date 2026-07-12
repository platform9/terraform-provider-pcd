resource "pcd_vpnaas_ike_policy" "example" {
  name                 = "tf-example-ike"
  auth_algorithm       = "sha256"
  encryption_algorithm = "aes-256"
  pfs                  = "group14"

  lifetime = {
    units = "seconds"
    value = 3600
  }
}
