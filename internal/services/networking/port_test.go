// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package networking_test

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/ports"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"

	"github.com/platform9/terraform-provider-pcd/internal/acctest"
)

func TestAccNetworkingPort_basic(t *testing.T) {
	const portName = "pcd_networking_port.test"

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { acctest.PreCheck(t) },
		ProtoV6ProviderFactories: acctest.ProtoV6ProviderFactories,
		CheckDestroy: resource.ComposeAggregateTestCheckFunc(
			testAccCheckPortDestroy(t),
			testAccCheckSubnetDestroy(t),
			testAccCheckNetworkDestroy(t),
		),
		Steps: []resource.TestStep{
			{
				Config: testAccPortConfig("tf-acc-port", true),
				Check: resource.ComposeAggregateTestCheckFunc(
					testAccCheckPortExists(t, portName),
					resource.TestCheckResourceAttr(portName, "name", "tf-acc-port"),
					resource.TestCheckResourceAttr(portName, "admin_state_up", "true"),
					resource.TestCheckResourceAttrPair(portName, "network_id", "pcd_networking_network.test", "id"),
					resource.TestCheckResourceAttr(portName, "all_fixed_ips.#", "1"),
					resource.TestCheckResourceAttr(portName, "tags.#", "1"),
					resource.TestCheckResourceAttrSet(portName, "mac_address"),
					resource.TestCheckResourceAttrSet(portName, "status"),
				),
			},
			{
				Config: testAccPortConfig("tf-acc-port-updated", false),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(portName, "name", "tf-acc-port-updated"),
					resource.TestCheckResourceAttr(portName, "admin_state_up", "false"),
				),
			},
			{
				ResourceName:            portName,
				ImportState:             true,
				ImportStateVerify:       true,
				ImportStateVerifyIgnore: []string{"fixed_ip", "allowed_address_pairs"},
			},
		},
	})
}

func testAccPortConfig(name string, adminUp bool) string {
	return fmt.Sprintf(`
resource "pcd_networking_network" "test" {
  name           = "tf-acc-port-net"
  admin_state_up = true
}

resource "pcd_networking_subnet" "test" {
  name       = "tf-acc-port-subnet"
  network_id = pcd_networking_network.test.id
  cidr       = "10.101.0.0/24"
  ip_version = 4
}

resource "pcd_networking_port" "test" {
  name           = %q
  network_id     = pcd_networking_network.test.id
  admin_state_up = %t
  tags           = ["tf-acc"]

  fixed_ip = [{
    subnet_id = pcd_networking_subnet.test.id
  }]
}
`, name, adminUp)
}

func testAccCheckPortExists(t *testing.T, n string) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		rs, ok := s.RootModule().Resources[n]
		if !ok {
			return fmt.Errorf("not found in state: %s", n)
		}
		client, err := acctest.LabConfig(t).NetworkV2Client()
		if err != nil {
			return err
		}
		if _, err := ports.Get(context.Background(), client, rs.Primary.ID).Extract(); err != nil {
			return fmt.Errorf("port %s not found via API: %w", rs.Primary.ID, err)
		}
		return nil
	}
}

func testAccCheckPortDestroy(t *testing.T) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		client, err := acctest.LabConfig(t).NetworkV2Client()
		if err != nil {
			return err
		}
		for _, rs := range s.RootModule().Resources {
			if rs.Type != "pcd_networking_port" {
				continue
			}
			_, err := ports.Get(context.Background(), client, rs.Primary.ID).Extract()
			if err == nil {
				return fmt.Errorf("port %s still exists", rs.Primary.ID)
			}
			if !gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
				return fmt.Errorf("unexpected error checking port %s: %w", rs.Primary.ID, err)
			}
		}
		return nil
	}
}
