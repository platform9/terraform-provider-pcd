// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package blockstorage_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/gophercloud/gophercloud/v2/openstack/blockstorage/v3/volumes"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"

	"github.com/platform9/terraform-provider-pcd/internal/acctest"
)

// TestAccBlockStorageVolumeTransfer_cancel offers a volume, imports the
// offer, and destroys it, which cancels the transfer and returns the volume
// to "available". It needs only the test's own project.
func TestAccBlockStorageVolumeTransfer_cancel(t *testing.T) {
	const offer = "pcd_blockstorage_volume_transfer.test"
	const volume = "pcd_blockstorage_volume.test"
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { acctest.PreCheck(t) },
		ProtoV6ProviderFactories: acctest.ProtoV6ProviderFactories,
		CheckDestroy:             testAccCheckVolumeDestroy(t),
		Steps: []resource.TestStep{
			{
				Config: testAccVolumeTransferOfferConfig(),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrPair(offer, "volume_id", volume, "id"),
					resource.TestCheckResourceAttrSet(offer, "auth_key"),
					resource.TestCheckResourceAttrSet(offer, "source_project_id"),
					resource.TestCheckResourceAttr(offer, "accepted", "false"),
					testAccCheckVolumeStatus(t, volume, "awaiting-transfer"),
				),
			},
			{
				ResourceName:            offer,
				ImportState:             true,
				ImportStateVerify:       true,
				ImportStateVerifyIgnore: []string{"auth_key"},
			},
			{
				// Removing the offer from the configuration cancels it.
				Config: testAccVolumeTransferVolumeOnlyConfig(),
				Check:  testAccCheckVolumeStatus(t, volume, "available"),
			},
		},
	})
}

func testAccVolumeTransferVolumeOnlyConfig() string {
	return `
resource "pcd_blockstorage_volume" "test" {
  name = "tf-acc-xfer-vol"
  size = 1
}
`
}

func testAccVolumeTransferOfferConfig() string {
	return testAccVolumeTransferVolumeOnlyConfig() + `
resource "pcd_blockstorage_volume_transfer" "test" {
  volume_id = pcd_blockstorage_volume.test.id
  name      = "tf-acc-xfer"
}
`
}

// testAccCheckVolumeStatus reads the volume's status from Cinder.
func testAccCheckVolumeStatus(t *testing.T, n, want string) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		rs := s.RootModule().Resources[n]
		if rs == nil {
			return fmt.Errorf("not found in state: %s", n)
		}
		client, err := acctest.LabConfig(t).BlockStorageV3Client()
		if err != nil {
			return err
		}
		vol, err := volumes.Get(context.Background(), client, rs.Primary.ID).Extract()
		if err != nil {
			return fmt.Errorf("reading volume %s: %w", rs.Primary.ID, err)
		}
		if vol.Status != want {
			return fmt.Errorf("volume %s status is %q, want %q", rs.Primary.ID, vol.Status, want)
		}
		return nil
	}
}
