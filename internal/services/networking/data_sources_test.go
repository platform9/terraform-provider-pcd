// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package networking_test

import (
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"

	"github.com/platform9/terraform-provider-pcd/internal/acctest"
)

// TestAccNetworkingDataSources_byName creates a network, subnet, and security
// group, then looks each up by name and asserts the resolved ID matches.
func TestAccNetworkingDataSources_byName(t *testing.T) {
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { acctest.PreCheck(t) },
		ProtoV6ProviderFactories: acctest.ProtoV6ProviderFactories,
		CheckDestroy: resource.ComposeAggregateTestCheckFunc(
			testAccCheckSubnetDestroy(t),
			testAccCheckNetworkDestroy(t),
			testAccCheckSecgroupDestroy(t),
		),
		Steps: []resource.TestStep{
			{
				Config: testAccNetworkingDataSourcesConfig,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrPair("data.pcd_networking_network.by_name", "id", "pcd_networking_network.test", "id"),
					resource.TestCheckResourceAttrPair("data.pcd_networking_subnet.by_name", "id", "pcd_networking_subnet.test", "id"),
					resource.TestCheckResourceAttrPair("data.pcd_networking_secgroup.by_name", "id", "pcd_networking_secgroup.test", "id"),
					resource.TestCheckResourceAttr("data.pcd_networking_subnet.by_name", "cidr", "10.102.0.0/24"),
				),
			},
		},
	})
}

const testAccNetworkingDataSourcesConfig = `
resource "pcd_networking_network" "test" {
  name = "tf-acc-ds-net"
}

data "pcd_networking_network" "by_name" {
  name = pcd_networking_network.test.name
}

resource "pcd_networking_subnet" "test" {
  name       = "tf-acc-ds-subnet"
  network_id = pcd_networking_network.test.id
  cidr       = "10.102.0.0/24"
}

data "pcd_networking_subnet" "by_name" {
  name = pcd_networking_subnet.test.name
}

resource "pcd_networking_secgroup" "test" {
  name = "tf-acc-ds-sg"
}

data "pcd_networking_secgroup" "by_name" {
  name = pcd_networking_secgroup.test.name
}
`
