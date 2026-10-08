// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package compute_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/instanceactions"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
	"github.com/hashicorp/terraform-plugin-testing/tfversion"

	"github.com/platform9/terraform-provider-pcd/internal/acctest"
)

// TestAccComputeInstanceRebootAction hard-reboots an instance from an
// action_trigger after terraform_data is created, then again when its input
// changes, and checks Nova's action log for each reboot.
func TestAccComputeInstanceRebootAction(t *testing.T) {
	imageName := testAccBootImageName(t)
	const rn = "pcd_compute_instance.test"

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { acctest.PreCheck(t) },
		ProtoV6ProviderFactories: acctest.ProtoV6ProviderFactories,
		TerraformVersionChecks:   []tfversion.TerraformVersionCheck{tfversion.SkipBelow(tfversion.Version1_14_0)},
		CheckDestroy:             testAccCheckInstanceDestroy(t),
		Steps: []resource.TestStep{
			{
				Config: testAccInstanceRebootActionConfig(imageName, 1),
				Check: resource.ComposeAggregateTestCheckFunc(
					testAccCheckInstanceRebootCount(t, rn, 1),
					testAccCheckInstanceStatus(t, rn, "ACTIVE"),
				),
			},
			{
				Config: testAccInstanceRebootActionConfig(imageName, 2),
				Check: resource.ComposeAggregateTestCheckFunc(
					testAccCheckInstanceRebootCount(t, rn, 2),
					testAccCheckInstanceStatus(t, rn, "ACTIVE"),
				),
			},
		},
	})
}

func testAccInstanceRebootActionConfig(imageName string, generation int) string {
	return fmt.Sprintf(`
data "pcd_images_image" "boot" {
  name = %q
}

resource "pcd_networking_network" "test" {
  name = "tf-acc-reboot-net"
}

resource "pcd_networking_subnet" "test" {
  network_id = pcd_networking_network.test.id
  cidr       = "10.121.0.0/24"
}

resource "pcd_compute_instance" "test" {
  name        = "tf-acc-reboot"
  image_id    = data.pcd_images_image.boot.id
  flavor_name = "m1.small"

  network {
    uuid = pcd_networking_network.test.id
  }

  depends_on = [pcd_networking_subnet.test]
}

action "pcd_compute_instance_reboot" "test" {
  config {
    instance_id = pcd_compute_instance.test.id
    type        = "HARD"
  }
}

resource "terraform_data" "reboot" {
  input      = %d
  depends_on = [pcd_compute_instance.test]

  lifecycle {
    action_trigger {
      events  = [after_create, after_update]
      actions = [action.pcd_compute_instance_reboot.test]
    }
  }
}
`, imageName, generation)
}

// testAccCheckInstanceRebootCount counts the reboots in the instance's Nova
// action log; an action leaves nothing in Terraform state to check.
func testAccCheckInstanceRebootCount(t *testing.T, n string, want int) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		rs := s.RootModule().Resources[n]
		if rs == nil {
			return fmt.Errorf("not found in state: %s", n)
		}
		client, err := acctest.LabConfig(t).ComputeV2Client()
		if err != nil {
			return err
		}
		pages, err := instanceactions.List(client, rs.Primary.ID, nil).AllPages(context.Background())
		if err != nil {
			return err
		}
		all, err := instanceactions.ExtractInstanceActions(pages)
		if err != nil {
			return err
		}
		got := 0
		for _, a := range all {
			if a.Action == "reboot" {
				got++
			}
		}
		if got != want {
			return fmt.Errorf("instance %s has %d reboot actions in its log, want %d", rs.Primary.ID, got, want)
		}
		return nil
	}
}
