// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package compute_test

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"testing"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/secgroups"
	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/servers"
	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/ports"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"

	"github.com/platform9/terraform-provider-pcd/internal/acctest"
)

// testAccBootImageName returns the name of a pre-synced bootable image to boot
// VMs from, taken from PCD_ACC_IMAGE_NAME. PCD's image library does not propagate
// web-download (copy-from) images to the hypervisor's local Glance, so the VM
// boot tests require an image already uploaded to the library (e.g. a CirrOS
// image). The test skips when the variable is unset.
func testAccBootImageName(t *testing.T) string {
	t.Helper()
	name := os.Getenv("PCD_ACC_IMAGE_NAME")
	if name == "" {
		t.Skip("PCD_ACC_IMAGE_NAME not set; skipping VM-boot test (needs a bootable image already in the image library)")
	}
	return name
}

// TestAccComputeInstance_basic boots a real VM through Terraform end to end:
// network + subnet + Glance image (web-download) + keypair + instance on m1.tiny.
func TestAccComputeInstance_basic(t *testing.T) {
	imageName := testAccBootImageName(t)
	const rn = "pcd_compute_instance.test"
	var instanceID, flavorID string

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { acctest.PreCheck(t) },
		ProtoV6ProviderFactories: acctest.ProtoV6ProviderFactories,
		CheckDestroy:             testAccCheckInstanceDestroy(t),
		Steps: []resource.TestStep{
			{
				Config: testAccInstanceConfig(imageName, "tf-acc-instance", "m1.tiny"),
				Check: resource.ComposeAggregateTestCheckFunc(
					testAccCheckInstanceExists(t, rn),
					testAccCaptureID(rn, &instanceID),
					testAccCaptureAttr(rn, "flavor_id", &flavorID),
					resource.TestCheckResourceAttr(rn, "name", "tf-acc-instance"),
					resource.TestCheckResourceAttr(rn, "status", "ACTIVE"),
					resource.TestCheckResourceAttrSet(rn, "access_ip_v4"),
					resource.TestCheckResourceAttrSet(rn, "flavor_id"),
				),
			},
			{
				Config: testAccInstanceConfig(imageName, "tf-acc-instance-renamed", "m1.tiny"),
				Check:  resource.TestCheckResourceAttr(rn, "name", "tf-acc-instance-renamed"),
			},
			{
				// Resize: change flavor in place (same instance ID, new flavor_id).
				Config: testAccInstanceConfig(imageName, "tf-acc-instance-renamed", "m1.small"),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrWith(rn, "id", func(v string) error {
						if v != instanceID {
							return fmt.Errorf("instance was replaced (%s -> %s); flavor change should resize in place", instanceID, v)
						}
						return nil
					}),
					resource.TestCheckResourceAttrWith(rn, "flavor_id", func(v string) error {
						if v == flavorID {
							return fmt.Errorf("flavor_id did not change after resize (still %s)", v)
						}
						return nil
					}),
				),
			},
		},
	})
}

// TestAccComputeInstance_resizeRejected resizes an instance to a flavor no host
// can hold. Nova accepts the resize and fails it while scheduling; the apply
// must report that promptly instead of waiting 30 minutes for VERIFY_RESIZE.
// PCD_ACC_UNSCHEDULABLE_RAM_MB names a RAM size (MiB) larger than any
// hypervisor's free memory but inside the project's RAM quota; the test skips
// without it, because a region with a large enough host would resize.
func TestAccComputeInstance_resizeRejected(t *testing.T) {
	imageName := testAccBootImageName(t)
	ram, err := strconv.Atoi(os.Getenv("PCD_ACC_UNSCHEDULABLE_RAM_MB"))
	if err != nil || ram <= 0 {
		t.Skip("PCD_ACC_UNSCHEDULABLE_RAM_MB not set; skipping the rejected-resize test (needs a RAM size no hypervisor can hold)")
	}

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { acctest.PreCheck(t) },
		ProtoV6ProviderFactories: acctest.ProtoV6ProviderFactories,
		CheckDestroy:             testAccCheckInstanceDestroy(t),
		Steps: []resource.TestStep{
			{
				Config: testAccInstanceResizeRejectedConfig(imageName, ram, `flavor_name = "m1.small"`),
				Check:  resource.TestCheckResourceAttr("pcd_compute_instance.test", "status", "ACTIVE"),
			},
			{
				Config:      testAccInstanceResizeRejectedConfig(imageName, ram, `flavor_id = pcd_compute_flavor.huge.id`),
				ExpectError: regexp.MustCompile(`compute: resizing instance`),
			},
		},
	})
}

