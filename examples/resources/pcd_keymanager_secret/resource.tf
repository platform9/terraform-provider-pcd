resource "pcd_keymanager_secret" "example" {
  name                 = "tf-example-passphrase"
  secret_type          = "passphrase"
  payload              = "super-secret-value"
  payload_content_type = "text/plain"
}
