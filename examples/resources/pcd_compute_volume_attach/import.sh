# instance_id: pcdctl server list. attachment_id is the Compute attachment ID, which is the attached volume's ID:
#   pcdctl volume list (the Attached to column names the server)
terraform import pcd_compute_volume_attach.example <instance_id>/<attachment_id>
