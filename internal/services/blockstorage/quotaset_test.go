// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package blockstorage_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/gophercloud/gophercloud/v2/openstack/blockstorage/v3/quotasets"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"

	"github.com/platform9/terraform-provider-pcd/internal/acctest"
)

// TestAccBlockStorageQuotaset sets Cinder quotas on a freshly-created project,
// verifies them against the API, updates a field in place, and imports it.
func TestAccBlockStorageQuotaset(t *testing.T) {
	const rn = "pcd_blockstorage_quotaset.test"

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { acctest.PreCheck(t) },
		ProtoV6ProviderFactories: acctest.ProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: testAccBlockStorageQuotasetConfig(50),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(rn, "volumes", "50"),
					resource.TestCheckResourceAttr(rn, "gigabytes", "1000"),
					testAccCheckBlockStorageQuotaValue(t, rn, 50),
				),
			},
			{
				Config: testAccBlockStorageQuotasetConfig(75),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(rn, "volumes", "75"),
					testAccCheckBlockStorageQuotaValue(t, rn, 75),
				),
			},
			{ResourceName: rn, ImportState: true, ImportStateVerify: true},
		},
	})
}

func testAccBlockStorageQuotasetConfig(volumes int) string {
	return fmt.Sprintf(`
resource "pcd_identity_project" "test" {
  name = "tf-acc-cinderquota-proj"
}

resource "pcd_blockstorage_quotaset" "test" {
  project_id = pcd_identity_project.test.id
  volumes    = %d
  snapshots  = 50
  gigabytes  = 1000
}
`, volumes)
}

func testAccCheckBlockStorageQuotaValue(t *testing.T, n string, wantVolumes int) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		rs, ok := s.RootModule().Resources[n]
		if !ok {
			return fmt.Errorf("not found in state: %s", n)
		}
		projectID := rs.Primary.Attributes["project_id"]
		client, err := acctest.LabConfig(t).BlockStorageV3Client()
		if err != nil {
			return err
		}
		qs, err := quotasets.Get(context.Background(), client, projectID).Extract()
		if err != nil {
			return fmt.Errorf("reading quotas for %s: %w", projectID, err)
		}
		if qs.Volumes != wantVolumes {
			return fmt.Errorf("expected volumes=%d, got %d", wantVolumes, qs.Volumes)
		}
		return nil
	}
}
