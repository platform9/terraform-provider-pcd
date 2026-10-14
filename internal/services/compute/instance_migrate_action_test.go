// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package compute_test

import (
	"fmt"
	"os"
	"regexp"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/tfversion"

	"github.com/platform9/terraform-provider-pcd/internal/acctest"
)

// The migrate tests boot their own instance, pinned to PCD_ACC_HOST_ID with
// PCD_ACC_CLUSTER_NAME, and are opt-in: PCD_ACC_ADMIN_COMPUTE=1.
// Without a second hypervisor only the refusals can be tested; with one
// (PCD_ACC_HOST2_ID, same cluster) TestAccComputeInstanceMigrate_moves runs.

// TestAccComputeInstanceMigrate_sameHost asks for a cold migration to the host
// the instance is on: Nova refuses it at once, and the error says so.
func TestAccComputeInstanceMigrate_sameHost(t *testing.T) {
	image, cluster, hostID := testAccMigrateEnv(t)
	resource.Test(t, resource.TestCase{
		TerraformVersionChecks:   []tfversion.TerraformVersionCheck{tfversion.SkipBelow(tfversion.Version1_14_0)},
		PreCheck:                 func() { acctest.PreCheck(t) },
		ProtoV6ProviderFactories: acctest.ProtoV6ProviderFactories,
		CheckDestroy:             testAccCheckInstanceDestroy(t),
		Steps: []resource.TestStep{{
			Config:      testAccInstanceMigrateConfig(image, cluster, hostID, "1", fmt.Sprintf(`mode = "cold"`+"\n    host = %q", hostID)),
			ExpectError: regexp.MustCompile(`Migration failed`),
		}},
	})
}

// TestAccComputeInstanceMigrate_noTarget live-migrates with no second
// hypervisor: Nova finds no host, the action fails, the instance stays.
func TestAccComputeInstanceMigrate_noTarget(t *testing.T) {
	image, cluster, hostID := testAccMigrateEnv(t)
	if os.Getenv("PCD_ACC_HOST2_ID") != "" {
		t.Skip("PCD_ACC_HOST2_ID is set, so the instance has somewhere to go")
	}
	cfg := testAccInstanceMigrateConfig(image, cluster, hostID, "1", `mode = "live"`)
	resource.Test(t, resource.TestCase{
		TerraformVersionChecks:   []tfversion.TerraformVersionCheck{tfversion.SkipBelow(tfversion.Version1_14_0)},
		PreCheck:                 func() { acctest.PreCheck(t) },
		ProtoV6ProviderFactories: acctest.ProtoV6ProviderFactories,
		CheckDestroy:             testAccCheckInstanceDestroy(t),
		Steps: []resource.TestStep{
			{Config: cfg, ExpectError: regexp.MustCompile(`Migration failed`)},
			{Config: cfg, Check: testAccCheckInstanceOnHost(t, "pcd_compute_instance.pinned", hostID)},
		},
	})
}

// TestAccComputeInstanceMigrate_moves live-migrates to the second hypervisor,
// then (after bumping the trigger) cold-migrates back with the scheduler's
// choice, checking where Nova runs it each time and that the instance was
// never replaced.
func TestAccComputeInstanceMigrate_moves(t *testing.T) {
	image, cluster, hostID := testAccMigrateEnv(t)
	host2 := os.Getenv("PCD_ACC_HOST2_ID")
	if host2 == "" {
		t.Skip("PCD_ACC_HOST2_ID not set; a successful migration needs a second hypervisor in the cluster")
	}
	const rn = "pcd_compute_instance.pinned"
	var id string
	resource.Test(t, resource.TestCase{
		TerraformVersionChecks:   []tfversion.TerraformVersionCheck{tfversion.SkipBelow(tfversion.Version1_14_0)},
		PreCheck:                 func() { acctest.PreCheck(t) },
		ProtoV6ProviderFactories: acctest.ProtoV6ProviderFactories,
		CheckDestroy:             testAccCheckInstanceDestroy(t),
		Steps: []resource.TestStep{
			{
				Config: testAccInstanceMigrateConfig(image, cluster, hostID, "1", fmt.Sprintf("host = %q", host2)),
				Check:  resource.ComposeAggregateTestCheckFunc(testAccCaptureID(rn, &id), testAccCheckInstanceOnHost(t, rn, host2)),
			},
			{
				Config: testAccInstanceMigrateConfig(image, cluster, hostID, "2", `mode = "cold"`),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrPtr(rn, "id", &id),
					testAccCheckInstanceOnHost(t, rn, hostID),
					resource.TestCheckResourceAttr(rn, "status", "ACTIVE"),
				),
			},
		},
	})
}

func testAccMigrateEnv(t *testing.T) (image, cluster, hostID string) {
	t.Helper()
	if os.Getenv("PCD_ACC_ADMIN_COMPUTE") == "" {
		t.Skip("PCD_ACC_ADMIN_COMPUTE not set; skipping a migration test")
	}
	hostID = os.Getenv("PCD_ACC_HOST_ID")
	if hostID == "" {
		t.Skip("PCD_ACC_HOST_ID not set; it names the hypervisor the test instance boots on")
	}
	return testAccBootImageName(t), testAccClusterName(t), hostID
}

// testAccInstanceMigrateConfig boots a pinned instance and runs the migrate
// action after it is created and whenever generation changes. args are
// extra lines for the action's config block.
func testAccInstanceMigrateConfig(image, cluster, hostID, generation, args string) string {
	return testAccPinnedInstanceConfig(image, cluster, hostID, "tf-acc-migrate") + fmt.Sprintf(`
terraform {
  required_version = ">= 1.14"
}

action "pcd_compute_instance_migrate" "test" {
  config {
    instance_id = pcd_compute_instance.pinned.id
    %[2]s
  }
}

resource "terraform_data" "migrate" {
  input = %[1]q
  lifecycle {
    action_trigger {
      events  = [after_create, after_update]
      actions = [action.pcd_compute_instance_migrate.test]
    }
  }
  depends_on = [pcd_compute_instance.pinned]
}
`, generation, args)
}
