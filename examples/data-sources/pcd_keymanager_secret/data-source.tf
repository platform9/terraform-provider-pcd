data "pcd_keymanager_secret" "example" {
  name                 = "tf-example-passphrase"
  payload_content_type = "text/plain"
}
