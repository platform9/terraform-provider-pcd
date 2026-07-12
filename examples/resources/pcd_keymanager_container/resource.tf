resource "pcd_keymanager_secret" "example" {
  name                 = "tf-example-tls-key"
  payload              = "-----BEGIN PRIVATE KEY-----\n...\n-----END PRIVATE KEY-----"
  payload_content_type = "text/plain"
}

resource "pcd_keymanager_container" "example" {
  name = "tf-example-container"
  type = "generic"

  secret_refs {
    name       = "private_key"
    secret_ref = pcd_keymanager_secret.example.secret_ref
  }
}
