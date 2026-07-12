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
	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/attachinterfaces"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"

	"github.com/platform9/terraform-provider-pcd/internal/acctest"
)

// TestAccComputeInterfaceAttach_basic boots an instance and attaches a second
// network interface to it by network_id. Requires a bootable instance.
func TestAccComputeInterfaceAttach_basic(t *testing.T) {
	imageURL := os.Getenv("PCD_ACC_IMAGE_URL")
	if imageURL == "" {
		imageURL = defaultTestImageURL
	}
	const rn = "pcd_compute_interface_attach.test"

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { acctest.PreCheck(t) },
		ProtoV6ProviderFactories: acctest.ProtoV6ProviderFactories,
		CheckDestroy:             testAccCheckInterfaceAttachDestroy(t),
		Steps: []resource.TestStep{
			{
				Config: testAccInterfaceAttachConfig(imageURL),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrSet(rn, "port_id"),
					resource.TestCheckResourceAttrSet(rn, "mac"),
					resource.TestCheckResourceAttrSet(rn, "port_state"),
					resource.TestCheckResourceAttrPair(rn, "network_id", "pcd_networking_network.attach", "id"),
				),
			},
			{
				ResourceName:      rn,
				ImportState:       true,
				ImportStateVerify: true,
				ImportStateIdFunc: func(s *terraform.State) (string, error) {
					rs := s.RootModule().Resources[rn]
					if rs == nil {
						return "", fmt.Errorf("not found in state: %s", rn)
					}
					return rs.Primary.ID, nil
				},
			},
		},
	})
}

func testAccInterfaceAttachConfig(imageURL string) string {
	return fmt.Sprintf(`
resource "pcd_networking_network" "boot" {
  name = "tf-acc-ia-boot-net"
}

resource "pcd_networking_subnet" "boot" {
  network_id = pcd_networking_network.boot.id
  cidr       = "10.114.0.0/24"
}

resource "pcd_networking_network" "attach" {
  name = "tf-acc-ia-attach-net"
}

resource "pcd_networking_subnet" "attach" {
  network_id = pcd_networking_network.attach.id
  cidr       = "10.115.0.0/24"
}

resource "pcd_images_image" "test" {
  name             = "tf-acc-ia-img"
  container_format = "bare"
  disk_format      = "qcow2"
  image_source_url = %q
}

resource "pcd_compute_instance" "test" {
  name        = "tf-acc-ia-instance"
  image_id    = pcd_images_image.test.id
  flavor_name = "m1.tiny"

  network {
    uuid = pcd_networking_network.boot.id
  }

  depends_on = [pcd_networking_subnet.boot]
}

resource "pcd_compute_interface_attach" "test" {
  instance_id = pcd_compute_instance.test.id
  network_id  = pcd_networking_network.attach.id

  depends_on = [pcd_networking_subnet.attach]
}
`, imageURL)
}

func testAccCheckInterfaceAttachDestroy(t *testing.T) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		client, err := acctest.LabConfig(t).ComputeV2Client()
		if err != nil {
			return err
		}
		for _, rs := range s.RootModule().Resources {
			if rs.Type != "pcd_compute_interface_attach" {
				continue
			}
			instanceID := rs.Primary.Attributes["instance_id"]
			portID := rs.Primary.Attributes["port_id"]
			_, err := attachinterfaces.Get(context.Background(), client, instanceID, portID).Extract()
			if err == nil {
				return fmt.Errorf("interface %s on instance %s still attached", portID, instanceID)
			}
			if !gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
				return err
			}
		}
		return nil
	}
}
