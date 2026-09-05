// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package resmgr_test

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"

	"github.com/platform9/terraform-provider-pcd/internal/acctest"
)

// TestAccResmgrBlueprintDataSource reads an existing blueprint by name. Set
// PCD_ACC_BLUEPRINT_NAME to an existing blueprint to run it (read-only).
func TestAccResmgrBlueprintDataSource(t *testing.T) {
	name := os.Getenv("PCD_ACC_BLUEPRINT_NAME")
	if name == "" {
		t.Skip("PCD_ACC_BLUEPRINT_NAME not set; skipping blueprint data source test")
	}
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { acctest.PreCheck(t) },
		ProtoV6ProviderFactories: acctest.ProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: fmt.Sprintf(`data "pcd_cluster_blueprint" "test" { name = %q }`, name),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("data.pcd_cluster_blueprint.test", "name", name),
					resource.TestCheckResourceAttrSet("data.pcd_cluster_blueprint.test", "networking_type"),
				),
			},
		},
	})
}

// TestAccResmgrHostConfig creates a host configuration, adds a network label
// (an in-place update), moves it to another interface (a replacement: resmgr
// refuses to change the interface an existing label maps to, PCD-9803), and
// imports it. This mutates the control
// plane, so it is opt-in: set PCD_ACC_RESMGR=1 to run, and PCD_ACC_BLUEPRINT_NAME
// to the region's cluster blueprint: resmgr refuses to create a host
// configuration that does not name one (404 ClusterBlueprintNotFound) or that
// leaves any traffic interface unset (400 HostconfBadConfigInput), so the
// configuration under test names the blueprint and puts every traffic type on
// the one interface.
func TestAccResmgrHostConfig(t *testing.T) {
	if os.Getenv("PCD_ACC_RESMGR") == "" {
		t.Skip("PCD_ACC_RESMGR not set; skipping resmgr mutation test")
	}
	blueprint := os.Getenv("PCD_ACC_BLUEPRINT_NAME")
	if blueprint == "" {
		t.Skip("PCD_ACC_BLUEPRINT_NAME not set; resmgr needs a cluster blueprint to create a host config")
	}
	const rn = "pcd_host_config.test"
	var firstID string
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { acctest.PreCheck(t) },
		ProtoV6ProviderFactories: acctest.ProtoV6ProviderFactories,
		CheckDestroy:             testAccCheckHostConfigDestroy(t),
		Steps: []resource.TestStep{
			{
				Config: testAccHostConfigConfig("tf-acc-hc", blueprint, "enp1s0", map[string]string{"physnet1": "enp1s0"}),
				Check: resource.ComposeAggregateTestCheckFunc(
					testAccCheckHostConfigExists(t, rn),
					resource.TestCheckResourceAttr(rn, "name", "tf-acc-hc"),
					resource.TestCheckResourceAttr(rn, "mgmt_interface", "enp1s0"),
					resource.TestCheckResourceAttr(rn, "network_labels.physnet1", "enp1s0"),
					testAccRememberID(rn, &firstID),
				),
			},
			// A second label on another interface: resmgr accepts additions, so this
			// is an in-place update and the id stays.
			{
				Config: testAccHostConfigConfig("tf-acc-hc", blueprint, "enp1s0", map[string]string{"physnet1": "enp1s0", "physnet2": "enp3s0"}),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(rn, plancheck.ResourceActionUpdate)},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					testAccCheckHostConfigExists(t, rn),
					resource.TestCheckResourceAttr(rn, "network_labels.physnet2", "enp3s0"),
					testAccCheckIDUnchanged(rn, &firstID),
				),
			},
			// The host moves to another interface, label included (the PCD-9803
			// scenario: a bond renamed under a physnet). The interfaces alone would
			// update in place; the changed label value makes it a replacement, so
			// the plan must be one and the new configuration must carry a new id.
			// resmgr insists the tunneling interface carries a label, so the label
			// cannot move alone.
			{
				Config: testAccHostConfigConfig("tf-acc-hc", blueprint, "enp2s0", map[string]string{"physnet1": "enp2s0", "physnet2": "enp3s0"}),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(rn, plancheck.ResourceActionReplace)},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					testAccCheckHostConfigExists(t, rn),
					resource.TestCheckResourceAttr(rn, "mgmt_interface", "enp2s0"),
					resource.TestCheckResourceAttr(rn, "network_labels.physnet1", "enp2s0"),
					testAccCheckIDChanged(rn, &firstID),
				),
			},
			{ResourceName: rn, ImportState: true, ImportStateVerify: true},
		},
	})
}

