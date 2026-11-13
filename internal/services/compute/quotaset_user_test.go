// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package compute_test

import (
	"context"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"testing"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/quotasets"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"

	"github.com/platform9/terraform-provider-pcd/internal/acctest"
	"github.com/platform9/terraform-provider-pcd/internal/clients"
)

// TestAccComputeQuotaset_user sets one user's quotas in a new project, raises
// them in place in the same apply that raises a project quota the user quota
// set does not set, imports them with the three-part ID, checks that Nova
// refuses a user quota above the project's, and then removes the user quota
// set and checks that the user is back on the project's quotas.
func TestAccComputeQuotaset_user(t *testing.T) {
	const rn = "pcd_compute_quotaset.user"

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { acctest.PreCheck(t) },
		ProtoV6ProviderFactories: acctest.ProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: testAccComputeQuotasetUserConfig(10, 8, true),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(rn, "cores", "8"),
					resource.TestCheckResourceAttr(rn, "key_pairs", "10"),
					resource.TestCheckResourceAttrPair(rn, "user_id", "pcd_identity_user.test", "id"),
					testAccCheckComputeUserQuotaCores(t, rn, 8),
				),
			},
			{
				// One apply raises the project's key_pairs, which the user quota set
				// does not set and reads back from the project, and the user's cores.
				Config: testAccComputeQuotasetUserConfig(20, 12, true),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(rn, "cores", "12"),
					resource.TestCheckResourceAttr(rn, "key_pairs", "20"),
					testAccCheckComputeUserQuotaCores(t, rn, 12),
				),
			},
			{ResourceName: rn, ImportState: true, ImportStateVerify: true},
			{
				// The project allows 32 cores in all; Nova refuses a user above that.
				Config:      testAccComputeQuotasetUserConfig(20, 64, true),
				ExpectError: regexp.MustCompile(`compute: updating quotas`),
			},
			{
				// Destroying the user quota set removes the user's overrides.
				Config: testAccComputeQuotasetUserConfig(20, 12, false),
				Check:  testAccCheckComputeUserQuotaReverted(t, "pcd_compute_quotaset.project", "pcd_identity_user.test"),
			},
		},
	})
}

func testAccComputeQuotasetUserConfig(projectKeyPairs, cores int, withUserQuota bool) string {
	cfg := fmt.Sprintf(`
resource "pcd_identity_project" "test" {
  name = "tf-acc-user-quota-proj"
}

resource "pcd_identity_user" "test" {
  name     = "tf-acc-user-quota-user"
  password = "Tf-Acc-Passw0rd!"
}

resource "pcd_compute_quotaset" "project" {
  project_id = pcd_identity_project.test.id
  cores      = 32
  instances  = 15
  ram        = 65536
  key_pairs  = %d
}
`, projectKeyPairs)
	if withUserQuota {
		cfg += fmt.Sprintf(`
resource "pcd_compute_quotaset" "user" {
  project_id = pcd_identity_project.test.id
  user_id    = pcd_identity_user.test.id
  cores      = %d
  instances  = 4
  ram        = 16384

  depends_on = [pcd_compute_quotaset.project]
}
`, cores)
	}
	return cfg
}

// userQuotaCores reads the cores quota Nova applies to the user in the project.
func userQuotaCores(t *testing.T, projectID, userID string) (int, error) {
	client, err := acctest.LabConfig(t).ComputeV2Client()
	if err != nil {
		return 0, err
	}
	var r quotasets.GetResult
	resp, err := client.Get(context.Background(),
		client.ServiceURL("os-quota-sets", projectID)+"?user_id="+url.QueryEscape(userID), &r.Body, nil)
	_, r.Header, r.Err = gophercloud.ParseResponse(resp, err)
	qs, err := clients.RequireObject(r.Extract())
	if err != nil {
		return 0, fmt.Errorf("reading the quotas of user %s in project %s: %w", userID, projectID, err)
	}
	return qs.Cores, nil
}

func testAccCheckComputeUserQuotaCores(t *testing.T, n string, want int) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		rs, ok := s.RootModule().Resources[n]
		if !ok {
			return fmt.Errorf("not found in state: %s", n)
		}
		got, err := userQuotaCores(t, rs.Primary.Attributes["project_id"], rs.Primary.Attributes["user_id"])
		if err != nil {
			return err
		}
		if got != want {
			return fmt.Errorf("user cores = %d, want %d", got, want)
		}
		return nil
	}
}

// testAccCheckComputeUserQuotaReverted checks that the user's cores quota is
// the project's again, which Nova reports once the user has no override.
func testAccCheckComputeUserQuotaReverted(t *testing.T, projectQuota, user string) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		pq, ok := s.RootModule().Resources[projectQuota]
		if !ok {
			return fmt.Errorf("not found in state: %s", projectQuota)
		}
		us, ok := s.RootModule().Resources[user]
		if !ok {
			return fmt.Errorf("not found in state: %s", user)
		}
		p, u := pq.Primary.Attributes, us.Primary.ID
		got, err := userQuotaCores(t, p["project_id"], u)
		if err != nil {
			return err
		}
		if want := p["cores"]; strconv.Itoa(got) != want {
			return fmt.Errorf("user cores = %d after destroy, want the project's %s", got, want)
		}
		return nil
	}
}
