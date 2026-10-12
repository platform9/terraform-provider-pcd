# Find the transfer ID with: pcdctl volume transfer request list
# Cinder returns auth_key only when the offer is created, so it stays empty after an import.
terraform import pcd_blockstorage_volume_transfer.example <transfer_id>
