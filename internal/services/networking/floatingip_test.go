// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package networking_test

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"testing"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/extensions/layer3/floatingips"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"

	"github.com/platform9/terraform-provider-pcd/internal/acctest"
)

// TestAccNetworkingFloatingIP_basic allocates a floating IP from an external
// network and associates it with a port. It requires an external network to be
// present in the lab; set PCD_ACC_EXTERNAL_NETWORK to its name to run, otherwise
// the test is skipped.
func TestAccNetworkingFloatingIP_basic(t *testing.T) {
	pool := os.Getenv("PCD_ACC_EXTERNAL_NETWORK")
	if pool == "" {
		t.Skip("PCD_ACC_EXTERNAL_NETWORK not set; skipping floating IP test (needs an external network)")
	}
	const fipName = "pcd_networking_floatingip.test"

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { acctest.PreCheck(t) },
		ProtoV6ProviderFactories: acctest.ProtoV6ProviderFactories,
		CheckDestroy: resource.ComposeAggregateTestCheckFunc(
			testAccCheckFloatingIPDestroy(t),
			testAccCheckPortDestroy(t),
			testAccCheckSubnetDestroy(t),
			testAccCheckNetworkDestroy(t),
		),
		Steps: []resource.TestStep{
			{
				Config: testAccFloatingIPConfig(pool, true),
				Check: resource.ComposeAggregateTestCheckFunc(
					testAccCheckFloatingIPExists(t, fipName),
					resource.TestCheckResourceAttr(fipName, "pool", pool),
					resource.TestCheckResourceAttrSet(fipName, "address"),
					resource.TestCheckResourceAttrSet(fipName, "floating_network_id"),
					resource.TestCheckResourceAttrPair(fipName, "port_id", "pcd_networking_port.test", "id"),
				),
			},
			{
				// Disassociate from the port.
				Config: testAccFloatingIPConfig(pool, false),
				Check:  resource.TestCheckResourceAttr(fipName, "port_id", ""),
			},
			{ResourceName: fipName, ImportState: true, ImportStateVerify: true, ImportStateVerifyIgnore: []string{"pool"}},
		},
	})
}

func testAccFloatingIPConfig(pool string, associate bool) string {
	portAssoc := ""
	if associate {
		portAssoc = "port_id = pcd_networking_port.test.id"
	}
	return fmt.Sprintf(`
resource "pcd_networking_network" "test" {
  name = "tf-acc-fip-net"
}

resource "pcd_networking_subnet" "test" {
  name       = "tf-acc-fip-subnet"
  network_id = pcd_networking_network.test.id
  cidr       = "10.104.0.0/24"
}

resource "pcd_networking_port" "test" {
  name       = "tf-acc-fip-port"
  network_id = pcd_networking_network.test.id

  fixed_ip {
    subnet_id = pcd_networking_subnet.test.id
  }
}

resource "pcd_networking_floatingip" "test" {
  pool = %q
  %s
}
`, pool, portAssoc)
}

func testAccCheckFloatingIPExists(t *testing.T, n string) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		rs, ok := s.RootModule().Resources[n]
		if !ok {
			return fmt.Errorf("not found in state: %s", n)
		}
		client, err := acctest.LabConfig(t).NetworkV2Client()
		if err != nil {
			return err
		}
		if _, err := floatingips.Get(context.Background(), client, rs.Primary.ID).Extract(); err != nil {
			return fmt.Errorf("floating IP %s not found via API: %w", rs.Primary.ID, err)
		}
		return nil
	}
}

func testAccCheckFloatingIPDestroy(t *testing.T) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		client, err := acctest.LabConfig(t).NetworkV2Client()
		if err != nil {
			return err
		}
		for _, rs := range s.RootModule().Resources {
			if rs.Type != "pcd_networking_floatingip" {
				continue
			}
			_, err := floatingips.Get(context.Background(), client, rs.Primary.ID).Extract()
			if err == nil {
				return fmt.Errorf("floating IP %s still exists", rs.Primary.ID)
			}
			if !gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
				return fmt.Errorf("unexpected error checking floating IP %s: %w", rs.Primary.ID, err)
			}
		}
		return nil
	}
}
