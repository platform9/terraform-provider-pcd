// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package compute_test

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"testing"

	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/services"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"

	"github.com/platform9/terraform-provider-pcd/internal/acctest"
)

var uiMaintenanceReason = regexp.MustCompile(`^[0-9a-f-]{36}:[0-9]{13}$`)

// TestAccComputeService disables scheduling on a hypervisor, puts it in
// maintenance, re-enables it, disables it again, imports it, and lets destroy
// re-enable it. While the host is disabled nothing
// can be scheduled onto it, and on a single-hypervisor cluster the PCD UI
// warns most VM operations fail, so this is opt-in and must run alone:
// PCD_ACC_ADMIN_COMPUTE=1 and PCD_ACC_HOST_ID (the hypervisor's resmgr UUID).
// If a run is interrupted, re-enable the host from the PCD UI (Enable
// Scheduling) or with `pcdctl compute service set --enable <host_id> nova-compute`.
func TestAccComputeService(t *testing.T) {
	if os.Getenv("PCD_ACC_ADMIN_COMPUTE") == "" {
		t.Skip("PCD_ACC_ADMIN_COMPUTE not set; skipping a test that disables scheduling on a shared hypervisor")
	}
	hostID := os.Getenv("PCD_ACC_HOST_ID")
	if hostID == "" {
		t.Skip("PCD_ACC_HOST_ID not set; it names the hypervisor whose scheduling the test toggles")
	}
	const rn = "pcd_compute_service.test"
	update := resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(rn, plancheck.ResourceActionUpdate)}}
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { acctest.PreCheck(t) },
		ProtoV6ProviderFactories: acctest.ProtoV6ProviderFactories,
		CheckDestroy:             testAccCheckComputeServiceEnabled(t, hostID),
		Steps: []resource.TestStep{
			{
				Config: testAccComputeServiceConfig(hostID, "disabled"),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(rn, "status", "disabled"),
					resource.TestCheckNoResourceAttr(rn, "disabled_reason"),
					resource.TestCheckResourceAttrSet(rn, "service_id"),
					testAccCheckComputeServiceRemote(t, hostID, "disabled", nil),
				),
			},
			{
				Config:           testAccComputeServiceConfig(hostID, "maintenance"),
				ConfigPlanChecks: update,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(rn, "status", "maintenance"),
					resource.TestMatchResourceAttr(rn, "disabled_reason", uiMaintenanceReason),
					testAccCheckComputeServiceRemote(t, hostID, "disabled", uiMaintenanceReason),
				),
			},
			{
				Config:           testAccComputeServiceConfig(hostID, "enabled"),
				ConfigPlanChecks: update,
				Check:            testAccCheckComputeServiceRemote(t, hostID, "enabled", nil),
			},
			// End disabled so that destroy has to re-enable the host, which
			// CheckDestroy then confirms.
			{
				Config:           testAccComputeServiceConfig(hostID, "disabled"),
				ConfigPlanChecks: update,
				Check:            testAccCheckComputeServiceRemote(t, hostID, "disabled", nil),
			},
			// compute_state is Nova's live up/down report and may flip between
			// the apply and the import.
			{ResourceName: rn, ImportState: true, ImportStateVerify: true, ImportStateVerifyIgnore: []string{"compute_state"}},
		},
	})
}

func testAccComputeServiceConfig(hostID, status string) string {
	return fmt.Sprintf(`
resource "pcd_compute_service" "test" {
  host_id = %q
  status  = %q
}
`, hostID, status)
}

// testAccCheckComputeServiceRemote reads the service from Nova. A nil reason
// pattern requires no reason.
func testAccCheckComputeServiceRemote(t *testing.T, hostID, status string, reason *regexp.Regexp) resource.TestCheckFunc {
	return func(*terraform.State) error {
		svc, err := testAccComputeServiceFor(t, hostID)
		if err != nil {
			return err
		}
		if svc.Status != status {
			return fmt.Errorf("nova-compute on %s is %s, want %s", hostID, svc.Status, status)
		}
		if reason == nil && svc.DisabledReason != "" {
			return fmt.Errorf("nova-compute on %s has reason %q, want none", hostID, svc.DisabledReason)
		}
		if reason != nil && !reason.MatchString(svc.DisabledReason) {
			return fmt.Errorf("nova-compute on %s has reason %q, want <uuid>:<epoch ms>", hostID, svc.DisabledReason)
		}
		return nil
	}
}

func testAccCheckComputeServiceEnabled(t *testing.T, hostID string) resource.TestCheckFunc {
	return testAccCheckComputeServiceRemote(t, hostID, "enabled", nil)
}

func testAccComputeServiceFor(t *testing.T, hostID string) (*services.Service, error) {
	client, err := acctest.LabConfig(t).ComputeV2Client()
	if err != nil {
		return nil, err
	}
	pages, err := services.List(client, services.ListOpts{Binary: "nova-compute", Host: hostID}).AllPages(context.Background())
	if err != nil {
		return nil, err
	}
	all, err := services.ExtractServices(pages)
	if err != nil {
		return nil, err
	}
	if len(all) != 1 {
		return nil, fmt.Errorf("%d nova-compute services for host %s", len(all), hostID)
	}
	return &all[0], nil
}
