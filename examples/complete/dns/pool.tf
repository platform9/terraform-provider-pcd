# The pool: what Designate needs to know about the BIND server it pushes zones
# to. pcd_dns_pools_config validates it at plan time and renders pools.yaml.
data "pcd_dns_pools_config" "default" {
  pools = [{
    name        = "default"
    description = "BIND9 on the DNS host"

    ns_records = [{
      hostname = var.ns_hostname
      priority = 1
    }]

    nameservers = [{
      host = var.dns_host_ip
      port = var.bind_port
    }]

    targets = [{
      type = "bind9"
      masters = [{
        host = var.dns_host_ip
        port = 5354
      }]
      options = {
        host          = var.dns_host_ip
        port          = var.bind_port
        rndc_host     = var.dns_host_ip
        rndc_port     = 953
        rndc_key_file = var.rndc_key_file
      }
    }]
  }]
}

# Delivery: copy the file to the DNS host and apply it. The trigger is the
# file's hash, so this re-runs exactly when the pool changes. Without --delete
# a pool removed from the configuration stays in Designate; delete it by hand.
resource "ssh_resource" "pools" {
  host        = var.dns_host_ip
  user        = var.dns_host_ssh_user
  private_key = file(var.dns_host_ssh_key)
  agent       = false
  timeout     = "5m"
  retry_delay = "10s"

  triggers = {
    pools = data.pcd_dns_pools_config.default.id
  }

  file {
    destination = "/tmp/pools.yaml"
    content     = data.pcd_dns_pools_config.default.yaml
    permissions = "0600"
  }

  # /etc/designate may not exist yet (-D creates it); the file is owned by the
  # Designate user because designate-manage runs as that user and reads its own
  # configuration file to reach the database.
  commands = [
    "sudo install -D -o ${var.designate_user} -m 0600 /tmp/pools.yaml /etc/designate/pools.yaml && rm -f /tmp/pools.yaml",
    "sudo -u ${var.designate_user} ${var.designate_manage} --config-file ${var.designate_conf} pool update --file /etc/designate/pools.yaml",
  ]

  depends_on = [pcd_host_cluster_role.dns]
}
