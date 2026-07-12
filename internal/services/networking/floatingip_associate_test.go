// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package networking_test

import (
	"fmt"
	"os"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"

	"github.com/platform9/terraform-provider-pcd/internal/acctest"
)

// TestAccNetworkingFloatingIPAssociate_basic allocates a floating IP separately
// and binds it to a port with the associate resource. Requires an external
// network; set PCD_ACC_EXTERNAL_NETWORK to its name to run.
func TestAccNetworkingFloatingIPAssociate_basic(t *testing.T) {
	pool := os.Getenv("PCD_ACC_EXTERNAL_NETWORK")
	if pool == "" {
		t.Skip("PCD_ACC_EXTERNAL_NETWORK not set; skipping floating IP associate test (needs an external network)")
	}
	const assocName = "pcd_networking_floatingip_associate.test"

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
				Config: testAccFloatingIPAssociateConfig(pool),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrPair(assocName, "floating_ip_id", "pcd_networking_floatingip.test", "id"),
					resource.TestCheckResourceAttrPair(assocName, "port_id", "pcd_networking_port.test", "id"),
				),
			},
			{ResourceName: assocName, ImportState: true, ImportStateVerify: true},
		},
	})
}

func testAccFloatingIPAssociateConfig(pool string) string {
	return fmt.Sprintf(`
resource "pcd_networking_network" "test" {
  name = "tf-acc-fipa-net"
}

resource "pcd_networking_subnet" "test" {
  name       = "tf-acc-fipa-subnet"
  network_id = pcd_networking_network.test.id
  cidr       = "10.107.0.0/24"
}

resource "pcd_networking_port" "test" {
  name       = "tf-acc-fipa-port"
  network_id = pcd_networking_network.test.id

  fixed_ip = [{
    subnet_id = pcd_networking_subnet.test.id
  }]
}

resource "pcd_networking_floatingip" "test" {
  pool = %q
}

resource "pcd_networking_floatingip_associate" "test" {
  floating_ip_id = pcd_networking_floatingip.test.id
  port_id        = pcd_networking_port.test.id
}
`, pool)
}
