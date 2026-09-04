# router_id: pcdctl router list. The route's destination and next hop:
#   pcdctl router show <router_id> -c routes
terraform import pcd_networking_router_route.example <router_id>/<destination_cidr>/<next_hop>
