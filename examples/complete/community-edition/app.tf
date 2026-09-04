# Day 2, part two: an instance with a persistent volume attached.

# The instance references the network by ID, not the subnet, and PCD needs a
# subnet to give it an address, so the subnet dependency is explicit.
resource "pcd_compute_instance" "workload" {
  name        = "workload-vm"
  image_name  = pcd_images_image.cirros.name
  flavor_name = pcd_compute_flavor.small.name

  security_groups = [pcd_networking_secgroup.workload.name]

  network {
    uuid = pcd_networking_network.workload.id
  }

  depends_on = [pcd_networking_subnet.workload]
}

# The volume lands on the nfs backend through its volume type. The backend
# only accepts volumes once the persistent-storage role has converged.
resource "pcd_blockstorage_volume" "data" {
  name        = "workload-data"
  size        = 1
  volume_type = pcd_blockstorage_volume_type.nfs.name

  depends_on = [pcd_host_cluster_role.storage]
}

resource "pcd_compute_volume_attach" "data" {
  instance_id = pcd_compute_instance.workload.id
  volume_id   = pcd_blockstorage_volume.data.id
}
