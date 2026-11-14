// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package mors_test

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/servers"
	"github.com/gophercloud/gophercloud/v2/openstack/identity/v3/tokens"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"

	"github.com/platform9/terraform-provider-pcd/internal/acctest"
)

// testAccProviderProjectID returns the project the provider's token is scoped
// to, where pcd_compute_instance boots its VMs.
func testAccProviderProjectID(t *testing.T) string {
	t.Helper()
	client, err := acctest.LabConfig(t).IdentityV3Client()
	if err != nil {
		t.Fatalf("identity client: %v", err)
	}
	project, err := tokens.Get(context.Background(), client, client.Token()).ExtractProject()
	if err != nil || project == nil {
		t.Fatalf("reading the token's project: %v", err)
	}
	return project.ID
}

// testAccSharedPolicyMargin is how much younger than the test's one-year
// policy every server in the provider's project must be: 365 days minus a
// 30-day margin.
const testAccSharedPolicyMargin = 335 * 24 * time.Hour

// testAccSkipUnlessServersAreYoung skips the test unless every server in the
// provider's project was created less than testAccSharedPolicyMargin ago, so
// the test's one-year power-off policy acts on none of them. Nova lists the
// servers of the token's project.
func testAccSkipUnlessServersAreYoung(t *testing.T) {
	t.Helper()
	compute, err := acctest.LabConfig(t).ComputeV2Client()
	if err != nil {
		t.Fatalf("compute client: %v", err)
	}
	pages, err := servers.List(compute, servers.ListOpts{}).AllPages(context.Background())
	if err != nil {
		t.Fatalf("listing the provider project's servers: %v", err)
	}
	all, err := servers.ExtractServers(pages)
	if err != nil {
		t.Fatalf("reading the provider project's servers: %v", err)
	}
	cutoff := time.Now().Add(-testAccSharedPolicyMargin)
	for _, s := range all {
		if s.Created.Before(cutoff) {
			t.Skipf("server %s (%s) in the provider's project was created %s, 335 days ago or more; skipping: "+
				"this test's one-year power-off policy could power it off", s.ID, s.Name, s.Created.UTC().Format(time.RFC3339))
		}
	}
}

// TestAccComputeInstanceLease_basic needs a VM, and pcd_compute_instance boots
// into the provider's own project, so the test puts a lease policy on that
// shared project: one year, power off, so no VM younger than a year is
// touched. Any VM in the project older than a year would be powered off, so
// the test skips unless every server in the project is younger than 335 days.
// It runs only with PCD_ACC_MORS_SHARED_PROJECT=1 as well as PCD_ACC_MORS, and
// skips when the project already has a policy, which it would otherwise
// replace and then delete. The VM boots on m1.small, so PCD_ACC_IMAGE_NAME
// must name an image whose virtual size fits a 20 GB disk.
func TestAccComputeInstanceLease_basic(t *testing.T) {
	imageName := os.Getenv("PCD_ACC_IMAGE_NAME")
	const rn = "pcd_compute_instance_lease.test"
	first := time.Now().UTC().Add(24 * time.Hour).Truncate(time.Second).Format("2006-01-02T15:04:05Z")
	second := time.Now().UTC().Add(48 * time.Hour).Truncate(time.Second).Format("2006-01-02T15:04:05Z")

	resource.Test(t, resource.TestCase{
		PreCheck: func() {
			acctest.PreCheck(t)
			testAccMorsGate(t)
			if os.Getenv("PCD_ACC_MORS_SHARED_PROJECT") == "" {
				t.Skip("PCD_ACC_MORS_SHARED_PROJECT not set; skipping: this test writes a lease policy on the provider's own project")
			}
			if imageName == "" {
				t.Skip("PCD_ACC_IMAGE_NAME not set; skipping VM-boot test (needs a bootable image already in the image library)")
			}
			var existing testAccPolicy
			err := testAccMorsGet(t, &existing, "tenant", testAccProviderProjectID(t))
			if err == nil {
				t.Skip("the provider's project already has a lease policy; skipping rather than replacing it")
			}
			if !gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
				t.Fatalf("checking for an existing lease policy: %v", err)
			}
			testAccSkipUnlessServersAreYoung(t)
		},
		ProtoV6ProviderFactories: acctest.ProtoV6ProviderFactories,
		CheckDestroy:             resource.ComposeAggregateTestCheckFunc(testAccCheckInstanceLeaseDestroy(t), testAccCheckLeasePolicyDestroy(t)),
		Steps: []resource.TestStep{
			{
				Config: testAccInstanceLeaseConfig(imageName, first),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrPair(rn, "instance_id", "pcd_compute_instance.test", "id"),
					resource.TestCheckResourceAttrPair(rn, "project_id", "data.pcd_identity_auth_scope.me", "project_id"),
					resource.TestCheckResourceAttr(rn, "expiry", first),
					resource.TestCheckResourceAttr(rn, "action", "power off"),
					testAccCheckInstanceLeaseInMors(t, rn, first),
				),
			},
			{
				Config: testAccInstanceLeaseConfig(imageName, second),
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
					plancheck.ExpectResourceAction(rn, plancheck.ResourceActionUpdate),
				}},
				Check: testAccCheckInstanceLeaseInMors(t, rn, second),
			},
			{ResourceName: rn, ImportState: true, ImportStateVerify: true},
		},
	})
}

