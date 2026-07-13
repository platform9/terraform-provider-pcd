// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package networking_test

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/networks"
	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/subnets"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"

	"github.com/platform9/terraform-provider-pcd/internal/acctest"
)

func TestAccNetworkingNetworkAndSubnet_basic(t *testing.T) {
	const netName = "pcd_networking_network.test"
	const subName = "pcd_networking_subnet.test"

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { acctest.PreCheck(t) },
		ProtoV6ProviderFactories: acctest.ProtoV6ProviderFactories,
		CheckDestroy: resource.ComposeAggregateTestCheckFunc(
			testAccCheckSubnetDestroy(t),
			testAccCheckNetworkDestroy(t),
		),
		Steps: []resource.TestStep{
			{
				Config: testAccNetworkSubnetConfig("tf-acc-net", "10.100.0.0/24"),
				Check: resource.ComposeAggregateTestCheckFunc(
					testAccCheckNetworkExists(t, netName),
					testAccCheckSubnetExists(t, subName),
					resource.TestCheckResourceAttr(netName, "name", "tf-acc-net"),
					resource.TestCheckResourceAttr(netName, "admin_state_up", "true"),
					resource.TestCheckResourceAttr(subName, "cidr", "10.100.0.0/24"),
					resource.TestCheckResourceAttr(subName, "enable_dhcp", "true"),
					resource.TestCheckResourceAttrSet(subName, "gateway_ip"),
					resource.TestCheckResourceAttrPair(subName, "network_id", netName, "id"),
				),
			},
			{
				Config: testAccNetworkSubnetConfig("tf-acc-net-updated", "10.100.0.0/24"),
				Check:  resource.TestCheckResourceAttr(netName, "name", "tf-acc-net-updated"),
			},
			{ResourceName: netName, ImportState: true, ImportStateVerify: true},
			{ResourceName: subName, ImportState: true, ImportStateVerify: true},
		},
	})
}

func testAccNetworkSubnetConfig(netName, cidr string) string {
	return fmt.Sprintf(`
resource "pcd_networking_network" "test" {
  name           = %q
  admin_state_up = true
}

resource "pcd_networking_subnet" "test" {
  name       = "tf-acc-subnet"
  network_id = pcd_networking_network.test.id
  cidr       = %q
  ip_version = 4
}
`, netName, cidr)
}

func testAccCheckNetworkExists(t *testing.T, n string) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		rs, ok := s.RootModule().Resources[n]
		if !ok {
			return fmt.Errorf("not found in state: %s", n)
		}
		client, err := acctest.LabConfig(t).NetworkV2Client()
		if err != nil {
			return err
		}
		if _, err := networks.Get(context.Background(), client, rs.Primary.ID).Extract(); err != nil {
			return fmt.Errorf("network %s not found via API: %w", rs.Primary.ID, err)
		}
		return nil
	}
}

func testAccCheckSubnetExists(t *testing.T, n string) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		rs, ok := s.RootModule().Resources[n]
		if !ok {
			return fmt.Errorf("not found in state: %s", n)
		}
		client, err := acctest.LabConfig(t).NetworkV2Client()
		if err != nil {
			return err
		}
		if _, err := subnets.Get(context.Background(), client, rs.Primary.ID).Extract(); err != nil {
			return fmt.Errorf("subnet %s not found via API: %w", rs.Primary.ID, err)
		}
		return nil
	}
}

func testAccCheckNetworkDestroy(t *testing.T) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		client, err := acctest.LabConfig(t).NetworkV2Client()
		if err != nil {
			return err
		}
		for addr, rs := range s.RootModule().Resources {
			// Skip data sources (e.g. a data.pcd_networking_network lookup of a
			// pre-existing external network); only managed networks are destroyed.
			if strings.HasPrefix(addr, "data.") || rs.Type != "pcd_networking_network" {
				continue
			}
			_, err := networks.Get(context.Background(), client, rs.Primary.ID).Extract()
			if err == nil {
				return fmt.Errorf("network %s still exists", rs.Primary.ID)
			}
			if !gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
				return fmt.Errorf("unexpected error checking network %s: %w", rs.Primary.ID, err)
			}
		}
		return nil
	}
}

func testAccCheckSubnetDestroy(t *testing.T) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		client, err := acctest.LabConfig(t).NetworkV2Client()
		if err != nil {
			return err
		}
		for _, rs := range s.RootModule().Resources {
			if rs.Type != "pcd_networking_subnet" {
				continue
			}
			_, err := subnets.Get(context.Background(), client, rs.Primary.ID).Extract()
			if err == nil {
				return fmt.Errorf("subnet %s still exists", rs.Primary.ID)
			}
			if !gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
				return fmt.Errorf("unexpected error checking subnet %s: %w", rs.Primary.ID, err)
			}
		}
		return nil
	}
}
