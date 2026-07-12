// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package compute_test

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/flavors"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"

	"github.com/platform9/terraform-provider-pcd/internal/acctest"
)

func TestAccComputeFlavor_basic(t *testing.T) {
	const rn = "pcd_compute_flavor.test"

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { acctest.PreCheck(t) },
		ProtoV6ProviderFactories: acctest.ProtoV6ProviderFactories,
		CheckDestroy:             testAccCheckFlavorDestroy(t),
		Steps: []resource.TestStep{
			{
				Config: testAccFlavorConfig,
				Check: resource.ComposeAggregateTestCheckFunc(
					testAccCheckFlavorExists(t, rn),
					resource.TestCheckResourceAttr(rn, "name", "tf-acc-flavor"),
					resource.TestCheckResourceAttr(rn, "ram", "256"),
					resource.TestCheckResourceAttr(rn, "vcpus", "1"),
					resource.TestCheckResourceAttr(rn, "disk", "1"),
					resource.TestCheckResourceAttrPair("data.pcd_compute_flavor.by_name", "id", rn, "id"),
					resource.TestCheckResourceAttr("data.pcd_compute_flavor.by_name", "ram", "256"),
				),
			},
			{ResourceName: rn, ImportState: true, ImportStateVerify: true},
		},
	})
}

const testAccFlavorConfig = `
resource "pcd_compute_flavor" "test" {
  name  = "tf-acc-flavor"
  ram   = 256
  vcpus = 1
  disk  = 1
}

data "pcd_compute_flavor" "by_name" {
  name = pcd_compute_flavor.test.name
}
`

func testAccCheckFlavorExists(t *testing.T, n string) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		rs := s.RootModule().Resources[n]
		if rs == nil {
			return fmt.Errorf("not found in state: %s", n)
		}
		client, err := acctest.LabConfig(t).ComputeV2Client()
		if err != nil {
			return err
		}
		if _, err := flavors.Get(context.Background(), client, rs.Primary.ID).Extract(); err != nil {
			return fmt.Errorf("flavor %s not found: %w", rs.Primary.ID, err)
		}
		return nil
	}
}

func testAccCheckFlavorDestroy(t *testing.T) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		client, err := acctest.LabConfig(t).ComputeV2Client()
		if err != nil {
			return err
		}
		for _, rs := range s.RootModule().Resources {
			if rs.Type != "pcd_compute_flavor" {
				continue
			}
			_, err := flavors.Get(context.Background(), client, rs.Primary.ID).Extract()
			if err == nil {
				return fmt.Errorf("flavor %s still exists", rs.Primary.ID)
			}
			if !gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
				return err
			}
		}
		return nil
	}
}
