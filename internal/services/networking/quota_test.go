// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package networking_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/extensions/quotas"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"

	"github.com/platform9/terraform-provider-pcd/internal/acctest"
)

// TestAccNetworkingQuota sets Neutron quotas on a freshly-created project,
// verifies them against the API, updates a field in place, and imports it.
func TestAccNetworkingQuota(t *testing.T) {
	const rn = "pcd_networking_quota.test"

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { acctest.PreCheck(t) },
		ProtoV6ProviderFactories: acctest.ProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: testAccNetworkingQuotaConfig(20),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(rn, "network", "20"),
					resource.TestCheckResourceAttr(rn, "router", "10"),
					testAccCheckNetworkingQuotaValue(t, rn, 20),
				),
			},
			{
				Config: testAccNetworkingQuotaConfig(30),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(rn, "network", "30"),
					testAccCheckNetworkingQuotaValue(t, rn, 30),
				),
			},
			{ResourceName: rn, ImportState: true, ImportStateVerify: true},
		},
	})
}

func testAccNetworkingQuotaConfig(network int) string {
	return fmt.Sprintf(`
resource "pcd_identity_project" "test" {
  name = "tf-acc-netquota-proj"
}

resource "pcd_networking_quota" "test" {
  project_id     = pcd_identity_project.test.id
  network        = %d
  router         = 10
  port           = 200
  security_group = 20
}
`, network)
}

func testAccCheckNetworkingQuotaValue(t *testing.T, n string, wantNetwork int) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		rs, ok := s.RootModule().Resources[n]
		if !ok {
			return fmt.Errorf("not found in state: %s", n)
		}
		projectID := rs.Primary.Attributes["project_id"]
		client, err := acctest.LabConfig(t).NetworkV2Client()
		if err != nil {
			return err
		}
		q, err := quotas.Get(context.Background(), client, projectID).Extract()
		if err != nil {
			return fmt.Errorf("reading quotas for %s: %w", projectID, err)
		}
		if q.Network != wantNetwork {
			return fmt.Errorf("expected network=%d, got %d", wantNetwork, q.Network)
		}
		return nil
	}
}