func testAccInstanceResizeRejectedConfig(imageName string, ramMB int, flavorLine string) string {
	return fmt.Sprintf(`
data "pcd_images_image" "boot" {
  name = %q
}

resource "pcd_compute_flavor" "huge" {
  name  = "tf-acc-flavor-huge"
  ram   = %d
  vcpus = 1
  disk  = 20
}

resource "pcd_networking_network" "test" {
  name = "tf-acc-resize-net"
}

resource "pcd_networking_subnet" "test" {
  network_id = pcd_networking_network.test.id
  cidr       = "10.119.0.0/24"
}

resource "pcd_compute_instance" "test" {
  name     = "tf-acc-resize"
  image_id = data.pcd_images_image.boot.id
  %s

  network {
    uuid = pcd_networking_network.test.id
  }

  depends_on = [pcd_networking_subnet.test]
}
`, imageName, ramMB, flavorLine)
}

// TestAccComputeInstance_imageName boots an instance referencing its image by
// name (resolved via Glance) instead of image_id.
func TestAccComputeInstance_imageName(t *testing.T) {
	imageName := testAccBootImageName(t)
	const rn = "pcd_compute_instance.test"

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { acctest.PreCheck(t) },
		ProtoV6ProviderFactories: acctest.ProtoV6ProviderFactories,
		CheckDestroy:             testAccCheckInstanceDestroy(t),
		Steps: []resource.TestStep{
			{
				Config: testAccInstanceImageNameConfig(imageName),
				Check: resource.ComposeAggregateTestCheckFunc(
					testAccCheckInstanceExists(t, rn),
					resource.TestCheckResourceAttr(rn, "status", "ACTIVE"),
					// image_id is Computed and gets populated from the resolved image_name.
					resource.TestCheckResourceAttrPair(rn, "image_id", "data.pcd_images_image.boot", "id"),
				),
			},
		},
	})
}

func testAccInstanceConfig(imageName, name, flavorName string) string {
	return fmt.Sprintf(`
data "pcd_images_image" "boot" {
  name = %q
}

resource "pcd_networking_network" "test" {
  name = "tf-acc-inst-net"
}

resource "pcd_networking_subnet" "test" {
  network_id = pcd_networking_network.test.id
  cidr       = "10.103.0.0/24"
}

resource "pcd_compute_keypair" "test" {
  name = "tf-acc-inst-key"
}

resource "pcd_compute_instance" "test" {
  name        = %q
  image_id    = data.pcd_images_image.boot.id
  flavor_name = %q
  key_pair    = pcd_compute_keypair.test.name

  network {
    uuid = pcd_networking_network.test.id
  }

  depends_on = [pcd_networking_subnet.test]
}
`, imageName, name, flavorName)
}

func testAccInstanceImageNameConfig(imageName string) string {
	return fmt.Sprintf(`
data "pcd_images_image" "boot" {
  name = %[1]q
}

resource "pcd_networking_network" "test" {
  name = "tf-acc-instn-net"
}

resource "pcd_networking_subnet" "test" {
  network_id = pcd_networking_network.test.id
  cidr       = "10.113.0.0/24"
}

resource "pcd_compute_instance" "test" {
  name        = "tf-acc-instn"
  image_name  = %[1]q
  flavor_name = "m1.tiny"

  network {
    uuid = pcd_networking_network.test.id
  }

  depends_on = [pcd_networking_subnet.test]
}
`, imageName)
}

