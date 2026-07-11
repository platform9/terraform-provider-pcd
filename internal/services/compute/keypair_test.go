// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package compute_test

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/keypairs"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"

	"github.com/platform9/terraform-provider-pcd/internal/acctest"
)

func TestAccComputeKeypair_basic(t *testing.T) {
	const rn = "pcd_compute_keypair.test"

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { acctest.PreCheck(t) },
		ProtoV6ProviderFactories: acctest.ProtoV6ProviderFactories,
		CheckDestroy:             testAccCheckKeypairDestroy(t),
		Steps: []resource.TestStep{
			{
				// No public_key: Nova generates the pair and returns the private key.
				Config: `resource "pcd_compute_keypair" "test" { name = "tf-acc-key" }`,
				Check: resource.ComposeAggregateTestCheckFunc(
					testAccCheckKeypairExists(t, rn),
					resource.TestCheckResourceAttr(rn, "name", "tf-acc-key"),
					resource.TestCheckResourceAttrSet(rn, "public_key"),
					resource.TestCheckResourceAttrSet(rn, "fingerprint"),
					resource.TestCheckResourceAttrSet(rn, "private_key"),
				),
			},
			{
				ResourceName:            rn,
				ImportState:             true,
				ImportStateVerify:       true,
				ImportStateVerifyIgnore: []string{"private_key"},
			},
		},
	})
}

func testAccCheckKeypairExists(t *testing.T, n string) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		rs := s.RootModule().Resources[n]
		if rs == nil {
			return fmt.Errorf("not found in state: %s", n)
		}
		client, err := acctest.LabConfig(t).ComputeV2Client()
		if err != nil {
			return err
		}
		if _, err := keypairs.Get(context.Background(), client, rs.Primary.ID, keypairs.GetOpts{}).Extract(); err != nil {
			return fmt.Errorf("keypair %s not found: %w", rs.Primary.ID, err)
		}
		return nil
	}
}

func testAccCheckKeypairDestroy(t *testing.T) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		client, err := acctest.LabConfig(t).ComputeV2Client()
		if err != nil {
			return err
		}
		for _, rs := range s.RootModule().Resources {
			if rs.Type != "pcd_compute_keypair" {
				continue
			}
			_, err := keypairs.Get(context.Background(), client, rs.Primary.ID, keypairs.GetOpts{}).Extract()
			if err == nil {
				return fmt.Errorf("keypair %s still exists", rs.Primary.ID)
			}
			if !gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
				return err
			}
		}
		return nil
	}
}
