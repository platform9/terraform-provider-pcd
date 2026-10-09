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

// TestAccComputeInstanceRebuildAction_basic reimages an instance with its own
// image through the action, triggered after terraform_data is created and
// again after its input changes, and checks Nova's action log for one
// rebuild per run.
func TestAccComputeInstanceRebuildAction_basic(t *testing.T) {
	imageName := testAccBootImageName(t)
	const rn = "pcd_compute_instance.test"
	var instanceID string

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { acctest.PreCheck(t) },
		ProtoV6ProviderFactories: acctest.ProtoV6ProviderFactories,
		TerraformVersionChecks:   []tfversion.TerraformVersionCheck{tfversion.SkipBelow(tfversion.Version1_14_0)},
		CheckDestroy:             testAccCheckInstanceDestroy(t),
		Steps: []resource.TestStep{
			{
				Config: testAccInstanceRebuildActionConfig(imageName, 1),
				Check: resource.ComposeAggregateTestCheckFunc(
					testAccCaptureID(rn, &instanceID),
					resource.TestCheckResourceAttr(rn, "status", "ACTIVE"),
					testAccCheckInstanceRebuilds(t, rn, 1),
				),
			},
			{
				Config: testAccInstanceRebuildActionConfig(imageName, 2),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrWith(rn, "id", func(v string) error {
						if v != instanceID {
							return fmt.Errorf("instance was replaced (%s -> %s)", instanceID, v)
						}
						return nil
					}),
					testAccCheckInstanceRebuilds(t, rn, 2),
				),
			},
		},
	})
}

func testAccInstanceRebuildActionConfig(imageName string, generation int) string {
	return fmt.Sprintf(`
data "pcd_images_image" "boot" {
  name = %q
}

resource "pcd_networking_network" "test" {
  name = "tf-acc-rebuild-action-net"
}

resource "pcd_networking_subnet" "test" {
  network_id = pcd_networking_network.test.id
  cidr       = "10.131.0.0/24"
}

resource "pcd_compute_instance" "test" {
  name        = "tf-acc-rebuild-action"
  image_id    = data.pcd_images_image.boot.id
  flavor_name = "m1.small"

  network {
    uuid = pcd_networking_network.test.id
  }

  depends_on = [pcd_networking_subnet.test]
}

action "pcd_compute_instance_rebuild" "test" {
  config {
    instance_id = pcd_compute_instance.test.id
  }
}

resource "terraform_data" "rebuild" {
  input = "${pcd_compute_instance.test.id}-%d"

  lifecycle {
    action_trigger {
      events  = [after_create, after_update]
      actions = [action.pcd_compute_instance_rebuild.test]
    }
  }
}
`, imageName, generation)
}

// testAccCheckInstanceRebuilds checks Nova's action log for exactly want
// rebuilds of the instance in rn.
func testAccCheckInstanceRebuilds(t *testing.T, rn string, want int) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		rs := s.RootModule().Resources[rn]
		if rs == nil {
			return fmt.Errorf("not found in state: %s", rn)
		}
		client, err := acctest.LabConfig(t).ComputeV2Client()
		if err != nil {
			return err
		}
		pages, err := instanceactions.List(client, rs.Primary.ID, nil).AllPages(context.Background())
		if err != nil {
			return err
		}
		actions, err := instanceactions.ExtractInstanceActions(pages)
		if err != nil {
			return err
		}
		got := 0
		for _, a := range actions {
			if a.Action == "rebuild" {
				got++
			}
		}
		if got != want {
			return fmt.Errorf("instance %s has %d rebuild actions, want %d", rs.Primary.ID, got, want)
		}
		return nil
	}
}
