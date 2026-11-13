variable "alice_password" {
  type      = string
  sensitive = true
}

resource "pcd_identity_user" "alice" {
  name     = "alice@example.com"
  password = var.alice_password
}

# Every login of alice now needs her password and a passcode from an
# authenticator app. The provider generates the secret.
resource "pcd_identity_user_mfa" "alice" {
  user_id = pcd_identity_user.alice.id
}

# The URI to enroll in the authenticator app, for example as a QR code.
output "alice_totp_uri" {
  value     = "otpauth://totp/PCD:${pcd_identity_user.alice.name}?secret=${pcd_identity_user_mfa.alice.totp_secret}&issuer=PCD"
  sensitive = true
}
