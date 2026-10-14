// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package compute_test

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"testing"

	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/servers"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
	"github.com/hashicorp/terraform-plugin-testing/tfversion"

	"github.com/platform9/terraform-provider-pcd/internal/acctest"
	"github.com/platform9/terraform-provider-pcd/internal/clients"
)

// The drain moves every instance on the host, whoever owns it, so these tests
// refuse to start unless the host is empty: testAccDrainHost skips when it
// finds any instance there. They also disable scheduling on the host for
// their duration, so they are opt-in: PCD_ACC_ADMIN_COMPUTE=1 plus PCD_ACC_HOST_ID.

// TestAccComputeHostDrain_emptyHost puts an empty host in maintenance and
// drains it: nothing moves, the action succeeds, destroy re-enables the host.
func TestAccComputeHostDrain_emptyHost(t *testing.T) {
	hostID := testAccDrainHost(t)
	resource.Test(t, resource.TestCase{
		TerraformVersionChecks:   []tfversion.TerraformVersionCheck{tfversion.SkipBelow(tfversion.Version1_14_0)},
		PreCheck:                 func() { acctest.PreCheck(t) },
		ProtoV6ProviderFactories: acctest.ProtoV6ProviderFactories,
		CheckDestroy:             testAccCheckComputeServiceEnabled(t, hostID),
		Steps: []resource.TestStep{{
			Config: testAccHostDrainConfig(hostID, ""),
			Check: resource.ComposeAggregateTestCheckFunc(
				resource.TestCheckResourceAttr("pcd_compute_service.drain", "status", "maintenance"),
				testAccCheckComputeServiceRemote(t, hostID, "disabled", uiMaintenanceReason),
			),
		}},
	})
}

// TestAccComputeHostDrain_noTarget boots one instance on the host and drains
// it with nowhere to go: the drain must fail and name the instance, which
// stays ACTIVE where it was. Runs only without a second hypervisor
// (PCD_ACC_HOST2_ID unset); it needs PCD_ACC_IMAGE_NAME and
// PCD_ACC_CLUSTER_NAME (the host's cluster, to pin the instance to it).
func TestAccComputeHostDrain_noTarget(t *testing.T) {
	hostID := testAccDrainHost(t)
	if os.Getenv("PCD_ACC_HOST2_ID") != "" {
		t.Skip("PCD_ACC_HOST2_ID is set, so the instance has somewhere to go; TestAccComputeHostDrain_moves covers that")
	}
	image := testAccBootImageName(t)
	cluster := testAccClusterName(t)
	resource.Test(t, resource.TestCase{
		TerraformVersionChecks:   []tfversion.TerraformVersionCheck{tfversion.SkipBelow(tfversion.Version1_14_0)},
		PreCheck:                 func() { acctest.PreCheck(t) },
		ProtoV6ProviderFactories: acctest.ProtoV6ProviderFactories,
		CheckDestroy: resource.ComposeAggregateTestCheckFunc(
			testAccCheckInstanceDestroy(t), testAccCheckComputeServiceEnabled(t, hostID)),
		Steps: []resource.TestStep{
			{
				Config:      testAccHostDrainConfig(hostID, testAccPinnedInstanceConfig(image, cluster, hostID, "tf-acc-drain")),
				ExpectError: regexp.MustCompile(`Host drain incomplete`),
			},
			// The same configuration again: nothing to apply (the action ran on
			// create only), and the instance is still on the host.
			{
				Config: testAccHostDrainConfig(hostID, testAccPinnedInstanceConfig(image, cluster, hostID, "tf-acc-drain")),
				Check:  testAccCheckInstanceOnHost(t, "pcd_compute_instance.pinned", hostID),
			},
		},
	})
}

// TestAccComputeHostDrain_moves drains one instance to a second hypervisor
// in the same cluster (PCD_ACC_HOST2_ID), then checks Nova has it there.
func TestAccComputeHostDrain_moves(t *testing.T) {
	hostID := testAccDrainHost(t)
	host2 := os.Getenv("PCD_ACC_HOST2_ID")
	if host2 == "" {
		t.Skip("PCD_ACC_HOST2_ID not set; a successful drain needs a second hypervisor in the cluster")
	}
	image := testAccBootImageName(t)
	cluster := testAccClusterName(t)
	resource.Test(t, resource.TestCase{
		TerraformVersionChecks:   []tfversion.TerraformVersionCheck{tfversion.SkipBelow(tfversion.Version1_14_0)},
		PreCheck:                 func() { acctest.PreCheck(t) },
		ProtoV6ProviderFactories: acctest.ProtoV6ProviderFactories,
		CheckDestroy: resource.ComposeAggregateTestCheckFunc(
			testAccCheckInstanceDestroy(t), testAccCheckComputeServiceEnabled(t, hostID)),
		Steps: []resource.TestStep{{
			Config: testAccHostDrainConfig(hostID, testAccPinnedInstanceConfig(image, cluster, hostID, "tf-acc-drain")),
			Check:  testAccCheckInstanceOnHost(t, "pcd_compute_instance.pinned", host2),
		}},
	})
}

