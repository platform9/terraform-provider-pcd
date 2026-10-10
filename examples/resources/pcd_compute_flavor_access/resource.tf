# A private flavor is usable only by the projects granted access to it.
resource "pcd_compute_flavor" "gpu" {
  name      = "gpu.large"
  ram       = 16384
  vcpus     = 8
  disk      = 80
  is_public = false
}

variable "gpu_project_ids" {
  type    = set(string)
  default = []
}

resource "pcd_compute_flavor_access" "gpu" {
  for_each  = var.gpu_project_ids
  flavor_id = pcd_compute_flavor.gpu.id
  tenant_id = each.value
}
