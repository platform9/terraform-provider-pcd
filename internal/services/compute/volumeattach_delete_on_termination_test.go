// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package compute_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/volumeattach"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"

	"github.com/platform9/terraform-provider-pcd/internal/acctest"
	"github.com/platform9/terraform-provider-pcd/internal/clients"
)

// TestAccComputeVolumeAttach_deleteOnTermination attaches a volume with
// delete_on_termination = true and checks the flag in state and in Nova (read
// at microversion 2.85). It then drops the attribute from the configuration
// and expects an empty plan, with true still in state and in Nova: an unset
// flag is left alone, where a default of false would plan an update. Next it
// sets false and expects an in-place update, with false in state and in Nova.
// Last it imports the attachment and verifies the imported state against the
// prior one. The instance uses m1.small, whose 20 GB disk holds a typical
// cloud image; m1.tiny's 1 GB disk does not.
func TestAccComputeVolumeAttach_deleteOnTermination(t *testing.T) {
	imageName := testAccBootImageName(t)
	const rn = "pcd_compute_volume_attach.test"

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { acctest.PreCheck(t) },
		ProtoV6ProviderFactories: acctest.ProtoV6ProviderFactories,
		CheckDestroy:             testAccCheckVolumeAttachDestroy(t),
		Steps: []resource.TestStep{
			{
				Config: testAccVolumeAttachDeleteOnTerminationConfig(imageName, "delete_on_termination = true"),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(rn, "delete_on_termination", "true"),
					testAccCheckVolumeAttachDeleteOnTermination(t, rn, true),
				),
			},
			{
				Config: testAccVolumeAttachDeleteOnTerminationConfig(imageName, ""),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(rn, "delete_on_termination", "true"),
					testAccCheckVolumeAttachDeleteOnTermination(t, rn, true),
				),
			},
			{
				Config: testAccVolumeAttachDeleteOnTerminationConfig(imageName, "delete_on_termination = false"),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(rn, plancheck.ResourceActionUpdate)},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(rn, "delete_on_termination", "false"),
					testAccCheckVolumeAttachDeleteOnTermination(t, rn, false),
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

func testAccVolumeAttachDeleteOnTerminationConfig(imageName, flagLine string) string {
	return fmt.Sprintf(`
data "pcd_images_image" "boot" {
  name = %q
}

resource "pcd_networking_network" "test" {
  name = "tf-acc-va-dot-net"
}

resource "pcd_networking_subnet" "test" {
  network_id = pcd_networking_network.test.id
  cidr       = "10.140.0.0/24"
}

resource "pcd_compute_instance" "test" {
  name        = "tf-acc-va-dot-instance"
  image_id    = data.pcd_images_image.boot.id
  flavor_name = "m1.small"

  network {
    uuid = pcd_networking_network.test.id
  }

  depends_on = [pcd_networking_subnet.test]
}

resource "pcd_blockstorage_volume" "test" {
  name = "tf-acc-va-dot-vol"
  size = 1
}

resource "pcd_compute_volume_attach" "test" {
  instance_id = pcd_compute_instance.test.id
  volume_id   = pcd_blockstorage_volume.test.id
  %s
}
`, imageName, flagLine)
}

// testAccCheckVolumeAttachDeleteOnTermination reads the attachment from Nova
// at microversion 2.85, where the flag is part of the response.
func testAccCheckVolumeAttachDeleteOnTermination(t *testing.T, n string, want bool) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		rs := s.RootModule().Resources[n]
		if rs == nil {
			return fmt.Errorf("not found in state: %s", n)
		}
		client, err := acctest.LabConfig(t).ComputeV2Client()
		if err != nil {
			return err
		}
		pinned := *client
		pinned.Microversion = "2.85"
		att, err := clients.RequireObject(volumeattach.Get(context.Background(), &pinned, rs.Primary.Attributes["instance_id"], rs.Primary.ID).Extract())
		if err != nil {
			return fmt.Errorf("reading attachment %s: %w", rs.Primary.ID, err)
		}
		if att.DeleteOnTermination == nil {
			return fmt.Errorf("attachment %s: Nova returned no delete_on_termination at 2.85", rs.Primary.ID)
		}
		if *att.DeleteOnTermination != want {
			return fmt.Errorf("attachment %s: Nova reports delete_on_termination = %v, want %v", rs.Primary.ID, *att.DeleteOnTermination, want)
		}
		return nil
	}
}
