// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package compute_test

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/servergroups"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"

	"github.com/platform9/terraform-provider-pcd/internal/acctest"
)

func TestAccComputeServergroup_basic(t *testing.T) {
	const rn = "pcd_compute_servergroup.test"

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { acctest.PreCheck(t) },
		ProtoV6ProviderFactories: acctest.ProtoV6ProviderFactories,
		CheckDestroy:             testAccCheckServergroupDestroy(t),
		Steps: []resource.TestStep{
			{
				Config: `resource "pcd_compute_servergroup" "test" {
  name     = "tf-acc-servergroup"
  policies = ["anti-affinity"]
}`,
				Check: resource.ComposeAggregateTestCheckFunc(
					testAccCheckServergroupExists(t, rn),
					resource.TestCheckResourceAttr(rn, "name", "tf-acc-servergroup"),
					resource.TestCheckResourceAttr(rn, "policies.0", "anti-affinity"),
				),
			},
			{ResourceName: rn, ImportState: true, ImportStateVerify: true},
		},
	})
}

func testAccCheckServergroupExists(t *testing.T, n string) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		rs := s.RootModule().Resources[n]
		if rs == nil {
			return fmt.Errorf("not found in state: %s", n)
		}
		client, err := acctest.LabConfig(t).ComputeV2Client()
		if err != nil {
			return err
		}
		if _, err := servergroups.Get(context.Background(), client, rs.Primary.ID).Extract(); err != nil {
			return fmt.Errorf("server group %s not found: %w", rs.Primary.ID, err)
		}
		return nil
	}
}

func testAccCheckServergroupDestroy(t *testing.T) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		client, err := acctest.LabConfig(t).ComputeV2Client()
		if err != nil {
			return err
		}
		for _, rs := range s.RootModule().Resources {
			if rs.Type != "pcd_compute_servergroup" {
				continue
			}
			_, err := servergroups.Get(context.Background(), client, rs.Primary.ID).Extract()
			if err == nil {
				return fmt.Errorf("server group %s still exists", rs.Primary.ID)
			}
			if !gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
				return err
			}
		}
		return nil
	}
}
