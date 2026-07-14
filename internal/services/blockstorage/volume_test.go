// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package blockstorage_test

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/blockstorage/v3/volumes"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"

	"github.com/platform9/terraform-provider-pcd/internal/acctest"
)

// TestAccBlockStorageVolume_basic exercises create (wait for available), extend,
// and import. It requires a working Cinder storage backend on the target cloud;
// on the CE lab used during development there is none configured, so volumes go
// straight to ERROR.
func TestAccBlockStorageVolume_basic(t *testing.T) {
	const rn = "pcd_blockstorage_volume.test"

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { acctest.PreCheck(t) },
		ProtoV6ProviderFactories: acctest.ProtoV6ProviderFactories,
		CheckDestroy:             testAccCheckVolumeDestroy(t),
		Steps: []resource.TestStep{
			{
				Config: testAccVolumeConfig("tf-acc-volume", 1),
				Check: resource.ComposeAggregateTestCheckFunc(
					testAccCheckVolumeExists(t, rn),
					resource.TestCheckResourceAttr(rn, "size", "1"),
					resource.TestCheckResourceAttr(rn, "status", "available"),
				),
			},
			{
				Config: testAccVolumeConfig("tf-acc-volume", 2),
				Check:  resource.TestCheckResourceAttr(rn, "size", "2"),
			},
			{ResourceName: rn, ImportState: true, ImportStateVerify: true},
		},
	})
}

func testAccVolumeConfig(name string, size int) string {
	return fmt.Sprintf(`
resource "pcd_blockstorage_volume" "test" {
  name = %q
  size = %d
}
`, name, size)
}

func testAccCheckVolumeExists(t *testing.T, n string) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		rs := s.RootModule().Resources[n]
		if rs == nil {
			return fmt.Errorf("not found in state: %s", n)
		}
		client, err := acctest.LabConfig(t).BlockStorageV3Client()
		if err != nil {
			return err
		}
		if _, err := volumes.Get(context.Background(), client, rs.Primary.ID).Extract(); err != nil {
			return fmt.Errorf("volume %s not found: %w", rs.Primary.ID, err)
		}
		return nil
	}
}

func testAccCheckVolumeDestroy(t *testing.T) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		client, err := acctest.LabConfig(t).BlockStorageV3Client()
		if err != nil {
			return err
		}
		for _, rs := range s.RootModule().Resources {
			if rs.Type != "pcd_blockstorage_volume" {
				continue
			}
			_, err := volumes.Get(context.Background(), client, rs.Primary.ID).Extract()
			if err == nil {
				return fmt.Errorf("volume %s still exists", rs.Primary.ID)
			}
			if !gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
				return err
			}
		}
		return nil
	}
}