func testAccInstanceLeaseConfig(imageName, expiry string) string {
	return fmt.Sprintf(`
data "pcd_identity_auth_scope" "me" {
  name = "tf-acc-lease-scope"
}

data "pcd_images_image" "boot" {
  name = %q
}

resource "pcd_networking_network" "test" {
  name = "tf-acc-lease-net"
}

resource "pcd_networking_subnet" "test" {
  name       = "tf-acc-lease-subnet"
  network_id = pcd_networking_network.test.id
  cidr       = "10.143.0.0/24"
}

resource "pcd_compute_instance" "test" {
  name        = "tf-acc-lease-vm"
  image_id    = data.pcd_images_image.boot.id
  flavor_name = "m1.small"

  network {
    uuid = pcd_networking_network.test.id
  }

  depends_on = [pcd_networking_subnet.test]
}

resource "pcd_compute_lease_policy" "shared" {
  project_id     = data.pcd_identity_auth_scope.me.project_id
  expiry_minutes = 525600
  action         = "power off"
}

resource "pcd_compute_instance_lease" "test" {
  instance_id = pcd_compute_instance.test.id
  project_id  = pcd_compute_lease_policy.shared.project_id
  expiry      = %q
}
`, imageName, expiry)
}

type testAccLease struct {
	InstanceUUID string `json:"instance_uuid"`
	TenantUUID   string `json:"tenant_uuid"`
	Expiry       string `json:"expiry"`
	Action       string `json:"action"`
}

func testAccCheckInstanceLeaseInMors(t *testing.T, n, expiry string) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		rs, ok := s.RootModule().Resources[n]
		if !ok {
			return fmt.Errorf("not found in state: %s", n)
		}
		var got testAccLease
		project, instance := rs.Primary.Attributes["project_id"], rs.Primary.Attributes["instance_id"]
		if err := testAccMorsGet(t, &got, "tenant", project, "instance", instance); err != nil {
			return fmt.Errorf("reading the lease from mors: %w", err)
		}
		if got.InstanceUUID != instance || got.TenantUUID != project || got.Expiry != expiry || got.Action != "power off" {
			return fmt.Errorf("mors holds %+v, want instance %s in project %s until %s, power off", got, instance, project, expiry)
		}
		return nil
	}
}

func testAccCheckInstanceLeaseDestroy(t *testing.T) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		for _, rs := range s.RootModule().Resources {
			if rs.Type != "pcd_compute_instance_lease" {
				continue
			}
			var got testAccLease
			err := testAccMorsGet(t, &got, "tenant", rs.Primary.Attributes["project_id"], "instance", rs.Primary.Attributes["instance_id"])
			if err == nil {
				return fmt.Errorf("lease for instance %s still exists", rs.Primary.Attributes["instance_id"])
			}
			if !gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
				return fmt.Errorf("checking the lease for instance %s: %w", rs.Primary.Attributes["instance_id"], err)
			}
		}
		return nil
	}
}
