// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package compute_test

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"testing"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/blockstorage/v3/snapshots"
	"github.com/gophercloud/gophercloud/v2/openstack/image/v2/images"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"

	"github.com/platform9/terraform-provider-pcd/internal/acctest"
)

// TestAccComputeInstanceSnapshot_basic snapshots an instance booted from an
// image, renames the snapshot in place, and imports it by image ID.
func TestAccComputeInstanceSnapshot_basic(t *testing.T) {
	imageName := testAccBootImageName(t)
	const rn = "pcd_compute_instance_snapshot.test"
	var imageID string

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { acctest.PreCheck(t) },
		ProtoV6ProviderFactories: acctest.ProtoV6ProviderFactories,
		CheckDestroy:             testAccCheckInstanceSnapshotDestroy(t),
		Steps: []resource.TestStep{
			{
				Config: testAccInstanceSnapshotConfig(imageName, "tf-acc-snap", ""),
				Check: resource.ComposeAggregateTestCheckFunc(
					testAccCaptureID(rn, &imageID),
					resource.TestCheckResourceAttrPair(rn, "instance_id", "pcd_compute_instance.test", "id"),
					resource.TestCheckResourceAttr(rn, "status", "active"),
					resource.TestCheckResourceAttr(rn, "volume_backed", "false"),
					resource.TestCheckResourceAttr(rn, "volume_snapshot_ids.#", "0"),
					resource.TestCheckResourceAttrSet(rn, "created_at"),
					testAccCheckSnapshotOfInstance(t, rn),
				),
			},
			{
				Config: testAccInstanceSnapshotConfig(imageName, "tf-acc-snap-renamed", ""),
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
					plancheck.ExpectResourceAction(rn, plancheck.ResourceActionUpdate),
				}},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(rn, "name", "tf-acc-snap-renamed"),
					resource.TestCheckResourceAttrWith(rn, "id", func(v string) error {
						if v != imageID {
							return fmt.Errorf("snapshot was replaced (%s -> %s); a rename is in place", imageID, v)
						}
						return nil
					}),
				),
			},
			{ResourceName: rn, ImportState: true, ImportStateVerify: true},
		},
	})
}

// TestAccComputeInstanceSnapshot_rebuildTarget boots a second instance from
// the first one's image and rebuilds it onto the snapshot: the snapshot is
// bootable, and an image-backed rebuild works with only one image in the
// library. The first instance is stopped before the second one boots, so the
// test never runs two instances at once.
func TestAccComputeInstanceSnapshot_rebuildTarget(t *testing.T) {
	imageName := testAccBootImageName(t)
	const target = "pcd_compute_instance.target"
	var targetID string

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { acctest.PreCheck(t) },
		ProtoV6ProviderFactories: acctest.ProtoV6ProviderFactories,
		CheckDestroy:             testAccCheckInstanceSnapshotDestroy(t),
		Steps: []resource.TestStep{
			{
				Config: testAccInstanceSnapshotRebuildConfig(imageName, "data.pcd_images_image.boot.id"),
				Check:  testAccCaptureID(target, &targetID),
			},
			{
				Config: testAccInstanceSnapshotRebuildConfig(imageName, "pcd_compute_instance_snapshot.test.id"),
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
					plancheck.ExpectResourceAction(target, plancheck.ResourceActionUpdate),
				}},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrWith(target, "id", func(v string) error {
						if v != targetID {
							return fmt.Errorf("instance was replaced (%s -> %s)", targetID, v)
						}
						return nil
					}),
					resource.TestCheckResourceAttrPair(target, "image_id", "pcd_compute_instance_snapshot.test", "id"),
					resource.TestCheckResourceAttr(target, "status", "ACTIVE"),
				),
			},
		},
	})
}

// TestAccComputeInstanceSnapshot_volumeBacked snapshots an instance that
// boots from a volume: the image refers to Cinder snapshots, which destroy
// deletes. It needs a Cinder backend that takes snapshots
// (PCD_ACC_VOLUME_BOOT=1).
func TestAccComputeInstanceSnapshot_volumeBacked(t *testing.T) {
	imageName := testAccBootImageName(t)
	testAccVolumeBoot(t)
	const rn = "pcd_compute_instance_snapshot.test"

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { acctest.PreCheck(t) },
		ProtoV6ProviderFactories: acctest.ProtoV6ProviderFactories,
		CheckDestroy:             testAccCheckInstanceSnapshotDestroy(t),
		Steps: []resource.TestStep{
			{
				Config: testAccInstanceSnapshotVolumeConfig(imageName),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(rn, "volume_backed", "true"),
					resource.TestCheckResourceAttr(rn, "size_bytes", "0"),
					resource.TestCheckResourceAttr(rn, "volume_snapshot_ids.#", "1"),
					testAccCheckSnapshotVolumesAvailable(t, rn),
				),
			},
			{ResourceName: rn, ImportState: true, ImportStateVerify: true},
		},
	})
}

// testAccVolumeBoot skips a test that boots from a Cinder volume unless
// PCD_ACC_VOLUME_BOOT says a Cinder backend for it is available.
func testAccVolumeBoot(t *testing.T) {
	t.Helper()
	if os.Getenv("PCD_ACC_VOLUME_BOOT") == "" {
		t.Skip("PCD_ACC_VOLUME_BOOT not set; skipping a test that boots from a Cinder volume (needs a Cinder backend that takes snapshots)")
	}
}

