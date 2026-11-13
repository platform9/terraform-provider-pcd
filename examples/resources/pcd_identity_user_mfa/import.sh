# The import ID is the user's ID (pcdctl user list); the provider finds the user's TOTP credential.
terraform import pcd_identity_user_mfa.example <user_id>