// testAccHostConfigConfig puts every traffic type on iface and renders labels
// (sorted, so the configuration is stable across steps).
func testAccHostConfigConfig(name, blueprint, iface string, labels map[string]string) string {
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		fmt.Fprintf(&b, "    %s = %q\n", k, labels[k])
	}
	return fmt.Sprintf(`
resource "pcd_host_config" "test" {
  name         = %q
  cluster_name = %q

  mgmt_interface           = %[3]q
  vm_console_interface     = %[3]q
  host_liveness_interface  = %[3]q
  tunneling_interface      = %[3]q
  imagelib_interface       = %[3]q
  live_migration_interface = %[3]q

  network_labels = {
%s  }
}
`, name, blueprint, iface, b.String())
}

// testAccRememberID stores the resource's current id for a later step to compare against.
func testAccRememberID(n string, into *string) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		rs, ok := s.RootModule().Resources[n]
		if !ok {
			return fmt.Errorf("not found in state: %s", n)
		}
		*into = rs.Primary.ID
		return nil
	}
}

// testAccCheckIDUnchanged fails when the resource lost the id an earlier step remembered.
func testAccCheckIDUnchanged(n string, was *string) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		rs, ok := s.RootModule().Resources[n]
		if !ok {
			return fmt.Errorf("not found in state: %s", n)
		}
		if rs.Primary.ID != *was {
			return fmt.Errorf("%s changed id from %s to %s on a label addition; it should have been updated in place", n, *was, rs.Primary.ID)
		}
		return nil
	}
}

// testAccCheckIDChanged fails when the resource kept the id an earlier step remembered.
func testAccCheckIDChanged(n string, was *string) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		rs, ok := s.RootModule().Resources[n]
		if !ok {
			return fmt.Errorf("not found in state: %s", n)
		}
		if rs.Primary.ID == *was {
			return fmt.Errorf("%s kept id %s across a network_labels change; it should have been replaced", n, *was)
		}
		return nil
	}
}

func testAccCheckHostConfigExists(t *testing.T, n string) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		rs, ok := s.RootModule().Resources[n]
		if !ok {
			return fmt.Errorf("not found in state: %s", n)
		}
		client, err := acctest.LabConfig(t).ResmgrV2Client()
		if err != nil {
			return err
		}
		var hc map[string]any
		if _, err := client.Get(context.Background(), client.ServiceURL("hostconfigs", rs.Primary.ID), &hc, &gophercloud.RequestOpts{OkCodes: []int{200}}); err != nil {
			return fmt.Errorf("host config %s not found via API: %w", rs.Primary.ID, err)
		}
		return nil
	}
}

func testAccCheckHostConfigDestroy(t *testing.T) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		client, err := acctest.LabConfig(t).ResmgrV2Client()
		if err != nil {
			return err
		}
		for _, rs := range s.RootModule().Resources {
			if rs.Type != "pcd_host_config" {
				continue
			}
			var hc map[string]any
			_, err := client.Get(context.Background(), client.ServiceURL("hostconfigs", rs.Primary.ID), &hc, &gophercloud.RequestOpts{OkCodes: []int{200}})
			if err == nil {
				return fmt.Errorf("host config %s still exists", rs.Primary.ID)
			}
			if !gophercloud.ResponseCodeIs(err, 404) {
				return fmt.Errorf("unexpected error checking host config %s: %w", rs.Primary.ID, err)
			}
		}
		return nil
	}
}

