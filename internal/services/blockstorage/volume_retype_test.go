// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package blockstorage_test

import (
	"context"
	"fmt"
	"regexp"
	"testing"

	"github.com/gophercloud/gophercloud/v2/openstack/blockstorage/v3/volumes"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"

	"github.com/platform9/terraform-provider-pcd/internal/acctest"
	"github.com/platform9/terraform-provider-pcd/internal/clients"
)

// TestAccBlockStorageVolume_retype creates a volume on one type, retypes it
// in place to a second type, imports it, then shows that Cinder's refusal to
// retype a volume with a snapshot surfaces as an apply error. The two types
// carry the same (empty) extra_specs, so the retype is the in-place kind every
// backend supports: with volume_retype_policy = "never", types that differ in
// extra_specs, QoS or encryption need driver support or a migration.
func TestAccBlockStorageVolume_retype(t *testing.T) {
	const rn = "pcd_blockstorage_volume.test"
	var volumeID string

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { acctest.PreCheck(t) },
		ProtoV6ProviderFactories: acctest.ProtoV6ProviderFactories,
		CheckDestroy: resource.ComposeAggregateTestCheckFunc(
			testAccCheckVolumeDestroy(t),
			testAccCheckVolumeTypeDestroy(t),
		),
		Steps: []resource.TestStep{
			{
				Config: testAccVolumeRetypeConfig("a", false),
				Check: resource.ComposeAggregateTestCheckFunc(
					testAccCheckVolumeExists(t, rn),
					resource.TestCheckResourceAttr(rn, "volume_type", "tf-acc-retype-a"),
					resource.TestCheckResourceAttr(rn, "volume_retype_policy", "never"),
					testAccCaptureVolumeID(rn, &volumeID),
				),
			},
			{
				Config: testAccVolumeRetypeConfig("b", false),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(rn, plancheck.ResourceActionUpdate)},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(rn, "volume_type", "tf-acc-retype-b"),
					resource.TestCheckResourceAttrPtr(rn, "id", &volumeID),
					testAccCheckVolumeType(t, rn, "tf-acc-retype-b"),
				),
			},
			{ResourceName: rn, ImportState: true, ImportStateVerify: true},
			{
				Config: testAccVolumeRetypeConfig("b", true),
				Check:  testAccCheckVolumeType(t, rn, "tf-acc-retype-b"),
			},
			{
				Config:      testAccVolumeRetypeConfig("a", true),
				ExpectError: regexp.MustCompile(`blockstorage: retyping volume`),
			},
		},
	})
}

// testAccVolumeRetypeConfig puts a volume on type tf-acc-retype-<onType>,
// optionally with a snapshot.
func testAccVolumeRetypeConfig(onType string, withSnapshot bool) string {
	snapshot := ""
	if withSnapshot {
		snapshot = `
resource "pcd_blockstorage_snapshot" "test" {
  name      = "tf-acc-retype-snap"
  volume_id = pcd_blockstorage_volume.test.id
}
`
	}
	return fmt.Sprintf(`
resource "pcd_blockstorage_volume_type" "a" {
  name      = "tf-acc-retype-a"
  is_public = true
}

resource "pcd_blockstorage_volume_type" "b" {
  name      = "tf-acc-retype-b"
  is_public = true
}

resource "pcd_blockstorage_volume" "test" {
  name        = "tf-acc-retype-vol"
  size        = 1
  volume_type = pcd_blockstorage_volume_type.%s.name

  # Both types outlive the volume on destroy, whichever one it is on.
  depends_on = [pcd_blockstorage_volume_type.a, pcd_blockstorage_volume_type.b]
}
%s`, onType, snapshot)
}

func testAccCaptureVolumeID(n string, dst *string) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		rs := s.RootModule().Resources[n]
		if rs == nil {
			return fmt.Errorf("not found in state: %s", n)
		}
		*dst = rs.Primary.ID
		return nil
	}
}

// testAccCheckVolumeType reads the volume's type from Cinder.
func testAccCheckVolumeType(t *testing.T, n, want string) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		rs := s.RootModule().Resources[n]
		if rs == nil {
			return fmt.Errorf("not found in state: %s", n)
		}
		client, err := acctest.LabConfig(t).BlockStorageV3Client()
		if err != nil {
			return err
		}
		vol, err := clients.RequireObject(volumes.Get(context.Background(), client, rs.Primary.ID).Extract())
		if err == nil && vol.ID == "" {
			err = clients.ErrNoObject
		}
		if err != nil {
			return fmt.Errorf("reading volume %s: %w", rs.Primary.ID, err)
		}
		if vol.VolumeType != want {
			return fmt.Errorf("volume %s has type %q in Cinder, want %q", rs.Primary.ID, vol.VolumeType, want)
		}
		return nil
	}
}
