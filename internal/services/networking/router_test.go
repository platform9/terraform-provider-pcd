// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package networking_test

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/extensions/layer3/routers"
	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/ports"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"

	"github.com/platform9/terraform-provider-pcd/internal/acctest"
)

func TestAccNetworkingRouterAndInterface_basic(t *testing.T) {
	const rtrName = "pcd_networking_router.test"
	const ifaceName = "pcd_networking_router_interface.test"

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { acctest.PreCheck(t) },
		ProtoV6ProviderFactories: acctest.ProtoV6ProviderFactories,
		CheckDestroy: resource.ComposeAggregateTestCheckFunc(
			testAccCheckRouterInterfaceDestroy(t),
			testAccCheckRouterDestroy(t),
			testAccCheckSubnetDestroy(t),
			testAccCheckNetworkDestroy(t),
		),
		Steps: []resource.TestStep{
			{
				Config: testAccRouterConfig("tf-acc-router"),
				Check: resource.ComposeAggregateTestCheckFunc(
					testAccCheckRouterExists(t, rtrName),
					testAccCheckRouterInterfaceExists(t, ifaceName),
					resource.TestCheckResourceAttr(rtrName, "name", "tf-acc-router"),
					resource.TestCheckResourceAttrPair(ifaceName, "router_id", rtrName, "id"),
					resource.TestCheckResourceAttrSet(ifaceName, "port_id"),
				),
			},
			{
				Config: testAccRouterConfig("tf-acc-router-updated"),
				Check:  resource.TestCheckResourceAttr(rtrName, "name", "tf-acc-router-updated"),
			},
			{ResourceName: rtrName, ImportState: true, ImportStateVerify: true},
			{ResourceName: ifaceName, ImportState: true, ImportStateVerify: true},
		},
	})
}

func testAccRouterConfig(name string) string {
	return fmt.Sprintf(`
resource "pcd_networking_network" "test" {
  name = "tf-acc-rtr-net"
}

resource "pcd_networking_subnet" "test" {
  network_id = pcd_networking_network.test.id
  cidr       = "10.101.0.0/24"
}

resource "pcd_networking_router" "test" {
  name = %q
}

resource "pcd_networking_router_interface" "test" {
  router_id = pcd_networking_router.test.id
  subnet_id = pcd_networking_subnet.test.id
}
`, name)
}

func testAccCheckRouterExists(t *testing.T, n string) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		rs := s.RootModule().Resources[n]
		if rs == nil {
			return fmt.Errorf("not found in state: %s", n)
		}
		client, err := acctest.LabConfig(t).NetworkV2Client()
		if err != nil {
			return err
		}
		if _, err := routers.Get(context.Background(), client, rs.Primary.ID).Extract(); err != nil {
			return fmt.Errorf("router %s not found: %w", rs.Primary.ID, err)
		}
		return nil
	}
}

func testAccCheckRouterInterfaceExists(t *testing.T, n string) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		rs := s.RootModule().Resources[n]
		if rs == nil {
			return fmt.Errorf("not found in state: %s", n)
		}
		client, err := acctest.LabConfig(t).NetworkV2Client()
		if err != nil {
			return err
		}
		if _, err := ports.Get(context.Background(), client, rs.Primary.ID).Extract(); err != nil {
			return fmt.Errorf("router interface port %s not found: %w", rs.Primary.ID, err)
		}
		return nil
	}
}

func testAccCheckRouterDestroy(t *testing.T) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		client, err := acctest.LabConfig(t).NetworkV2Client()
		if err != nil {
			return err
		}
		for _, rs := range s.RootModule().Resources {
			if rs.Type != "pcd_networking_router" {
				continue
			}
			_, err := routers.Get(context.Background(), client, rs.Primary.ID).Extract()
			if err == nil {
				return fmt.Errorf("router %s still exists", rs.Primary.ID)
			}
			if !gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
				return err
			}
		}
		return nil
	}
}

func testAccCheckRouterInterfaceDestroy(t *testing.T) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		client, err := acctest.LabConfig(t).NetworkV2Client()
		if err != nil {
			return err
		}
		for _, rs := range s.RootModule().Resources {
			if rs.Type != "pcd_networking_router_interface" {
				continue
			}
			_, err := ports.Get(context.Background(), client, rs.Primary.ID).Extract()
			if err == nil {
				return fmt.Errorf("router interface port %s still exists", rs.Primary.ID)
			}
			if !gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
				return err
			}
		}
		return nil
	}
}