// TestAccResmgrHostClusterRoleImport assigns a cluster role, then imports it
// into a fresh working directory and lets ImportStateVerify compare every
// imported attribute against the state created in the first step. Before the
// fix, the imported state left wait_until_converged null while the created
// state held false, so ImportStateVerify failed on the pre-fix code and
// passes on the fixed code.
// Mutates the control plane, so it is opt-in: PCD_ACC_RESMGR=1 and
// PCD_ACC_HOST_ID=<host uuid> (an onboarded host that has a host configuration
// assigned). PCD_ACC_CLUSTER_ROLE picks the role (default image-library); the
// test adds it and removes it again.
func TestAccResmgrHostClusterRoleImport(t *testing.T) {
	if os.Getenv("PCD_ACC_RESMGR") == "" {
		t.Skip("PCD_ACC_RESMGR not set; skipping resmgr mutation test")
	}
	hostID := os.Getenv("PCD_ACC_HOST_ID")
	if hostID == "" {
		t.Skip("PCD_ACC_HOST_ID not set; skipping cluster role import test")
	}
	role := os.Getenv("PCD_ACC_CLUSTER_ROLE")
	if role == "" {
		role = "image-library"
	}
	const rn = "pcd_host_cluster_role.test"
	cfg := fmt.Sprintf(`
resource "pcd_host_cluster_role" "test" {
  host_id = %q
  role    = %q
}
`, hostID, role)
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { acctest.PreCheck(t) },
		ProtoV6ProviderFactories: acctest.ProtoV6ProviderFactories,
		CheckDestroy:             testAccCheckHostClusterRoleDestroy(t, hostID, role),
		Steps: []resource.TestStep{
			{
				Config: cfg,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(rn, "id", hostID+"/"+role),
					resource.TestCheckResourceAttr(rn, "wait_until_converged", "false"),
				),
			},
			// ImportState runs in its own, fresh working directory; ImportStateVerify
			// then diffs every attribute of that imported state against the state
			// created above. That diff is the regression guard: before the fix, the
			// imported state left wait_until_converged null while the created state
			// held false, so this comparison failed.
			{ResourceName: rn, ImportState: true, ImportStateId: hostID + "/" + role, ImportStateVerify: true},
		},
	})
}

// testAccCheckHostClusterRoleDestroy polls the host record until role is no
// longer among its roles, or the host itself is gone. Deauthorising a role is
// asynchronous, so a single check right after destroy can observe the role
// still present; this polls instead of asserting once.
func testAccCheckHostClusterRoleDestroy(t *testing.T, hostID, role string) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		client, err := acctest.LabConfig(t).ResmgrV2Client()
		if err != nil {
			return err
		}
		const (
			interval = 10 * time.Second
			timeout  = 5 * time.Minute
		)
		ctx := context.Background()
		deadline := time.Now().Add(timeout)
		for {
			var host struct {
				Roles []string `json:"roles"`
			}
			_, err := client.Get(ctx, client.ServiceURL("hosts", hostID), &host, &gophercloud.RequestOpts{OkCodes: []int{200}})
			switch {
			case gophercloud.ResponseCodeIs(err, 404):
				return nil // the host itself is gone, so the role is certainly gone
			case err != nil:
				return fmt.Errorf("checking host %s for role %s: %w", hostID, role, err)
			}
			cleared := true
			for _, r := range host.Roles {
				if r == role {
					cleared = false
					break
				}
			}
			if cleared {
				return nil
			}
			if time.Now().After(deadline) {
				return fmt.Errorf("role %s still assigned to host %s after %s", role, hostID, timeout)
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(interval):
			}
		}
	}
}
