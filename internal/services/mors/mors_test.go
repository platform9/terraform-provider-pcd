// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package mors_test

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"testing"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"

	"github.com/platform9/terraform-provider-pcd/internal/acctest"
)

// testAccMorsGate skips unless PCD_ACC_MORS is set: these tests write lease
// policies, which mors acts on within a minute.
func testAccMorsGate(t *testing.T) {
	t.Helper()
	if os.Getenv("PCD_ACC_MORS") == "" {
		t.Skip("PCD_ACC_MORS not set; skipping mors tests (they write VM lease policies)")
	}
}

// testAccMorsGet reads a mors object out of band.
func testAccMorsGet(t *testing.T, out any, parts ...string) error {
	t.Helper()
	client, err := acctest.LabConfig(t).MorsV1Client()
	if err != nil {
		return err
	}
	_, err = client.Get(context.Background(), client.ServiceURL(parts...), out, &gophercloud.RequestOpts{OkCodes: []int{200}})
	return err
}

type testAccPolicy struct {
	Policy struct {
		TenantUUID string `json:"tenant_uuid"`
		ExpiryMins int64  `json:"expiry_mins"`
		Action     string `json:"action"`
	} `json:"vm_lease_policy"`
}

// TestAccComputeLeasePolicy_basic puts a policy on a project of its own, which
// has no VMs, so the policy can act on nothing.
func TestAccComputeLeasePolicy_basic(t *testing.T) {
	const rn = "pcd_compute_lease_policy.test"
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { acctest.PreCheck(t); testAccMorsGate(t) },
		ProtoV6ProviderFactories: acctest.ProtoV6ProviderFactories,
		CheckDestroy:             testAccCheckLeasePolicyDestroy(t),
		Steps: []resource.TestStep{
			{
				Config: testAccLeasePolicyConfig(1440, "power off"),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrPair(rn, "project_id", "pcd_identity_project.test", "id"),
					resource.TestCheckResourceAttrPair(rn, "id", "pcd_identity_project.test", "id"),
					resource.TestCheckResourceAttr(rn, "expiry_minutes", "1440"),
					resource.TestCheckResourceAttr(rn, "action", "power off"),
					testAccCheckLeasePolicyInMors(t, rn, 1440, "power off"),
				),
			},
			{
				Config: testAccLeasePolicyConfig(2880, "delete"),
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
					plancheck.ExpectResourceAction(rn, plancheck.ResourceActionUpdate),
				}},
				Check: testAccCheckLeasePolicyInMors(t, rn, 2880, "delete"),
			},
			{ResourceName: rn, ImportState: true, ImportStateVerify: true},
		},
	})
}

func testAccLeasePolicyConfig(minutes int, action string) string {
	return fmt.Sprintf(`
resource "pcd_identity_project" "test" {
  name = "tf-acc-lease-policy"
}

resource "pcd_compute_lease_policy" "test" {
  project_id     = pcd_identity_project.test.id
  expiry_minutes = %d
  action         = %q
}
`, minutes, action)
}

func testAccCheckLeasePolicyInMors(t *testing.T, n string, minutes int64, action string) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		rs, ok := s.RootModule().Resources[n]
		if !ok {
			return fmt.Errorf("not found in state: %s", n)
		}
		var got testAccPolicy
		if err := testAccMorsGet(t, &got, "tenant", rs.Primary.Attributes["project_id"]); err != nil {
			return fmt.Errorf("reading the policy from mors: %w", err)
		}
		if got.Policy.TenantUUID != rs.Primary.Attributes["project_id"] || got.Policy.ExpiryMins != minutes || got.Policy.Action != action {
			return fmt.Errorf("mors holds %+v, want project %s, %d minutes, %q", got.Policy, rs.Primary.Attributes["project_id"], minutes, action)
		}
		return nil
	}
}

func testAccCheckLeasePolicyDestroy(t *testing.T) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		for _, rs := range s.RootModule().Resources {
			if rs.Type != "pcd_compute_lease_policy" {
				continue
			}
			var got testAccPolicy
			err := testAccMorsGet(t, &got, "tenant", rs.Primary.Attributes["project_id"])
			if err == nil {
				return fmt.Errorf("lease policy for project %s still exists", rs.Primary.Attributes["project_id"])
			}
			if !gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
				return fmt.Errorf("checking the lease policy for project %s: %w", rs.Primary.Attributes["project_id"], err)
			}
		}
		return nil
	}
}
