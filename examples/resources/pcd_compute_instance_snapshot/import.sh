# The snapshot's image ID: pcdctl image list (snapshots of an instance carry its ID in the instance_uuid property)
terraform import pcd_compute_instance_snapshot.example <image_id>
