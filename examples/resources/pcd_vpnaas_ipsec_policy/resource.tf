resource "pcd_vpnaas_ipsec_policy" "example" {
  name                 = "tf-example-ipsec"
  auth_algorithm       = "sha256"
  encryption_algorithm = "aes-256"
  pfs                  = "group14"
}
