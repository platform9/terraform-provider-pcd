// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package blockstorage_test

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/blockstorage/v3/backups"
	"github.com/gophercloud/gophercloud/v2/openstack/blockstorage/v3/snapshots"
	"github.com/gophercloud/gophercloud/v2/openstack/blockstorage/v3/volumetypes"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"

	"github.com/platform9/terraform-provider-pcd/internal/acctest"
)

// TestAccBlockStorageVolumeType_basic creates a volume type with extra_specs,
// updates its name/description/specs, and imports it. Volume types are metadata,
// so this needs no working storage backend.
func TestAccBlockStorageVolumeType_basic(t *testing.T) {
	const rn = "pcd_blockstorage_volume_type.test"
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { acctest.PreCheck(t) },
		ProtoV6ProviderFactories: acctest.ProtoV6ProviderFactories,
		CheckDestroy:             testAccCheckVolumeTypeDestroy(t),
		Steps: []resource.TestStep{
			{
				Config: testAccVolumeTypeConfig("tf-acc-vt", "first", "iscsi-a"),
				Check: resource.ComposeAggregateTestCheckFunc(
					testAccCheckVolumeTypeExists(t, rn),
					resource.TestCheckResourceAttr(rn, "name", "tf-acc-vt"),
					resource.TestCheckResourceAttr(rn, "description", "first"),
					resource.TestCheckResourceAttr(rn, "is_public", "true"),
					resource.TestCheckResourceAttr(rn, "extra_specs.volume_backend_name", "iscsi-a"),
				),
			},
			{
				Config: testAccVolumeTypeConfig("tf-acc-vt", "second", "iscsi-b"),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(rn, "description", "second"),
					resource.TestCheckResourceAttr(rn, "extra_specs.volume_backend_name", "iscsi-b"),
				),
			},
			{ResourceName: rn, ImportState: true, ImportStateVerify: true},
		},
	})
}

func testAccVolumeTypeConfig(name, desc, backend string) string {
	return fmt.Sprintf(`
resource "pcd_blockstorage_volume_type" "test" {
  name        = %q
  description = %q
  is_public   = true
  extra_specs = {
    volume_backend_name = %q
  }
}
`, name, desc, backend)
}

// TestAccBlockStorageSnapshot_basic creates a volume and snapshots it. Requires a
// working Cinder storage backend.
func TestAccBlockStorageSnapshot_basic(t *testing.T) {
	const rn = "pcd_blockstorage_snapshot.test"
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { acctest.PreCheck(t) },
		ProtoV6ProviderFactories: acctest.ProtoV6ProviderFactories,
		CheckDestroy:             testAccCheckSnapshotDestroy(t),
		Steps: []resource.TestStep{
			{
				Config: testAccSnapshotConfig("tf-acc-snap"),
				Check: resource.ComposeAggregateTestCheckFunc(
					testAccCheckSnapshotExists(t, rn),
					resource.TestCheckResourceAttr(rn, "name", "tf-acc-snap"),
					resource.TestCheckResourceAttr(rn, "status", "available"),
					resource.TestCheckResourceAttrPair(rn, "volume_id", "pcd_blockstorage_volume.test", "id"),
				),
			},
			{ResourceName: rn, ImportState: true, ImportStateVerify: true, ImportStateVerifyIgnore: []string{"force"}},
		},
	})
}

func testAccSnapshotConfig(name string) string {
	return fmt.Sprintf(`
resource "pcd_blockstorage_volume" "test" {
  name = "tf-acc-snap-vol"
  size = 1
}

resource "pcd_blockstorage_snapshot" "test" {
  name      = %q
  volume_id = pcd_blockstorage_volume.test.id
}
`, name)
}

// TestAccBlockStorageVolumeBackup_basic creates a volume and backs it up.
// Requires a Cinder storage backend and the backup service.
func TestAccBlockStorageVolumeBackup_basic(t *testing.T) {
	const rn = "pcd_blockstorage_volume_backup.test"
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { acctest.PreCheck(t) },
		ProtoV6ProviderFactories: acctest.ProtoV6ProviderFactories,
		CheckDestroy:             testAccCheckBackupDestroy(t),
		Steps: []resource.TestStep{
			{
				Config: testAccBackupConfig("tf-acc-backup"),
				Check: resource.ComposeAggregateTestCheckFunc(
					testAccCheckBackupExists(t, rn),
					resource.TestCheckResourceAttr(rn, "name", "tf-acc-backup"),
					resource.TestCheckResourceAttr(rn, "status", "available"),
				),
			},
			{ResourceName: rn, ImportState: true, ImportStateVerify: true, ImportStateVerifyIgnore: []string{"force", "incremental"}},
		},
	})
}

