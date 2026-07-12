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

// defaultTestImageURL is a small bootable qcow2 (CirrOS) fetched by Glance via
// web-download, which bypasses the ingress upload-size cap. Override with
// PCD_ACC_IMAGE_URL.
const defaultTestImageURL = "https://download.cirros-cloud.net/0.6.2/cirros-0.6.2-x86_64-disk.img"

// TestAccComputeInstance_basic boots a real VM through Terraform end to end:
// network + subnet + Glance image (web-download) + keypair + instance on m1.tiny.
func TestAccComputeInstance_basic(t *testing.T) {
	imageURL := os.Getenv("PCD_ACC_IMAGE_URL")
	if imageURL == "" {
		imageURL = defaultTestImageURL
	}
	const rn = "pcd_compute_instance.test"

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { acctest.PreCheck(t) },
		ProtoV6ProviderFactories: acctest.ProtoV6ProviderFactories,
		CheckDestroy:             testAccCheckInstanceDestroy(t),
		Steps: []resource.TestStep{
			{
				Config: testAccInstanceConfig(imageURL, "tf-acc-instance"),
				Check: resource.ComposeAggregateTestCheckFunc(
					testAccCheckInstanceExists(t, rn),
					resource.TestCheckResourceAttr(rn, "name", "tf-acc-instance"),
					resource.TestCheckResourceAttr(rn, "status", "ACTIVE"),
					resource.TestCheckResourceAttrSet(rn, "access_ip_v4"),
					resource.TestCheckResourceAttrSet(rn, "flavor_id"),
				),
			},
			{
				Config: testAccInstanceConfig(imageURL, "tf-acc-instance-renamed"),
				Check:  resource.TestCheckResourceAttr(rn, "name", "tf-acc-instance-renamed"),
			},
		},
	})
}

func testAccInstanceConfig(imageURL, name string) string {
	return fmt.Sprintf(`
resource "pcd_networking_network" "test" {
  name = "tf-acc-inst-net"
}

resource "pcd_networking_subnet" "test" {
  network_id = pcd_networking_network.test.id
  cidr       = "10.103.0.0/24"
}

resource "pcd_images_image" "test" {
  name             = "tf-acc-inst-img"
  container_format = "bare"
  disk_format      = "qcow2"
  image_source_url = %q
}

resource "pcd_compute_keypair" "test" {
  name = "tf-acc-inst-key"
}

resource "pcd_compute_instance" "test" {
  name        = %q
  image_id    = pcd_images_image.test.id
  flavor_name = "m1.tiny"
  key_pair    = pcd_compute_keypair.test.name

  network {
    uuid = pcd_networking_network.test.id
  }

  depends_on = [pcd_networking_subnet.test]
}
`, imageURL, name)
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