// testAccDrainHost gates the drain tests and refuses a host with instances.
func testAccDrainHost(t *testing.T) string {
	t.Helper()
	if os.Getenv("PCD_ACC_ADMIN_COMPUTE") == "" {
		t.Skip("PCD_ACC_ADMIN_COMPUTE not set; skipping a test that disables scheduling on a shared hypervisor")
	}
	hostID := os.Getenv("PCD_ACC_HOST_ID")
	if hostID == "" {
		t.Skip("PCD_ACC_HOST_ID not set; it names the hypervisor to drain")
	}
	acctest.PreCheck(t)
	client, err := acctest.LabConfig(t).ComputeV2Client()
	if err != nil {
		t.Fatal(err)
	}
	pages, err := servers.List(client, servers.ListOpts{AllTenants: true, Host: hostID}).AllPages(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	onHost, err := servers.ExtractServers(pages)
	if err != nil {
		t.Fatal(err)
	}
	if len(onHost) > 0 {
		t.Skipf("host %s runs %d instances; the drain would move them all, so it runs only on an empty host", hostID, len(onHost))
	}
	return hostID
}

func testAccClusterName(t *testing.T) string {
	t.Helper()
	cluster := os.Getenv("PCD_ACC_CLUSTER_NAME")
	if cluster == "" {
		t.Skip("PCD_ACC_CLUSTER_NAME not set; it pins the test instance to PCD_ACC_HOST_ID's cluster")
	}
	return cluster
}

// testAccHostDrainConfig puts the host in maintenance and runs the drain once
// that is applied. extra is configuration the service must wait for (an
// instance that has to boot on the host before it is disabled).
func testAccHostDrainConfig(hostID, extra string) string {
	dependsOn := ""
	if extra != "" {
		dependsOn = "depends_on = [pcd_compute_instance.pinned]"
	}
	return fmt.Sprintf(`
terraform {
  required_version = ">= 1.14"
}

%[2]s

resource "pcd_compute_service" "drain" {
  host_id = %[1]q
  status  = "maintenance"
  %[3]s
}

action "pcd_compute_host_drain" "drain" {
  config {
    host_id = pcd_compute_service.drain.host_id
  }
}

resource "terraform_data" "drain" {
  input = pcd_compute_service.drain.disabled_reason
  lifecycle {
    action_trigger {
      events  = [after_create]
      actions = [action.pcd_compute_host_drain.drain]
    }
  }
}
`, hostID, extra, dependsOn)
}

// testAccPinnedInstanceConfig boots an instance on hostID with the admin
// "<zone>:<host>" pin; PCD's zone is the cluster name and its compute host
// name is the resmgr host UUID.
func testAccPinnedInstanceConfig(image, cluster, hostID, name string) string {
	return fmt.Sprintf(`
data "pcd_images_image" "boot" {
  name = %[1]q
}

resource "pcd_networking_network" "pinned" {
  name = "%[4]s-net"
}

resource "pcd_networking_subnet" "pinned" {
  network_id = pcd_networking_network.pinned.id
  cidr       = "10.141.0.0/24"
}

resource "pcd_compute_instance" "pinned" {
  name              = %[4]q
  image_id          = data.pcd_images_image.boot.id
  flavor_name       = "m1.small"
  availability_zone = "%[2]s:%[3]s"

  network {
    uuid = pcd_networking_network.pinned.id
  }

  depends_on = [pcd_networking_subnet.pinned]
}
`, image, cluster, hostID, name)
}

// testAccCheckInstanceOnHost reads where Nova runs the instance (admin only).
func testAccCheckInstanceOnHost(t *testing.T, n, hostID string) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		rs := s.RootModule().Resources[n]
		if rs == nil {
			return fmt.Errorf("not found in state: %s", n)
		}
		client, err := acctest.LabConfig(t).ComputeV2Client()
		if err != nil {
			return err
		}
		srv, err := clients.RequireObject(servers.Get(context.Background(), client, rs.Primary.ID).Extract())
		if err == nil && srv.ID == "" {
			err = clients.ErrNoObject
		}
		if err != nil {
			return err
		}
		if srv.Host != hostID {
			return fmt.Errorf("instance %s runs on %q, want %q", rs.Primary.ID, srv.Host, hostID)
		}
		return nil
	}
}
