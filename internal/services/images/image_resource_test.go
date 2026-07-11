// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package images_test

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/image/v2/images"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"

	"github.com/platform9/terraform-provider-pcd/internal/acctest"
)

func TestAccImagesImageResource_basic(t *testing.T) {
	const resourceName = "pcd_images_image.test"
	imgFile := writeTempImage(t)

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { acctest.PreCheck(t) },
		ProtoV6ProviderFactories: acctest.ProtoV6ProviderFactories,
		CheckDestroy:             testAccCheckImageDestroy(t),
		Steps: []resource.TestStep{
			{
				Config: testAccImageConfig(imgFile, "tf-acc-image", 0, false),
				Check: resource.ComposeAggregateTestCheckFunc(
					testAccCheckImageExists(t, resourceName),
					resource.TestCheckResourceAttr(resourceName, "name", "tf-acc-image"),
					resource.TestCheckResourceAttr(resourceName, "status", "active"),
					resource.TestCheckResourceAttr(resourceName, "disk_format", "raw"),
					resource.TestCheckResourceAttrSet(resourceName, "checksum"),
					resource.TestCheckResourceAttrSet(resourceName, "size_bytes"),
					resource.TestCheckResourceAttrPair("data.pcd_images_image.by_name", "id", resourceName, "id"),
					resource.TestCheckResourceAttr("data.pcd_images_image_ids.by_name", "ids.#", "1"),
				),
			},
			{
				Config: testAccImageConfig(imgFile, "tf-acc-image-updated", 1, true),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(resourceName, "name", "tf-acc-image-updated"),
					resource.TestCheckResourceAttr(resourceName, "min_disk_gb", "1"),
					resource.TestCheckResourceAttr(resourceName, "protected", "true"),
				),
			},
			{
				ResourceName:            resourceName,
				ImportState:             true,
				ImportStateVerify:       true,
				ImportStateVerifyIgnore: []string{"local_file_path", "image_source_url", "verify_checksum"},
			},
		},
	})
}

func writeTempImage(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "tf-acc.raw")
	data := make([]byte, 1<<20) // 1 MiB
	for i := range data {
		data[i] = byte(i % 251)
	}
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatalf("writing temp image: %v", err)
	}
	return p
}

func testAccImageConfig(path, name string, minDiskGB int, protected bool) string {
	return fmt.Sprintf(`
resource "pcd_images_image" "test" {
  name             = %q
  container_format = "bare"
  disk_format      = "raw"
  local_file_path  = %q
  min_disk_gb      = %d
  protected        = %t
}

data "pcd_images_image" "by_name" {
  name        = pcd_images_image.test.name
  most_recent = true
}

data "pcd_images_image_ids" "by_name" {
  name = pcd_images_image.test.name
}
`, name, path, minDiskGB, protected)
}

func testAccCheckImageExists(t *testing.T, n string) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		rs, ok := s.RootModule().Resources[n]
		if !ok {
			return fmt.Errorf("not found in state: %s", n)
		}
		client, err := acctest.LabConfig(t).ImageV2Client()
		if err != nil {
			return err
		}
		if _, err := images.Get(context.Background(), client, rs.Primary.ID).Extract(); err != nil {
			return fmt.Errorf("image %s not found via API: %w", rs.Primary.ID, err)
		}
		return nil
	}
}

func testAccCheckImageDestroy(t *testing.T) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		client, err := acctest.LabConfig(t).ImageV2Client()
		if err != nil {
			return err
		}
		for _, rs := range s.RootModule().Resources {
			if rs.Type != "pcd_images_image" {
				continue
			}
			_, err := images.Get(context.Background(), client, rs.Primary.ID).Extract()
			if err == nil {
				return fmt.Errorf("image %s still exists", rs.Primary.ID)
			}
			if !gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
				return fmt.Errorf("unexpected error checking image %s: %w", rs.Primary.ID, err)
			}
		}
		return nil
	}
}
