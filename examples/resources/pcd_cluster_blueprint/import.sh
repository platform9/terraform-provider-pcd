# PCD keeps one blueprint per region; its name is listed by GET /resmgr/v2/blueprint
#   (see the Importing guide). The import ID is the name, not a UUID.
terraform import pcd_cluster_blueprint.example <blueprint_name>
