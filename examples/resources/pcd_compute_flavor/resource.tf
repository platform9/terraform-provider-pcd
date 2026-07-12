resource "pcd_compute_flavor" "example" {
  name  = "tf-example-flavor"
  ram   = 4096
  vcpus = 2
  disk  = 20

  is_public = true
  swap      = 512
  ephemeral = 10

  extra_specs = {
    "hw:cpu_policy" = "dedicated"
  }
}
