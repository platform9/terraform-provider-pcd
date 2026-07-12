// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package networking_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/extensions/layer3/routers"
	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/subnets"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"

	"github.com/platform9/terraform-provider-pcd/internal/acctest"
)

func TestAccNetworkingRouterRoute_basic(t *testing.T) {
	const rrName = "pcd_networking_router_route.test"

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { acctest.PreCheck(t) },
		ProtoV6ProviderFactories: acctest.ProtoV6ProviderFactories,
		CheckDestroy: resource.ComposeAggregateTestCheckFunc(
			testAccCheckRouterInterfaceDestroy(t),
			testAccCheckSubnetDestroy(t),
			testAccCheckNetworkDestroy(t),
			testAccCheckRouterDestroy(t),
		),
		Steps: []resource.TestStep{
			{
				Config: testAccRouterRouteConfig,
				Check: resource.ComposeAggregateTestCheckFunc(
					testAccCheckRouterRoutePresent(t, "pcd_networking_router.test", "10.200.0.0/24", "10.105.0.5"),
					resource.TestCheckResourceAttr(rrName, "destination_cidr", "10.200.0.0/24"),
					resource.TestCheckResourceAttr(rrName, "next_hop", "10.105.0.5"),
				),
			},
			{ResourceName: rrName, ImportState: true, ImportStateVerify: true},
		},
	})
}

const testAccRouterRouteConfig = `
resource "pcd_networking_network" "test" {
  name = "tf-acc-rr-net"
}

resource "pcd_networking_subnet" "test" {
  name       = "tf-acc-rr-subnet"
  network_id = pcd_networking_network.test.id
  cidr       = "10.105.0.0/24"
}

resource "pcd_networking_router" "test" {
  name = "tf-acc-rr-router"
}

resource "pcd_networking_router_interface" "test" {
  router_id = pcd_networking_router.test.id
  subnet_id = pcd_networking_subnet.test.id
}

resource "pcd_networking_router_route" "test" {
  router_id        = pcd_networking_router.test.id
  destination_cidr = "10.200.0.0/24"
  next_hop         = "10.105.0.5"

  depends_on = [pcd_networking_router_interface.test]
}
`

func TestAccNetworkingSubnetRoute_basic(t *testing.T) {
	const srName = "pcd_networking_subnet_route.test"

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { acctest.PreCheck(t) },
		ProtoV6ProviderFactories: acctest.ProtoV6ProviderFactories,
		CheckDestroy: resource.ComposeAggregateTestCheckFunc(
			testAccCheckSubnetDestroy(t),
			testAccCheckNetworkDestroy(t),
		),
		Steps: []resource.TestStep{
			{
				Config: testAccSubnetRouteConfig,
				Check: resource.ComposeAggregateTestCheckFunc(
					testAccCheckSubnetHostRoutePresent(t, "pcd_networking_subnet.test", "10.210.0.0/24", "10.106.0.5"),
					resource.TestCheckResourceAttr(srName, "destination_cidr", "10.210.0.0/24"),
					resource.TestCheckResourceAttr(srName, "next_hop", "10.106.0.5"),
				),
			},
			{ResourceName: srName, ImportState: true, ImportStateVerify: true},
		},
	})
}

const testAccSubnetRouteConfig = `
resource "pcd_networking_network" "test" {
  name = "tf-acc-sr-net"
}

resource "pcd_networking_subnet" "test" {
  name       = "tf-acc-sr-subnet"
  network_id = pcd_networking_network.test.id
  cidr       = "10.106.0.0/24"
}

resource "pcd_networking_subnet_route" "test" {
  subnet_id        = pcd_networking_subnet.test.id
  destination_cidr = "10.210.0.0/24"
  next_hop         = "10.106.0.5"
}
`

func testAccCheckRouterRoutePresent(t *testing.T, routerRes, dst, hop string) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		rs, ok := s.RootModule().Resources[routerRes]
		if !ok {
			return fmt.Errorf("not found in state: %s", routerRes)
		}
		client, err := acctest.LabConfig(t).NetworkV2Client()
		if err != nil {
			return err
		}
		router, err := routers.Get(context.Background(), client, rs.Primary.ID).Extract()
		if err != nil {
			return err
		}
		for _, rt := range router.Routes {
			if rt.DestinationCIDR == dst && rt.NextHop == hop {
				return nil
			}
		}
		return fmt.Errorf("route %s -> %s not found on router %s", dst, hop, rs.Primary.ID)
	}
}

func testAccCheckSubnetHostRoutePresent(t *testing.T, subnetRes, dst, hop string) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		rs, ok := s.RootModule().Resources[subnetRes]
		if !ok {
			return fmt.Errorf("not found in state: %s", subnetRes)
		}
		client, err := acctest.LabConfig(t).NetworkV2Client()
		if err != nil {
			return err
		}
		subnet, err := subnets.Get(context.Background(), client, rs.Primary.ID).Extract()
		if err != nil {
			return err
		}
		for _, rt := range subnet.HostRoutes {
			if rt.DestinationCIDR == dst && rt.NextHop == hop {
				return nil
			}
		}
		return fmt.Errorf("host route %s -> %s not found on subnet %s", dst, hop, rs.Primary.ID)
	}
}
