// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package compute_test

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"testing"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/servers"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
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
