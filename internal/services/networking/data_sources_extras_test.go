// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package networking_test

import (
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"

	"github.com/platform9/terraform-provider-pcd/internal/acctest"
)

// TestAccNetworkingExtraDataSources_basic creates a network, subnet, port, and
// router, then exercises the port, router, subnet_ids, and port_ids data
// sources against them.
func TestAccNetworkingExtraDataSources_basic(t *testing.T) {
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { acctest.PreCheck(t) },
		ProtoV6ProviderFactories: acctest.ProtoV6ProviderFactories,
		CheckDestroy: resource.ComposeAggregateTestCheckFunc(
			testAccCheckPortDestroy(t),
			testAccCheckSubnetDestroy(t),
			testAccCheckNetworkDestroy(t),
			testAccCheckRouterDestroy(t),
		),
		Steps: []resource.TestStep{
			{
				Config: testAccNetworkingExtraDataSourcesConfig,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrPair("data.pcd_networking_port.by_name", "id", "pcd_networking_port.test", "id"),
					resource.TestCheckResourceAttrPair("data.pcd_networking_port.by_name", "network_id", "pcd_networking_network.test", "id"),
					resource.TestCheckResourceAttrPair("data.pcd_networking_router.by_name", "id", "pcd_networking_router.test", "id"),
					resource.TestCheckResourceAttr("data.pcd_networking_subnet_ids.by_network", "ids.#", "1"),
					resource.TestCheckResourceAttrPair("data.pcd_networking_subnet_ids.by_network", "ids.0", "pcd_networking_subnet.test", "id"),
					resource.TestCheckResourceAttr("data.pcd_networking_port_ids.by_name", "ids.#", "1"),
					resource.TestCheckResourceAttrPair("data.pcd_networking_port_ids.by_name", "ids.0", "pcd_networking_port.test", "id"),
				),
			},
		},
	})
}

const testAccNetworkingExtraDataSourcesConfig = `
resource "pcd_networking_network" "test" {
  name = "tf-acc-dsx-net"
}

resource "pcd_networking_subnet" "test" {
  name       = "tf-acc-dsx-subnet"
  network_id = pcd_networking_network.test.id
  cidr       = "10.103.0.0/24"
}

resource "pcd_networking_port" "test" {
  name       = "tf-acc-dsx-port"
  network_id = pcd_networking_network.test.id

  fixed_ip {
    subnet_id = pcd_networking_subnet.test.id
  }
}

resource "pcd_networking_router" "test" {
  name = "tf-acc-dsx-router"
}

data "pcd_networking_port" "by_name" {
  name = pcd_networking_port.test.name
}

data "pcd_networking_router" "by_name" {
  name = pcd_networking_router.test.name
}

data "pcd_networking_subnet_ids" "by_network" {
  network_id = pcd_networking_network.test.id

  depends_on = [pcd_networking_subnet.test]
}

data "pcd_networking_port_ids" "by_name" {
  name = pcd_networking_port.test.name

  depends_on = [pcd_networking_port.test]
}
`