// testAccInstanceSnapshotConfig boots the source instance, with extra (an
// argument line, or "") added to its block, and snapshots it.
func testAccInstanceSnapshotConfig(imageName, snapName, extra string) string {
	return fmt.Sprintf(`
data "pcd_images_image" "boot" {
  name = %q
}

resource "pcd_networking_network" "test" {
  name = "tf-acc-snap-net"
}

resource "pcd_networking_subnet" "test" {
  network_id = pcd_networking_network.test.id
  cidr       = "10.132.0.0/24"
}

resource "pcd_compute_instance" "test" {
  name        = "tf-acc-snap-source"
  image_id    = data.pcd_images_image.boot.id
  flavor_name = "m1.small"
  %s

  network {
    uuid = pcd_networking_network.test.id
  }

  depends_on = [pcd_networking_subnet.test]
}

resource "pcd_compute_instance_snapshot" "test" {
  instance_id = pcd_compute_instance.test.id
  name        = %q
}
`, imageName, extra, snapName)
}

// testAccInstanceSnapshotRebuildConfig stops the source instance once it is
// created and boots the target only after the snapshot exists, so only one
// instance runs at a time.
func testAccInstanceSnapshotRebuildConfig(imageName, targetImage string) string {
	return testAccInstanceSnapshotConfig(imageName, "tf-acc-snap", `power_state = "shutoff"`) + fmt.Sprintf(`
resource "pcd_compute_instance" "target" {
  name        = "tf-acc-snap-target"
  image_id    = %s
  flavor_name = "m1.small"

  network {
    uuid = pcd_networking_network.test.id
  }

  depends_on = [pcd_networking_subnet.test, pcd_compute_instance_snapshot.test]
}
`, targetImage)
}

func testAccInstanceSnapshotVolumeConfig(imageName string) string {
	return fmt.Sprintf(`
data "pcd_images_image" "boot" {
  name = %q
}

resource "pcd_networking_network" "test" {
  name = "tf-acc-snap-bfv-net"
}

resource "pcd_networking_subnet" "test" {
  network_id = pcd_networking_network.test.id
  cidr       = "10.133.0.0/24"
}

resource "pcd_compute_instance" "test" {
  name        = "tf-acc-snap-bfv"
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

resource "pcd_compute_instance_snapshot" "test" {
  instance_id = pcd_compute_instance.test.id
  name        = "tf-acc-snap-bfv"
}
`, imageName)
}

// testAccCheckSnapshotOfInstance checks in Glance that the image records the
// instance it was taken from.
func testAccCheckSnapshotOfInstance(t *testing.T, rn string) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		rs := s.RootModule().Resources[rn]
		if rs == nil {
			return fmt.Errorf("not found in state: %s", rn)
		}
		client, err := acctest.LabConfig(t).ImageV2Client()
		if err != nil {
			return err
		}
		img, err := images.Get(context.Background(), client, rs.Primary.ID).Extract()
		if err != nil {
			return err
		}
		if got, _ := img.Properties["instance_uuid"].(string); got != rs.Primary.Attributes["instance_id"] {
			return fmt.Errorf("image %s has instance_uuid %q, want %q", img.ID, got, rs.Primary.Attributes["instance_id"])
		}
		return nil
	}
}

// testAccCheckSnapshotVolumesAvailable checks in Cinder that every volume
// snapshot the image refers to is available.
func testAccCheckSnapshotVolumesAvailable(t *testing.T, rn string) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		rs := s.RootModule().Resources[rn]
		if rs == nil {
			return fmt.Errorf("not found in state: %s", rn)
		}
		client, err := acctest.LabConfig(t).BlockStorageV3Client()
		if err != nil {
			return err
		}
		for i := 0; ; i++ {
			id, ok := rs.Primary.Attributes[fmt.Sprintf("volume_snapshot_ids.%d", i)]
			if !ok {
				return nil
			}
			snap, err := snapshots.Get(context.Background(), client, id).Extract()
			if err != nil {
				return err
			}
			if snap.Status != "available" {
				return fmt.Errorf("volume snapshot %s is %s, want available", id, snap.Status)
			}
		}
	}
}

// testAccCheckInstanceSnapshotDestroy checks that every snapshot image and
// every Cinder snapshot it referred to is gone.
func testAccCheckInstanceSnapshotDestroy(t *testing.T) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		cfg := acctest.LabConfig(t)
		imgClient, err := cfg.ImageV2Client()
		if err != nil {
			return err
		}
		for _, rs := range s.RootModule().Resources {
			if rs.Type != "pcd_compute_instance_snapshot" {
				continue
			}
			_, err := images.Get(context.Background(), imgClient, rs.Primary.ID).Extract()
			if err == nil {
				return fmt.Errorf("snapshot image %s still exists", rs.Primary.ID)
			}
			if !gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
				return err
			}
			for i := 0; ; i++ {
				id, ok := rs.Primary.Attributes[fmt.Sprintf("volume_snapshot_ids.%d", i)]
				if !ok {
					break
				}
				bs, err := cfg.BlockStorageV3Client()
				if err != nil {
					return err
				}
				if _, err := snapshots.Get(context.Background(), bs, id).Extract(); err == nil {
					return fmt.Errorf("volume snapshot %s of image %s still exists", id, rs.Primary.ID)
				} else if !gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
					return err
				}
			}
		}
		return testAccCheckInstanceDestroy(t)(s)
	}
}
