// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package compute_test

import (
	"fmt"
	"os"
	"regexp"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"

	"github.com/platform9/terraform-provider-pcd/internal/acctest"
)

// TestAccComputeInstanceDataSource_basic boots an instance and reads it back
// by ID and by name: both must agree with the resource, and the network list
// must carry the port Nova created, which upstream's data source left empty.
// It needs PCD_ACC_IMAGE_NAME, like the other boot tests.
func TestAccComputeInstanceDataSource_basic(t *testing.T) {
	imageName := testAccBootImageName(t)
	const rn = "pcd_compute_instance.test"
	const byID = "data.pcd_compute_instance.by_id"
	const byName = "data.pcd_compute_instance.by_name"

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { acctest.PreCheck(t) },
		ProtoV6ProviderFactories: acctest.ProtoV6ProviderFactories,
		CheckDestroy:             testAccCheckInstanceDestroy(t),
		Steps: []resource.TestStep{
			{
				Config: testAccInstanceDataSourceConfig(imageName),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrPair(byID, "id", rn, "id"),
					resource.TestCheckResourceAttrPair(byName, "id", rn, "id"),
					resource.TestCheckResourceAttrPair(byID, "name", rn, "name"),
					resource.TestCheckResourceAttrPair(byID, "flavor_id", rn, "flavor_id"),
					resource.TestCheckResourceAttrPair(byID, "image_id", rn, "image_id"),
					resource.TestCheckResourceAttrPair(byID, "access_ip_v4", rn, "access_ip_v4"),
					resource.TestCheckResourceAttr(byID, "status", "ACTIVE"),
					resource.TestCheckResourceAttr(byID, "network.#", "1"),
					resource.TestCheckResourceAttrPair(byID, "network.0.uuid", "pcd_networking_network.test", "id"),
					resource.TestCheckResourceAttrSet(byID, "network.0.port"),
					resource.TestCheckResourceAttrSet(byID, "network.0.mac"),
					resource.TestCheckResourceAttrSet(byID, "network.0.fixed_ip_v4"),
					resource.TestCheckResourceAttrSet(byID, "project_id"),
					resource.TestCheckResourceAttrSet(byID, "flavor_name"),
					resource.TestCheckResourceAttrSet(byID, "created"),
				),
			},
			{
				Config:      testAccInstanceDataSourceConfig(imageName) + testAccInstanceDataSourceMissingConfig,
				ExpectError: regexp.MustCompile(`No instance found`),
			},
		},
	})
}

const testAccInstanceDataSourceMissingConfig = `
data "pcd_compute_instance" "missing" {
  name = "tf-acc-no-such-instance"
}
`

// TestAccComputeInstanceDataSource_existing reads an instance this test does
// not manage, named by PCD_ACC_EXISTING_INSTANCE_NAME. It boots nothing and
// changes nothing, so it can run against an instance created in the PCD UI.
func TestAccComputeInstanceDataSource_existing(t *testing.T) {
	name := os.Getenv("PCD_ACC_EXISTING_INSTANCE_NAME")
	if name == "" {
		t.Skip("PCD_ACC_EXISTING_INSTANCE_NAME not set; skipping the lookup of an instance Terraform does not manage")
	}
	const ds = "data.pcd_compute_instance.existing"
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { acctest.PreCheck(t) },
		ProtoV6ProviderFactories: acctest.ProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: fmt.Sprintf(`
data "pcd_compute_instance" "existing" {
  name = %q
}

data "pcd_compute_instance" "again" {
  instance_id = data.pcd_compute_instance.existing.id
}
`, name),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(ds, "name", name),
					resource.TestCheckResourceAttrSet(ds, "id"),
					resource.TestCheckResourceAttrSet(ds, "status"),
					resource.TestCheckResourceAttrSet(ds, "network.0.port"),
					resource.TestCheckResourceAttrPair("data.pcd_compute_instance.again", "network.0.port", ds, "network.0.port"),
					resource.TestCheckResourceAttrPair("data.pcd_compute_instance.again", "access_ip_v4", ds, "access_ip_v4"),
				),
			},
		},
	})
}

func testAccInstanceDataSourceConfig(imageName string) string {
	return fmt.Sprintf(`
data "pcd_images_image" "boot" {
  name = %q
}

resource "pcd_networking_network" "test" {
  name = "tf-acc-inst-ds-net"
}

resource "pcd_networking_subnet" "test" {
  network_id = pcd_networking_network.test.id
  cidr       = "10.139.0.0/24"
}

resource "pcd_compute_instance" "test" {
  name        = "tf-acc-inst-ds"
  image_id    = data.pcd_images_image.boot.id
  flavor_name = "m1.small"

  network {
    uuid = pcd_networking_network.test.id
  }

  depends_on = [pcd_networking_subnet.test]
}

data "pcd_compute_instance" "by_id" {
  instance_id = pcd_compute_instance.test.id
}

data "pcd_compute_instance" "by_name" {
  name = pcd_compute_instance.test.name
}
`, imageName)
}
