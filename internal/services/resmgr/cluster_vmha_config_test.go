// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package resmgr_test

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"

	"github.com/platform9/terraform-provider-pcd/internal/acctest"
)

// TestAccResmgrClusterVMHAConfig creates a cluster with no hosts and VM high
// availability on, sets its evacuation speed with no heartbeat path, then a
// speed and a heartbeat path, then clears the path alone, and imports the
// configuration. It writes to the VM high-availability service, which keeps
// the cluster's configuration after the test (it has no delete), so it is
// opt-in: PCD_ACC_VMHA=1. The first step sets both settings, so what an
// earlier, stopped run left in that configuration cannot fail this one. Step
// 1 can wait several minutes: hamgr can take up to five minutes to learn of
// the cluster the step has just created.
func TestAccResmgrClusterVMHAConfig(t *testing.T) {
	if os.Getenv("PCD_ACC_VMHA") == "" {
		t.Skip("PCD_ACC_VMHA not set; skipping a VM high-availability configuration test")
	}
	const rn = "pcd_cluster_vmha_config.test"
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { acctest.PreCheck(t) },
		ProtoV6ProviderFactories: acctest.ProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: testAccClusterVMHAConfig(`evacuation_speed = "balanced"` + "\n  nfs_heartbeat_path = \"\""),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(rn, "id", "tf-acc-vmha"),
					resource.TestCheckResourceAttr(rn, "evacuation_speed", "balanced"),
					resource.TestCheckResourceAttr(rn, "nfs_heartbeat_path", ""),
					testAccCheckVMHAConfigRemote(t, "balanced", ""),
				),
			},
			{
				Config: testAccClusterVMHAConfig(`evacuation_speed = "aggressive"` + "\n  nfs_heartbeat_path = \"/mnt/tf-acc-vmha\""),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(rn, "nfs_heartbeat_path", "/mnt/tf-acc-vmha"),
					testAccCheckVMHAConfigRemote(t, "aggressive", "/mnt/tf-acc-vmha"),
				),
			},
			{
				Config: testAccClusterVMHAConfig(`nfs_heartbeat_path = ""`),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(rn, "evacuation_speed", "aggressive"),
					resource.TestCheckResourceAttr(rn, "nfs_heartbeat_path", ""),
					testAccCheckVMHAConfigRemote(t, "aggressive", ""),
				),
			},
			{ResourceName: rn, ImportState: true, ImportStateId: "tf-acc-vmha", ImportStateVerify: true},
		},
	})
}

func testAccClusterVMHAConfig(settings string) string {
	return fmt.Sprintf(`
resource "pcd_cluster" "test" {
  name = "tf-acc-vmha"

  vm_high_availability = {
    enabled = true
  }
}

resource "pcd_cluster_vmha_config" "test" {
  cluster_name = pcd_cluster.test.name
  %s
}
`, settings)
}

// testAccCheckVMHAConfigRemote reads tf-acc-vmha's configuration from the VM
// high-availability service and compares the mode and heartbeat path.
func testAccCheckVMHAConfigRemote(t *testing.T, mode, path string) resource.TestCheckFunc {
	return func(*terraform.State) error {
		client, err := acctest.LabConfig(t).HamgrV1Client()
		if err != nil {
			return err
		}
		var got struct {
			Mode       string `json:"mode"`
			HostConfig struct {
				NFSHeartbeatPath string `json:"nfs_heartbeat_path"`
			} `json:"host_config"`
		}
		if _, err := client.Get(context.Background(), client.ServiceURL("vmha", "config", "tf-acc-vmha"), &got, nil); err != nil {
			return err
		}
		if got.Mode != mode || got.HostConfig.NFSHeartbeatPath != path {
			return fmt.Errorf("hamgr holds mode %q and path %q, want %q and %q", got.Mode, got.HostConfig.NFSHeartbeatPath, mode, path)
		}
		return nil
	}
}
