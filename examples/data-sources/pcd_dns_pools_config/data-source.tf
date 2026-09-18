# The pool file for a host carrying the dns cluster role, backed by BIND9 on
# the same host. designate-mdns (port 5354) on the DNS host is the master the
# BIND server transfers zones from; rndc lets Designate add and remove zones.
data "pcd_dns_pools_config" "default" {
  pools = [{
    name        = "default"
    description = "BIND9 on the DNS host"

    ns_records = [{
      hostname = "ns1.pcd.example.com."
      priority = 1
    }]

    nameservers = [{
      host = "10.0.0.5"
      port = 53
    }]

    targets = [{
      type = "bind9"
      masters = [{
        host = "10.0.0.5"
        port = 5354
      }]
      options = {
        host          = "10.0.0.5"
        port          = 53
        rndc_host     = "10.0.0.5"
        rndc_port     = 953
        rndc_key_file = "/etc/designate/rndc.key"
      }
    }]
  }]
}

# yaml is the file to place at /etc/designate/pools.yaml on the DNS host and
# apply with `designate-manage pool update --file /etc/designate/pools.yaml`;
# id changes with the content, so it can trigger the delivery. The DNS guide
# shows a complete delivery with the ssh provider.
output "pools_yaml" {
  value     = data.pcd_dns_pools_config.default.yaml
  sensitive = true
}
