resource "pcd_compute_keypair" "example" {
  name       = "tf-example-keypair"
  public_key = "ssh-rsa AAAAB3NzaC1yc2EAAAADAQABAAABgExampleKeyMaterial user@example"
}
