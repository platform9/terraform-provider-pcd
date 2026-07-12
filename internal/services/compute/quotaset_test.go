// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package compute_test

import (
	"context"
	"fmt"
	"strconv"
	"testing"

	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/quotasets"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"

	"github.com/platform9/terraform-provider-pcd/internal/acctest"
)

// TestAccComputeQuotaset sets Nova quotas on a freshly-created project, verifies
// them against the API, updates a field in place, and imports the resource.
func TestAccComputeQuotaset(t *testing.T) {
	const rn = "pcd_compute_quotaset.test"

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { acctest.PreCheck(t) },
		ProtoV6ProviderFactories: acctest.ProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: testAccComputeQuotasetConfig(32),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(rn, "cores", "32"),
					resource.TestCheckResourceAttr(rn, "instances", "15"),
					testAccCheckComputeQuotaValue(t, rn, 32),
				),
			},
			{
				Config: testAccComputeQuotasetConfig(48),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(rn, "cores", "48"),
					testAccCheckComputeQuotaValue(t, rn, 48),
				),
			},
			{ResourceName: rn, ImportState: true, ImportStateVerify: true},
		},
	})
}

func testAccComputeQuotasetConfig(cores int) string {
	return fmt.Sprintf(`
resource "pcd_identity_project" "test" {
  name = "tf-acc-quota-proj"
}

resource "pcd_compute_quotaset" "test" {
  project_id = pcd_identity_project.test.id
  cores      = %d
  instances  = 15
  ram        = 65536
}
`, cores)
}

func testAccCheckComputeQuotaValue(t *testing.T, n string, wantCores int) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		rs, ok := s.RootModule().Resources[n]
		if !ok {
			return fmt.Errorf("not found in state: %s", n)
		}
		projectID := rs.Primary.Attributes["project_id"]
		client, err := acctest.LabConfig(t).ComputeV2Client()
		if err != nil {
			return err
		}
		qs, err := quotasets.Get(context.Background(), client, projectID).Extract()
		if err != nil {
			return fmt.Errorf("reading quotas for %s: %w", projectID, err)
		}
		if qs.Cores != wantCores {
			return fmt.Errorf("expected cores=%d, got %d", wantCores, qs.Cores)
		}
		if got := rs.Primary.Attributes["cores"]; got != strconv.Itoa(wantCores) {
			return fmt.Errorf("state cores=%s, want %d", got, wantCores)
		}
		return nil
	}
}
