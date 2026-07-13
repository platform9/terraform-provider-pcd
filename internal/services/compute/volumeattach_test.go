// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package compute_test

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/volumeattach"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"

	"github.com/platform9/terraform-provider-pcd/internal/acctest"
)

// TestAccComputeVolumeAttach_basic boots an instance, creates a Cinder volume,
// and attaches the volume to the instance. Requires both a bootable instance and
// a working Cinder backend (lab-blocked on the current CE lab).
func TestAccComputeVolumeAttach_basic(t *testing.T) {
	imageName := testAccBootImageName(t)
	const rn = "pcd_compute_volume_attach.test"

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { acctest.PreCheck(t) },
		ProtoV6ProviderFactories: acctest.ProtoV6ProviderFactories,
		CheckDestroy:             testAccCheckVolumeAttachDestroy(t),
		Steps: []resource.TestStep{
			{
				Config: testAccVolumeAttachConfig(imageName),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrPair(rn, "volume_id", "pcd_blockstorage_volume.test", "id"),
					resource.TestCheckResourceAttrPair(rn, "instance_id", "pcd_compute_instance.test", "id"),
					resource.TestCheckResourceAttrSet(rn, "device"),
				),
			},
			{
				ResourceName:      rn,
				ImportState:       true,
				ImportStateVerify: true,
				ImportStateIdFunc: func(s *terraform.State) (string, error) {
					rs := s.RootModule().Resources[rn]
					if rs == nil {
						return "", fmt.Errorf("not found in state: %s", rn)
					}
					return rs.Primary.Attributes["instance_id"] + "/" + rs.Primary.ID, nil
				},
			},
		},
	})
}

func testAccVolumeAttachConfig(imageName string) string {
	return fmt.Sprintf(`
data "pcd_images_image" "boot" {
  name = %q
}

resource "pcd_networking_network" "test" {
  name = "tf-acc-va-net"
}

resource "pcd_networking_subnet" "test" {
  network_id = pcd_networking_network.test.id
  cidr       = "10.116.0.0/24"
}

resource "pcd_compute_instance" "test" {
  name        = "tf-acc-va-instance"
  image_id    = data.pcd_images_image.boot.id
  flavor_name = "m1.tiny"

  network {
    uuid = pcd_networking_network.test.id
  }

  depends_on = [pcd_networking_subnet.test]
}

resource "pcd_blockstorage_volume" "test" {
  name = "tf-acc-va-vol"
  size = 1
}

resource "pcd_compute_volume_attach" "test" {
  instance_id = pcd_compute_instance.test.id
  volume_id   = pcd_blockstorage_volume.test.id
}
`, imageName)
}

func testAccCheckVolumeAttachDestroy(t *testing.T) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		client, err := acctest.LabConfig(t).ComputeV2Client()
		if err != nil {
			return err
		}
		for _, rs := range s.RootModule().Resources {
			if rs.Type != "pcd_compute_volume_attach" {
				continue
			}
			instanceID := rs.Primary.Attributes["instance_id"]
			_, err := volumeattach.Get(context.Background(), client, instanceID, rs.Primary.ID).Extract()
			if err == nil {
				return fmt.Errorf("volume attachment %s on instance %s still exists", rs.Primary.ID, instanceID)
			}
			if !gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
				return err
			}
		}
		return nil
	}
}
