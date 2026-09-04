# subnet_id: pcdctl subnet list. The route's destination and next hop:
#   pcdctl subnet show <subnet_id> -c host_routes
terraform import pcd_networking_subnet_route.example <subnet_id>/<destination_cidr>/<next_hop>
