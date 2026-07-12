// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package networking_test

import (
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"

	"github.com/platform9/terraform-provider-pcd/internal/acctest"
)

// TestAccNetworkingPortSecgroupAssociate_basic attaches security groups to a
// port in shared (enforce = false) mode, then grows the managed set.
func TestAccNetworkingPortSecgroupAssociate_basic(t *testing.T) {
	const assocName = "pcd_networking_port_secgroup_associate.test"

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { acctest.PreCheck(t) },
		ProtoV6ProviderFactories: acctest.ProtoV6ProviderFactories,
		CheckDestroy: resource.ComposeAggregateTestCheckFunc(
			testAccCheckPortDestroy(t),
			testAccCheckSecgroupDestroy(t),
			testAccCheckNetworkDestroy(t),
		),
		Steps: []resource.TestStep{
			{
				Config: testAccPortSecgroupAssociateConfig(false),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(assocName, "enforce", "false"),
					resource.TestCheckResourceAttr(assocName, "security_group_ids.#", "1"),
					resource.TestCheckTypeSetElemAttrPair(assocName, "security_group_ids.*", "pcd_networking_secgroup.a", "id"),
					resource.TestCheckTypeSetElemAttrPair(assocName, "all_security_group_ids.*", "pcd_networking_secgroup.a", "id"),
				),
			},
			{
				Config: testAccPortSecgroupAssociateConfig(true),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(assocName, "security_group_ids.#", "2"),
					resource.TestCheckTypeSetElemAttrPair(assocName, "security_group_ids.*", "pcd_networking_secgroup.b", "id"),
				),
			},
			{
				ResourceName:            assocName,
				ImportState:             true,
				ImportStateVerify:       true,
				ImportStateVerifyIgnore: []string{"security_group_ids"},
			},
		},
	})
}

func testAccPortSecgroupAssociateConfig(both bool) string {
	sgs := "[pcd_networking_secgroup.a.id]"
	if both {
		sgs = "[pcd_networking_secgroup.a.id, pcd_networking_secgroup.b.id]"
	}
	return `
resource "pcd_networking_network" "test" {
  name = "tf-acc-psa-net"
}

resource "pcd_networking_port" "test" {
  name       = "tf-acc-psa-port"
  network_id = pcd_networking_network.test.id
}

resource "pcd_networking_secgroup" "a" {
  name = "tf-acc-psa-sg-a"
}

resource "pcd_networking_secgroup" "b" {
  name = "tf-acc-psa-sg-b"
}

resource "pcd_networking_port_secgroup_associate" "test" {
  port_id            = pcd_networking_port.test.id
  security_group_ids = ` + sgs + `
}
`
}
