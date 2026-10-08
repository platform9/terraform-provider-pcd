resource "pcd_networking_network" "example" {
  name = "tf-example-network"
}

# Boot from an image (ephemeral root disk on the hypervisor).
resource "pcd_compute_instance" "from_image" {
  name        = "tf-example-instance"
  image_name  = "Ubuntu-22.04"
  flavor_name = "m1.small"
  key_pair    = "tf-example-key"

  security_groups = ["default"]

  # How PCD's Dynamic Resource Rebalancing (DRR) treats this VM when balancing
  # hosts: normal | low | high | never (excluded). Same as the UI's
  # "Set Migration Priority" action. Updatable in place.
  migration_priority = "high"

  metadata = {
    environment = "dev"
  }

  network {
    uuid = pcd_networking_network.example.id
  }
}

# Keep an instance stopped: the PCD UI's Stop action, as desired state. Set
# "active", "shutoff", "paused" or "suspended". Changing the value starts,
# stops, pauses, unpauses, suspends or resumes the instance in place; leaving
# power_state out lets the UI or anything else power the instance freely.
resource "pcd_compute_instance" "stopped" {
  name        = "tf-example-stopped"
  image_name  = "Ubuntu-22.04"
  flavor_name = "m1.small"
  power_state = "shutoff"

  network {
    uuid = pcd_networking_network.example.id
  }
}

# Boot with a chosen address on the NIC, as the PCD UI's create wizard does
# with a Private IP. The address must be free and inside a subnet of the
# network. Changing it replaces the instance; to add a NIC with a chosen
# address to a running instance, use pcd_compute_interface_attach.
resource "pcd_networking_subnet" "example" {
  network_id = pcd_networking_network.example.id
  cidr       = "10.0.0.0/24"
}

resource "pcd_compute_instance" "fixed_ip" {
  name        = "tf-example-fixed-ip"
  image_name  = "Ubuntu-22.04"
  flavor_name = "m1.small"

  network {
    uuid        = pcd_networking_network.example.id
    fixed_ip_v4 = "10.0.0.50"
  }

  depends_on = [pcd_networking_subnet.example]
}

# A VM on a Layer 2 / "Simple" network. A subnet-less network cannot be booted
# on by network uuid (Nova requires a subnet for that), so the VM attaches
# through a port on it — which is the L2 model anyway: a port on the segment,
# and the guest owns its IP. No DHCP, so addressing comes from cloud-init via
# config drive; no security groups apply (port security is off on the network).
resource "pcd_networking_port" "l2" {
  name       = "tf-example-l2-port"
  network_id = "L2-NETWORK-UUID" # e.g. pcd_networking_network.l2.id
}

resource "pcd_compute_instance" "on_l2" {
  name         = "tf-example-l2"
  image_name   = "Ubuntu-22.04"
  flavor_name  = "m1.small"
  config_drive = true
  user_data    = file("cloud-init-static-ip.yaml")

  network {
    port = pcd_networking_port.l2.id
  }
}

# Boot from a NEW volume created from an image (persistent root disk) — the
# PCD UI's default "New Volume" option. No image_name needed: the block_device
# with boot_index = 0 is the root disk.
resource "pcd_compute_instance" "from_new_volume" {
  name        = "tf-example-bfv"
  flavor_name = "m1.small"

  block_device {
    source_type           = "image"
    uuid                  = "IMAGE-UUID"
    destination_type      = "volume"
    volume_size           = 20
    volume_type           = "ssd"
    boot_index            = 0
    delete_on_termination = true
  }

  network {
    uuid = pcd_networking_network.example.id
  }
}

# Boot from an EXISTING volume (e.g. one restored from a backup).
resource "pcd_compute_instance" "from_existing_volume" {
  name        = "tf-example-existing"
  flavor_name = "m1.small"

  block_device {
    source_type      = "volume"
    uuid             = "VOLUME-UUID"
    destination_type = "volume"
    boot_index       = 0
  }

  network {
    uuid = pcd_networking_network.example.id
  }
}

# Boot from a VOLUME SNAPSHOT (a new volume is cloned from it).
resource "pcd_compute_instance" "from_volume_snapshot" {
  name        = "tf-example-snap"
  flavor_name = "m1.small"

  block_device {
    source_type           = "snapshot"
    uuid                  = "SNAPSHOT-UUID"
    destination_type      = "volume"
    boot_index            = 0
    delete_on_termination = true
  }

  network {
    uuid = pcd_networking_network.example.id
  }
}

# Install from an ISO: a blank target volume to install onto (boot_index 0)
# plus the installer ISO attached as a CD-ROM (boot_index 1) — the PCD UI's
# "Install from ISO" option.
resource "pcd_compute_instance" "from_iso" {
  name        = "tf-example-iso"
  flavor_name = "m1.small"

  block_device {
    source_type           = "blank"
    destination_type      = "volume"
    volume_size           = 40
    boot_index            = 0
    delete_on_termination = false
  }

  block_device {
    source_type           = "image"
    uuid                  = "ISO-IMAGE-UUID"
    destination_type      = "volume"
    volume_size           = 1
    boot_index            = 1
    device_type           = "cdrom"
    delete_on_termination = true
  }

  network {
    uuid = pcd_networking_network.example.id
  }
}

# --- Placement: server groups and scheduler hints ---------------------------
resource "pcd_compute_servergroup" "web" {
  name     = "web-anti-affinity"
  policies = ["anti-affinity"]
}

resource "pcd_compute_instance" "web" {
  count       = 2
  name        = "web-${count.index}"
  image_name  = "cirros-0.6.2"
  flavor_name = "m1.tiny"

  network {
    uuid = pcd_networking_network.example.id
  }

  # Each member lands on a different hypervisor.
  scheduler_hints {
    group = pcd_compute_servergroup.web.id
  }
}
