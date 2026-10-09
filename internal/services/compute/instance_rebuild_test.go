// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package compute_test

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/servers"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"

	"github.com/platform9/terraform-provider-pcd/internal/acctest"
)

// testAccRebuildImageName returns a second bootable image, already in the
// image library, to rebuild an instance onto. The test skips without it.
func testAccRebuildImageName(t *testing.T) string {
	t.Helper()
	name := os.Getenv("PCD_ACC_REBUILD_IMAGE_NAME")
	if name == "" {
		t.Skip("PCD_ACC_REBUILD_IMAGE_NAME not set; skipping the rebuild test (needs a second bootable image in the image library)")
	}
	return name
}

// TestAccComputeInstance_rebuild changes the image of a running instance:
// the plan is an in-place update, and the instance keeps its ID and address
// while Nova reports the new image. An import then records the image.
func TestAccComputeInstance_rebuild(t *testing.T) {
	bootImage := testAccBootImageName(t)
	rebuildImage := testAccRebuildImageName(t)
	const rn = "pcd_compute_instance.test"
	var instanceID, ip, bootImageID string
	sameInstance := resource.ComposeAggregateTestCheckFunc(
		resource.TestCheckResourceAttrWith(rn, "id", func(v string) error {
			if v != instanceID {
				return fmt.Errorf("instance was replaced (%s -> %s); an image change should rebuild it in place", instanceID, v)
			}
			return nil
		}),
		resource.TestCheckResourceAttrWith(rn, "access_ip_v4", func(v string) error {
			if v != ip {
				return fmt.Errorf("address changed (%s -> %s); a rebuild keeps the ports", ip, v)
			}
			return nil
		}),
		resource.TestCheckResourceAttr(rn, "status", "ACTIVE"),
	)

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { acctest.PreCheck(t) },
		ProtoV6ProviderFactories: acctest.ProtoV6ProviderFactories,
		CheckDestroy:             testAccCheckInstanceDestroy(t),
		Steps: []resource.TestStep{
			{
				Config: testAccInstanceRebuildConfig(bootImage, rebuildImage, `image_id = data.pcd_images_image.boot.id`),
				Check: resource.ComposeAggregateTestCheckFunc(
					testAccCheckInstanceExists(t, rn),
					testAccCaptureID(rn, &instanceID),
					testAccCaptureAttr(rn, "access_ip_v4", &ip),
					testAccCaptureAttr("data.pcd_images_image.boot", "id", &bootImageID),
					resource.TestCheckResourceAttrPair(rn, "image_id", "data.pcd_images_image.boot", "id"),
				),
			},
			{
				Config: testAccInstanceRebuildConfig(bootImage, rebuildImage, `image_id = data.pcd_images_image.rebuild.id`),
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
					plancheck.ExpectResourceAction(rn, plancheck.ResourceActionUpdate),
				}},
				Check: resource.ComposeAggregateTestCheckFunc(
					sameInstance,
					resource.TestCheckResourceAttrPair(rn, "image_id", "data.pcd_images_image.rebuild", "id"),
					testAccCheckInstanceRunsImage(t, rn, "data.pcd_images_image.rebuild"),
				),
			},
			{
				// Back to the first image, by name this time.
				Config: testAccInstanceRebuildConfig(bootImage, rebuildImage, fmt.Sprintf(`image_name = %q`, bootImage)),
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
					plancheck.ExpectResourceAction(rn, plancheck.ResourceActionUpdate),
				}},
				Check: resource.ComposeAggregateTestCheckFunc(
					sameInstance,
					resource.TestCheckResourceAttrPair(rn, "image_id", "data.pcd_images_image.boot", "id"),
					resource.TestCheckResourceAttr(rn, "image_name", bootImage),
					testAccCheckInstanceRunsImage(t, rn, "data.pcd_images_image.boot"),
				),
			},
			{
				// An import records the image the instance runs. Other inputs
				// (flavor_name, network, ...) are not read back, so only
				// image_id is checked.
				ResourceName: rn,
				ImportState:  true,
				ImportStateCheck: func(states []*terraform.InstanceState) error {
					if len(states) != 1 {
						return fmt.Errorf("imported %d instances, want 1", len(states))
					}
					if got := states[0].Attributes["image_id"]; got != bootImageID {
						return fmt.Errorf("imported image_id %q, want %q", got, bootImageID)
					}
					return nil
				},
			},
		},
	})
}

func testAccInstanceRebuildConfig(bootImage, rebuildImage, imageLine string) string {
	return fmt.Sprintf(`
data "pcd_images_image" "boot" {
  name = %q
}

data "pcd_images_image" "rebuild" {
  name = %q
}

resource "pcd_networking_network" "test" {
  name = "tf-acc-rebuild-net"
}

resource "pcd_networking_subnet" "test" {
  network_id = pcd_networking_network.test.id
  cidr       = "10.130.0.0/24"
}

resource "pcd_compute_instance" "test" {
  name        = "tf-acc-rebuild"
  %s
  flavor_name = "m1.small"

  network {
    uuid = pcd_networking_network.test.id
  }

  depends_on = [pcd_networking_subnet.test]
}
`, bootImage, rebuildImage, imageLine)
}

// testAccCheckInstanceRunsImage checks through Nova that the instance in rn
// runs the image whose ID the data source ds holds.
func testAccCheckInstanceRunsImage(t *testing.T, rn, ds string) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		inst, img := s.RootModule().Resources[rn], s.RootModule().Resources[ds]
		if inst == nil || img == nil {
			return fmt.Errorf("%s or %s not found in state", rn, ds)
		}
		client, err := acctest.LabConfig(t).ComputeV2Client()
		if err != nil {
			return err
		}
		server, err := servers.Get(context.Background(), client, inst.Primary.ID).Extract()
		if err != nil {
			return err
		}
		if got, _ := server.Image["id"].(string); got != img.Primary.ID {
			return fmt.Errorf("instance %s runs image %q, want %q", inst.Primary.ID, got, img.Primary.ID)
		}
		return nil
	}
}