// testAccCaptureAttr records a resource attribute value for later comparison.
func testAccCaptureAttr(n, attr string, dst *string) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		rs := s.RootModule().Resources[n]
		if rs == nil {
			return fmt.Errorf("not found in state: %s", n)
		}
		*dst = rs.Primary.Attributes[attr]
		return nil
	}
}

func testAccCheckInstanceExists(t *testing.T, n string) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		rs := s.RootModule().Resources[n]
		if rs == nil {
			return fmt.Errorf("not found in state: %s", n)
		}
		client, err := acctest.LabConfig(t).ComputeV2Client()
		if err != nil {
			return err
		}
		if _, err := servers.Get(context.Background(), client, rs.Primary.ID).Extract(); err != nil {
			return fmt.Errorf("instance %s not found: %w", rs.Primary.ID, err)
		}
		return nil
	}
}

func testAccCheckInstanceDestroy(t *testing.T) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		client, err := acctest.LabConfig(t).ComputeV2Client()
		if err != nil {
			return err
		}
		for _, rs := range s.RootModule().Resources {
			if rs.Type != "pcd_compute_instance" {
				continue
			}
			_, err := servers.Get(context.Background(), client, rs.Primary.ID).Extract()
			if err == nil {
				return fmt.Errorf("instance %s still exists", rs.Primary.ID)
			}
			if !gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
				return err
			}
		}
		return nil
	}
}

// TestAccComputeInstance_powerState boots an instance stopped, resizes it while
// it stays stopped, then starts, pauses and suspends it in place, checks that
// dropping power_state from the configuration plans nothing, and imports it.
func TestAccComputeInstance_powerState(t *testing.T) {
	imageName := testAccBootImageName(t)
	const rn = "pcd_compute_instance.test"
	var instanceID, flavorID string
	sameInstance := resource.TestCheckResourceAttrWith(rn, "id", func(v string) error {
		if v != instanceID {
			return fmt.Errorf("instance was replaced (%s -> %s); power changes must apply in place", instanceID, v)
		}
		return nil
	})

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { acctest.PreCheck(t) },
		ProtoV6ProviderFactories: acctest.ProtoV6ProviderFactories,
		CheckDestroy:             testAccCheckInstanceDestroy(t),
		Steps: []resource.TestStep{
			{
				Config: testAccInstancePowerStateConfig(imageName, "m1.small", "shutoff"),
				Check: resource.ComposeAggregateTestCheckFunc(
					testAccCaptureID(rn, &instanceID),
					testAccCaptureAttr(rn, "flavor_id", &flavorID),
					resource.TestCheckResourceAttr(rn, "power_state", "shutoff"),
					resource.TestCheckResourceAttr(rn, "status", "SHUTOFF"),
					testAccCheckInstanceStatus(t, rn, "SHUTOFF"),
				),
			},
			{
				// A resize of a stopped instance leaves it stopped; the
				// provider used to wait for ACTIVE here until it timed out.
				Config: testAccInstancePowerStateConfig(imageName, "m1.medium", "shutoff"),
				Check: resource.ComposeAggregateTestCheckFunc(
					sameInstance,
					resource.TestCheckResourceAttrWith(rn, "flavor_id", func(v string) error {
						if v == flavorID {
							return fmt.Errorf("flavor_id did not change after resize (still %s)", v)
						}
						return nil
					}),
					resource.TestCheckResourceAttr(rn, "power_state", "shutoff"),
					testAccCheckInstanceStatus(t, rn, "SHUTOFF"),
				),
			},
			{
				Config: testAccInstancePowerStateConfig(imageName, "m1.medium", "active"),
				Check: resource.ComposeAggregateTestCheckFunc(
					sameInstance,
					resource.TestCheckResourceAttr(rn, "power_state", "active"),
					testAccCheckInstanceStatus(t, rn, "ACTIVE"),
				),
			},
			{
				Config: testAccInstancePowerStateConfig(imageName, "m1.medium", "paused"),
				Check: resource.ComposeAggregateTestCheckFunc(
					sameInstance,
					resource.TestCheckResourceAttr(rn, "power_state", "paused"),
					testAccCheckInstanceStatus(t, rn, "PAUSED"),
				),
			},
			{
				// paused -> suspended goes through active: unpause, then suspend.
				Config: testAccInstancePowerStateConfig(imageName, "m1.medium", "suspended"),
				Check: resource.ComposeAggregateTestCheckFunc(
					sameInstance,
					resource.TestCheckResourceAttr(rn, "power_state", "suspended"),
					testAccCheckInstanceStatus(t, rn, "SUSPENDED"),
				),
			},
			{
				// Unset means unmanaged: nothing to change, the instance stays suspended.
				Config:   testAccInstancePowerStateConfig(imageName, "m1.medium", ""),
				PlanOnly: true,
			},
			{
				// Import recovers power_state from the status. Instance import
				// does not recover the boot inputs, so they are not compared.
				ResourceName:      rn,
				ImportState:       true,
				ImportStateVerify: true,
				ImportStateVerifyIgnore: []string{
					"image_id", "image_name", "flavor_id", "flavor_name", "key_pair", "user_data",
					"config_drive", "network", "block_device", "scheduler_hints",
				},
			},
		},
	})
}

