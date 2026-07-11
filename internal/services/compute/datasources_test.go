// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package compute_test

import (
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"

	"github.com/platform9/terraform-provider-pcd/internal/acctest"
)

func TestAccComputeAvailabilityZones_basic(t *testing.T) {
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { acctest.PreCheck(t) },
		ProtoV6ProviderFactories: acctest.ProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: `data "pcd_compute_availability_zones" "zones" {}`,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrSet("data.pcd_compute_availability_zones.zones", "names.0"),
				),
			},
		},
	})
}

func TestAccComputeKeypairDataSource_basic(t *testing.T) {
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { acctest.PreCheck(t) },
		ProtoV6ProviderFactories: acctest.ProtoV6ProviderFactories,
		CheckDestroy:             testAccCheckKeypairDestroy(t),
		Steps: []resource.TestStep{
			{
				Config: `
resource "pcd_compute_keypair" "test" {
  name = "tf-acc-kp-ds"
}

data "pcd_compute_keypair" "by_name" {
  name = pcd_compute_keypair.test.name
}
`,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrPair("data.pcd_compute_keypair.by_name", "id", "pcd_compute_keypair.test", "name"),
					resource.TestCheckResourceAttrSet("data.pcd_compute_keypair.by_name", "public_key"),
					resource.TestCheckResourceAttrSet("data.pcd_compute_keypair.by_name", "fingerprint"),
				),
			},
		},
	})
}
