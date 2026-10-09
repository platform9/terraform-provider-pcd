// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package compute_test

import (
	"fmt"
	"os"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"

	"github.com/platform9/terraform-provider-pcd/internal/acctest"
)

// TestAccComputeInstanceRescue_basic rescues an instance with its own image,
// imports the rescue, and unrescues it by removing the resource: the
// instance is the same one, ACTIVE again.
func TestAccComputeInstanceRescue_basic(t *testing.T) {
	imageName := testAccBootImageName(t)
	const rn = "pcd_compute_instance_rescue.test"
	var instanceID string

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { acctest.PreCheck(t) },
		ProtoV6ProviderFactories: acctest.ProtoV6ProviderFactories,
		CheckDestroy:             testAccCheckInstanceDestroy(t),
		Steps: []resource.TestStep{
			{
				Config: testAccInstanceRescueConfig(imageName, true),
				Check: resource.ComposeAggregateTestCheckFunc(
					testAccCaptureID("pcd_compute_instance.test", &instanceID),
					resource.TestCheckResourceAttrPair(rn, "instance_id", "pcd_compute_instance.test", "id"),
					testAccCheckInstanceStatus(t, "pcd_compute_instance.test", "RESCUE"),
				),
			},
			{ResourceName: rn, ImportState: true, ImportStateVerify: true},
			{
				Config: testAccInstanceRescueConfig(imageName, false),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrWith("pcd_compute_instance.test", "id", func(v string) error {
						if v != instanceID {
							return fmt.Errorf("instance was replaced (%s -> %s)", instanceID, v)
						}
						return nil
					}),
					testAccCheckInstanceStatus(t, "pcd_compute_instance.test", "ACTIVE"),
				),
			},
		},
	})
}

// TestAccComputeInstanceRescue_volumeBacked rescues an instance that boots
// from a volume, which needs microversion 2.87 and a rescue image carrying
// hw_rescue_device and hw_rescue_bus (PCD_ACC_RESCUE_IMAGE_ID).
func TestAccComputeInstanceRescue_volumeBacked(t *testing.T) {
	imageName := testAccBootImageName(t)
	if os.Getenv("PCD_ACC_VOLUME_BOOT") == "" {
		t.Skip("PCD_ACC_VOLUME_BOOT not set; skipping a test that boots from a Cinder volume")
	}
	rescueImage := os.Getenv("PCD_ACC_RESCUE_IMAGE_ID")
	if rescueImage == "" {
		t.Skip("PCD_ACC_RESCUE_IMAGE_ID not set; skipping the volume-backed rescue (needs an image with hw_rescue_device and hw_rescue_bus)")
	}
	const rn = "pcd_compute_instance_rescue.test"

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { acctest.PreCheck(t) },
		ProtoV6ProviderFactories: acctest.ProtoV6ProviderFactories,
		CheckDestroy:             testAccCheckInstanceDestroy(t),
		Steps: []resource.TestStep{
			{
				Config: testAccInstanceRescueVolumeConfig(imageName, rescueImage),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(rn, "rescue_image_id", rescueImage),
					testAccCheckInstanceStatus(t, "pcd_compute_instance.test", "RESCUE"),
				),
			},
			{ResourceName: rn, ImportState: true, ImportStateVerify: true, ImportStateVerifyIgnore: []string{"rescue_image_id"}},
		},
	})
}

func testAccInstanceRescueConfig(imageName string, rescued bool) string {
	cfg := fmt.Sprintf(`
data "pcd_images_image" "boot" {
  name = %q
}

resource "pcd_networking_network" "test" {
  name = "tf-acc-rescue-net"
}

resource "pcd_networking_subnet" "test" {
  network_id = pcd_networking_network.test.id
  cidr       = "10.134.0.0/24"
}

resource "pcd_compute_instance" "test" {
  name        = "tf-acc-rescue"
  image_id    = data.pcd_images_image.boot.id
  flavor_name = "m1.small"

  network {
    uuid = pcd_networking_network.test.id
  }

  depends_on = [pcd_networking_subnet.test]
}
`, imageName)
	if rescued {
		cfg += `
resource "pcd_compute_instance_rescue" "test" {
  instance_id = pcd_compute_instance.test.id
}
`
	}
	return cfg
}

func testAccInstanceRescueVolumeConfig(imageName, rescueImage string) string {
	return fmt.Sprintf(`
data "pcd_images_image" "boot" {
  name = %q
}

resource "pcd_networking_network" "test" {
  name = "tf-acc-rescue-bfv-net"
}

resource "pcd_networking_subnet" "test" {
  network_id = pcd_networking_network.test.id
  cidr       = "10.135.0.0/24"
}

resource "pcd_compute_instance" "test" {
  name        = "tf-acc-rescue-bfv"
  flavor_name = "m1.small"

  block_device {
    source_type           = "image"
    uuid                  = data.pcd_images_image.boot.id
    destination_type      = "volume"
    volume_size           = 10
    boot_index            = 0
    delete_on_termination = true
  }

  network {
    uuid = pcd_networking_network.test.id
  }

  depends_on = [pcd_networking_subnet.test]
}

resource "pcd_compute_instance_rescue" "test" {
  instance_id     = pcd_compute_instance.test.id
  rescue_image_id = %q
}
`, imageName, rescueImage)
}
