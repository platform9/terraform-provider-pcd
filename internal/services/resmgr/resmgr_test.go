// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package resmgr_test

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
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

// TestAccResmgrHostConfig creates, updates, and imports a host configuration.
// This mutates the control plane, so it is opt-in: set PCD_ACC_RESMGR=1 to run.
func TestAccResmgrHostConfig(t *testing.T) {
	if os.Getenv("PCD_ACC_RESMGR") == "" {
		t.Skip("PCD_ACC_RESMGR not set; skipping resmgr mutation test")
	}
	const rn = "pcd_host_config.test"
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { acctest.PreCheck(t) },
		ProtoV6ProviderFactories: acctest.ProtoV6ProviderFactories,
		CheckDestroy:             testAccCheckHostConfigDestroy(t),
		Steps: []resource.TestStep{
			{
				Config: testAccHostConfigConfig("tf-acc-hc", "enp1s0"),
				Check: resource.ComposeAggregateTestCheckFunc(
					testAccCheckHostConfigExists(t, rn),
					resource.TestCheckResourceAttr(rn, "name", "tf-acc-hc"),
					resource.TestCheckResourceAttr(rn, "mgmt_interface", "enp1s0"),
					resource.TestCheckResourceAttr(rn, "network_labels.physnet1", "enp1s0"),
				),
			},
			{ResourceName: rn, ImportState: true, ImportStateVerify: true},
		},
	})
}

func testAccHostConfigConfig(name, iface string) string {
	return fmt.Sprintf(`
resource "pcd_host_config" "test" {
  name           = %q
  mgmt_interface = %q
  network_labels = {
    physnet1 = %q
  }
}
`, name, iface, iface)
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
