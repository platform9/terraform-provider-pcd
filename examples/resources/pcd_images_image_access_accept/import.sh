# image_id: pcdctl image list --shared. With <image_id> alone, the member is the
# image's only visible member, otherwise the provider's project; give
# <image_id>/<member_id> to name it.
terraform import pcd_images_image_access_accept.example <image_id>
