// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package networking_test

import (
	"fmt"
	"regexp"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"

	"github.com/platform9/terraform-provider-pcd/internal/acctest"
)

// TestAccNetworkingSubnet_noGateway creates an IPv4 subnet without a gateway,
// checks that adding gateway_ip = "" (the form an imported subnet without a
// gateway reads back as) plans nothing, restores Neutron's default gateway in
// place, removes it again in place,
// shows that omitting no_gateway leaves the subnet as it is, moves the
// gateway to an explicit address, and imports. The data source reads Neutron
// directly, so each step proves the API changed and not only the state. The
// pools leave .1 free: an IPv4 subnet created without a gateway otherwise gets
// pools that start at .1, and Neutron refuses a gateway inside them.
func TestAccNetworkingSubnet_noGateway(t *testing.T) {
	const rn = "pcd_networking_subnet.gw"
	const ds = "data.pcd_networking_subnet.gw"
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { acctest.PreCheck(t) },
		ProtoV6ProviderFactories: acctest.ProtoV6ProviderFactories,
		CheckDestroy: resource.ComposeAggregateTestCheckFunc(
			testAccCheckSubnetDestroy(t),
			testAccCheckNetworkDestroy(t),
		),
		Steps: []resource.TestStep{
			{
				Config: testAccSubnetGatewayConfig(4, "10.138.0.0/24", "10.138.0.10", "10.138.0.200", `no_gateway = true`),
				Check: resource.ComposeAggregateTestCheckFunc(
					testAccCheckSubnetExists(t, rn),
					resource.TestCheckResourceAttr(rn, "no_gateway", "true"),
					resource.TestCheckResourceAttr(rn, "gateway_ip", ""),
					resource.TestCheckResourceAttr(ds, "gateway_ip", ""),
				),
			},
			{
				Config: testAccSubnetGatewayConfig(4, "10.138.0.0/24", "10.138.0.10", "10.138.0.200", "no_gateway = true\n  gateway_ip = \"\""),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()},
				},
			},
			{
				Config: testAccSubnetGatewayConfig(4, "10.138.0.0/24", "10.138.0.10", "10.138.0.200", `no_gateway = false`),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(rn, plancheck.ResourceActionUpdate)},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(rn, "no_gateway", "false"),
					resource.TestCheckResourceAttr(rn, "gateway_ip", "10.138.0.1"),
					resource.TestCheckResourceAttr(ds, "gateway_ip", "10.138.0.1"),
				),
			},
			{
				Config: testAccSubnetGatewayConfig(4, "10.138.0.0/24", "10.138.0.10", "10.138.0.200", `no_gateway = true`),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(rn, plancheck.ResourceActionUpdate)},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(rn, "gateway_ip", ""),
					resource.TestCheckResourceAttr(rn, "no_gateway", "true"),
					resource.TestCheckResourceAttr(ds, "gateway_ip", ""),
				),
			},
			{
				// Unmanaged: removing no_gateway from the configuration plans nothing.
				Config: testAccSubnetGatewayConfig(4, "10.138.0.0/24", "10.138.0.10", "10.138.0.200", ""),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()},
				},
				Check: resource.TestCheckResourceAttr(rn, "no_gateway", "true"),
			},
			{
				Config: testAccSubnetGatewayConfig(4, "10.138.0.0/24", "10.138.0.10", "10.138.0.200", `gateway_ip = "10.138.0.254"`),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(rn, plancheck.ResourceActionUpdate)},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(rn, "gateway_ip", "10.138.0.254"),
					resource.TestCheckResourceAttr(rn, "no_gateway", "false"),
					resource.TestCheckResourceAttr(ds, "gateway_ip", "10.138.0.254"),
				),
			},
			{ResourceName: rn, ImportState: true, ImportStateVerify: true},
		},
	})
}

// TestAccNetworkingSubnet_noGatewayIPv6 checks the IPv6 default: Neutron gives
// an IPv6 subnet its network address as the gateway, not ::1, so restoring the
// gateway must plan fd00:138::.
func TestAccNetworkingSubnet_noGatewayIPv6(t *testing.T) {
	const rn = "pcd_networking_subnet.gw"
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { acctest.PreCheck(t) },
		ProtoV6ProviderFactories: acctest.ProtoV6ProviderFactories,
		CheckDestroy: resource.ComposeAggregateTestCheckFunc(
			testAccCheckSubnetDestroy(t),
			testAccCheckNetworkDestroy(t),
		),
		Steps: []resource.TestStep{
			{
				Config: testAccSubnetGatewayConfig(6, "fd00:138::/64", "fd00:138::10", "fd00:138::ff", `no_gateway = true`),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(rn, "gateway_ip", ""),
					resource.TestCheckResourceAttr(rn, "no_gateway", "true"),
				),
			},
			{
				Config: testAccSubnetGatewayConfig(6, "fd00:138::/64", "fd00:138::10", "fd00:138::ff", `no_gateway = false`),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(rn, plancheck.ResourceActionUpdate)},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(rn, "gateway_ip", "fd00:138::"),
					resource.TestCheckResourceAttr("data.pcd_networking_subnet.gw", "gateway_ip", "fd00:138::"),
				),
			},
		},
	})
}

// TestAccNetworkingSubnet_gatewayConfigRejected checks the plan-time errors.
func TestAccNetworkingSubnet_gatewayConfigRejected(t *testing.T) {
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { acctest.PreCheck(t) },
		ProtoV6ProviderFactories: acctest.ProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config:      testAccSubnetGatewayConfig(4, "10.138.0.0/24", "10.138.0.10", "10.138.0.200", `gateway_ip = ""`),
				PlanOnly:    true,
				ExpectError: regexp.MustCompile(`Invalid gateway_ip`),
			},
			{
				Config:      testAccSubnetGatewayConfig(4, "10.138.0.0/24", "10.138.0.10", "10.138.0.200", "no_gateway = true\n  gateway_ip = \"10.138.0.1\""),
				PlanOnly:    true,
				ExpectError: regexp.MustCompile(`Conflicting gateway settings`),
			},
		},
	})
}

func testAccSubnetGatewayConfig(ipVersion int, cidr, poolStart, poolEnd, gatewayLines string) string {
	return fmt.Sprintf(`
resource "pcd_networking_network" "gw" {
  name = "tf-acc-subnet-gw-net"
}

resource "pcd_networking_subnet" "gw" {
  name       = "tf-acc-subnet-gw"
  network_id = pcd_networking_network.gw.id
  ip_version = %d
  cidr       = %q
  allocation_pools = [
    {
      start = %q
      end   = %q
    }
  ]
  %s
}

data "pcd_networking_subnet" "gw" {
  subnet_id = pcd_networking_subnet.gw.id
}
`, ipVersion, cidr, poolStart, poolEnd, gatewayLines)
}