func testAccBackupConfig(name string) string {
	return fmt.Sprintf(`
resource "pcd_blockstorage_volume" "test" {
  name = "tf-acc-backup-vol"
  size = 1
}

resource "pcd_blockstorage_volume_backup" "test" {
  name      = %q
  volume_id = pcd_blockstorage_volume.test.id
}
`, name)
}

func testAccCheckVolumeTypeExists(t *testing.T, n string) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		rs, ok := s.RootModule().Resources[n]
		if !ok {
			return fmt.Errorf("not found in state: %s", n)
		}
		client, err := acctest.LabConfig(t).BlockStorageV3Client()
		if err != nil {
			return err
		}
		if _, err := volumetypes.Get(context.Background(), client, rs.Primary.ID).Extract(); err != nil {
			return fmt.Errorf("volume type %s not found via API: %w", rs.Primary.ID, err)
		}
		return nil
	}
}

func testAccCheckVolumeTypeDestroy(t *testing.T) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		client, err := acctest.LabConfig(t).BlockStorageV3Client()
		if err != nil {
			return err
		}
		for _, rs := range s.RootModule().Resources {
			if rs.Type != "pcd_blockstorage_volume_type" {
				continue
			}
			_, err := volumetypes.Get(context.Background(), client, rs.Primary.ID).Extract()
			if err == nil {
				return fmt.Errorf("volume type %s still exists", rs.Primary.ID)
			}
			if !gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
				return fmt.Errorf("unexpected error checking volume type %s: %w", rs.Primary.ID, err)
			}
		}
		return nil
	}
}

func testAccCheckSnapshotExists(t *testing.T, n string) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		rs, ok := s.RootModule().Resources[n]
		if !ok {
			return fmt.Errorf("not found in state: %s", n)
		}
		client, err := acctest.LabConfig(t).BlockStorageV3Client()
		if err != nil {
			return err
		}
		if _, err := snapshots.Get(context.Background(), client, rs.Primary.ID).Extract(); err != nil {
			return fmt.Errorf("snapshot %s not found via API: %w", rs.Primary.ID, err)
		}
		return nil
	}
}

func testAccCheckSnapshotDestroy(t *testing.T) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		client, err := acctest.LabConfig(t).BlockStorageV3Client()
		if err != nil {
			return err
		}
		for _, rs := range s.RootModule().Resources {
			if rs.Type != "pcd_blockstorage_snapshot" {
				continue
			}
			_, err := snapshots.Get(context.Background(), client, rs.Primary.ID).Extract()
			if err == nil {
				return fmt.Errorf("snapshot %s still exists", rs.Primary.ID)
			}
			if !gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
				return fmt.Errorf("unexpected error checking snapshot %s: %w", rs.Primary.ID, err)
			}
		}
		return nil
	}
}

func testAccCheckBackupExists(t *testing.T, n string) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		rs, ok := s.RootModule().Resources[n]
		if !ok {
			return fmt.Errorf("not found in state: %s", n)
		}
		client, err := acctest.LabConfig(t).BlockStorageV3Client()
		if err != nil {
			return err
		}
		if _, err := backups.Get(context.Background(), client, rs.Primary.ID).Extract(); err != nil {
			return fmt.Errorf("backup %s not found via API: %w", rs.Primary.ID, err)
		}
		return nil
	}
}

func testAccCheckBackupDestroy(t *testing.T) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		client, err := acctest.LabConfig(t).BlockStorageV3Client()
		if err != nil {
			return err
		}
		for _, rs := range s.RootModule().Resources {
			if rs.Type != "pcd_blockstorage_volume_backup" {
				continue
			}
			_, err := backups.Get(context.Background(), client, rs.Primary.ID).Extract()
			if err == nil {
				return fmt.Errorf("backup %s still exists", rs.Primary.ID)
			}
			if !gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
				return fmt.Errorf("unexpected error checking backup %s: %w", rs.Primary.ID, err)
			}
		}
		return nil
	}
}