// testAccInstancePowerStateConfig omits power_state when powerState is "".
func testAccInstancePowerStateConfig(imageName, flavorName, powerState string) string {
	powerLine := ""
	if powerState != "" {
		powerLine = fmt.Sprintf("power_state = %q", powerState)
	}
	return fmt.Sprintf(`
data "pcd_images_image" "boot" {
  name = %q
}

resource "pcd_networking_network" "test" {
  name = "tf-acc-power-net"
}

resource "pcd_networking_subnet" "test" {
  network_id = pcd_networking_network.test.id
  cidr       = "10.118.0.0/24"
}

resource "pcd_compute_instance" "test" {
  name        = "tf-acc-power"
  image_id    = data.pcd_images_image.boot.id
  flavor_name = %q
  %s

  network {
    uuid = pcd_networking_network.test.id
  }

  depends_on = [pcd_networking_subnet.test]
}
`, imageName, flavorName, powerLine)
}

// testAccCheckInstanceStatus reads the instance's Nova status directly, so a
// check does not rest on what the provider wrote to state.
func testAccCheckInstanceStatus(t *testing.T, n, want string) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		rs := s.RootModule().Resources[n]
		if rs == nil {
			return fmt.Errorf("not found in state: %s", n)
		}
		client, err := acctest.LabConfig(t).ComputeV2Client()
		if err != nil {
			return err
		}
		srv, err := servers.Get(context.Background(), client, rs.Primary.ID).Extract()
		if err != nil {
			return err
		}
		if srv.Status != want {
			return fmt.Errorf("instance %s is %s, want %s", rs.Primary.ID, srv.Status, want)
		}
		return nil
	}
}

