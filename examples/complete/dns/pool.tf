# The pool: what Designate needs to know about the BIND server it pushes zones
# to. pcd_dns_pools_config validates it and renders pools.yaml when Terraform
# reads it, which is at plan time here: every input is a variable.
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

# The file is staged in a directory only the SSH user can open. loafoe/ssh
# creates a copied file world-readable and sets its permissions afterward, in
# a separate command, so the directory is what keeps a PowerDNS token private.
# The path is absolute because loafoe/ssh quotes it when it sets permissions,
# where ~ does not expand: the user's home is dns_host_ssh_home when set, else
# /home/<dns_host_ssh_user>.
locals {
  pools_staging_dir = "${coalesce(var.dns_host_ssh_home, "/home/${var.dns_host_ssh_user}")}/.pcd-dns"
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

  # Runs before the file is copied.
  pre_commands = [
    "umask 077 && mkdir -p ${local.pools_staging_dir} && chmod 700 ${local.pools_staging_dir}",
  ]

  file {
    destination = "${local.pools_staging_dir}/pools.yaml"
    content     = data.pcd_dns_pools_config.default.yaml
    permissions = "0600"
  }

  # /etc/designate may not exist yet (-D creates it); the file is owned by the
  # Designate user because designate-manage runs as that user and reads its own
  # configuration file to reach the database. The staged copy is then removed.
  commands = [
    "sudo install -D -o ${var.designate_user} -m 0600 ${local.pools_staging_dir}/pools.yaml /etc/designate/pools.yaml && rm -f ${local.pools_staging_dir}/pools.yaml",
    "sudo -u ${var.designate_user} ${var.designate_manage} --config-file ${var.designate_conf} pool update --file /etc/designate/pools.yaml",
  ]

  depends_on = [pcd_host_cluster_role.dns]
}