// TestAccComputeInstance_securityGroupsInPlace swaps one security group for
// another on a running instance, then adds the old one back outside Terraform
// and lets the next apply remove it: both used to replace the instance.
func TestAccComputeInstance_securityGroupsInPlace(t *testing.T) {
	imageName := testAccBootImageName(t)
	const rn = "pcd_compute_instance.test"
	var instanceID string
	sameInstance := resource.TestCheckResourceAttrWith(rn, "id", func(v string) error {
		if v != instanceID {
			return fmt.Errorf("instance was replaced (%s -> %s); security groups must change in place", instanceID, v)
		}
		return nil
	})

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { acctest.PreCheck(t) },
		ProtoV6ProviderFactories: acctest.ProtoV6ProviderFactories,
		CheckDestroy:             testAccCheckInstanceDestroy(t),
		Steps: []resource.TestStep{
			{
				Config: testAccInstanceSecgroupsConfig(imageName, "a"),
				Check: resource.ComposeAggregateTestCheckFunc(
					testAccCaptureID(rn, &instanceID),
					resource.TestCheckTypeSetElemAttr(rn, "security_groups.*", "tf-acc-sg-a"),
					testAccCheckInstancePortSecgroups(t, rn, "pcd_networking_secgroup.a", "pcd_networking_secgroup.b"),
				),
			},
			{
				Config: testAccInstanceSecgroupsConfig(imageName, "b"),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(rn, plancheck.ResourceActionUpdate)},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					sameInstance,
					resource.TestCheckResourceAttr(rn, "security_groups.#", "2"),
					resource.TestCheckTypeSetElemAttr(rn, "security_groups.*", "default"),
					resource.TestCheckTypeSetElemAttr(rn, "security_groups.*", "tf-acc-sg-b"),
					testAccCheckInstancePortSecgroups(t, rn, "pcd_networking_secgroup.b", "pcd_networking_secgroup.a"),
				),
			},
			{
				// Drift: a group added outside Terraform is removed in place.
				PreConfig: func() {
					client, err := acctest.LabConfig(t).ComputeV2Client()
					if err != nil {
						t.Fatal(err)
					}
					if err := secgroups.AddServer(context.Background(), client, instanceID, "tf-acc-sg-a").ExtractErr(); err != nil {
						t.Fatalf("adding tf-acc-sg-a out of band: %v", err)
					}
				},
				Config: testAccInstanceSecgroupsConfig(imageName, "b"),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(rn, plancheck.ResourceActionUpdate)},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					sameInstance,
					resource.TestCheckResourceAttr(rn, "security_groups.#", "2"),
					testAccCheckInstancePortSecgroups(t, rn, "pcd_networking_secgroup.b", "pcd_networking_secgroup.a"),
				),
			},
		},
	})
}

// testAccInstanceSecgroupsConfig attaches "default" plus tf-acc-sg-<which>.
func testAccInstanceSecgroupsConfig(imageName, which string) string {
	return fmt.Sprintf(`
data "pcd_images_image" "boot" {
  name = %q
}

resource "pcd_networking_network" "test" {
  name = "tf-acc-sg-net"
}

resource "pcd_networking_subnet" "test" {
  network_id = pcd_networking_network.test.id
  cidr       = "10.122.0.0/24"
}

resource "pcd_networking_secgroup" "a" {
  name = "tf-acc-sg-a"
}

resource "pcd_networking_secgroup" "b" {
  name = "tf-acc-sg-b"
}

resource "pcd_compute_instance" "test" {
  name            = "tf-acc-sg"
  image_id        = data.pcd_images_image.boot.id
  flavor_name     = "m1.small"
  security_groups = ["default", pcd_networking_secgroup.%s.name]

  network {
    uuid = pcd_networking_network.test.id
  }

  depends_on = [pcd_networking_subnet.test]
}
`, imageName, which)
}

// testAccCheckInstancePortSecgroups checks, through Neutron, that every port
// of the instance carries the group withRN and not the group withoutRN.
func testAccCheckInstancePortSecgroups(t *testing.T, n, withRN, withoutRN string) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		inst, with, without := s.RootModule().Resources[n], s.RootModule().Resources[withRN], s.RootModule().Resources[withoutRN]
		if inst == nil || with == nil || without == nil {
			return fmt.Errorf("not found in state: %s, %s or %s", n, withRN, withoutRN)
		}
		client, err := acctest.LabConfig(t).NetworkV2Client()
		if err != nil {
			return err
		}
		pages, err := ports.List(client, ports.ListOpts{DeviceID: inst.Primary.ID}).AllPages(context.Background())
		if err != nil {
			return err
		}
		all, err := ports.ExtractPorts(pages)
		if err != nil {
			return err
		}
		if len(all) == 0 {
			return fmt.Errorf("instance %s has no ports", inst.Primary.ID)
		}
		for _, p := range all {
			has := map[string]bool{}
			for _, g := range p.SecurityGroups {
				has[g] = true
			}
			if !has[with.Primary.ID] || has[without.Primary.ID] {
				return fmt.Errorf("port %s has security groups %v; want %s and not %s", p.ID, p.SecurityGroups, with.Primary.ID, without.Primary.ID)
			}
		}
		return nil
	}
}
