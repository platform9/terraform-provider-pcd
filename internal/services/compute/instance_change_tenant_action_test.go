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
	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/ports"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
	"github.com/hashicorp/terraform-plugin-testing/tfversion"

	"github.com/platform9/terraform-provider-pcd/internal/acctest"
	"github.com/platform9/terraform-provider-pcd/internal/clients"
)

// The change-tenant tests boot their own instance and need an admin token, so
// they are opt-in: PCD_ACC_ADMIN_COMPUTE=1 and PCD_ACC_IMAGE_NAME. The move
// test also needs PCD_ACC_SECOND_PROJECT_ID: a project in which the provider's
// user may create instances, with instance quota left. The instance boots on a
// port with no security groups, on a shared network, with a public image and
// flavor and no floating IP, so the target project passes every check Nova
// makes.

// TestAccComputeInstanceChangeTenant_moves moves the instance to the second
// project and back, checking where Nova and Neutron report the instance and
// its port each time, and that the instance was never replaced.
func TestAccComputeInstanceChangeTenant_moves(t *testing.T) {
	image := testAccChangeTenantImage(t)
	target := os.Getenv("PCD_ACC_SECOND_PROJECT_ID")
	if target == "" {
		t.Skip("PCD_ACC_SECOND_PROJECT_ID not set; moving an instance needs a second project")
	}
	const rn = "pcd_compute_instance.test"
	var id string
	resource.Test(t, resource.TestCase{
		TerraformVersionChecks:   []tfversion.TerraformVersionCheck{tfversion.SkipBelow(tfversion.Version1_14_0)},
		PreCheck:                 func() { acctest.PreCheck(t) },
		ProtoV6ProviderFactories: acctest.ProtoV6ProviderFactories,
		CheckDestroy:             testAccCheckInstanceDestroy(t),
		Steps: []resource.TestStep{
			{
				Config: testAccChangeTenantConfig(image, "10.144.0.0/24", fmt.Sprintf("%q", target), "1"),
				Check:  resource.ComposeAggregateTestCheckFunc(testAccCaptureID(rn, &id), testAccCheckMovedTo(t, target)),
			},
			{
				Config: testAccChangeTenantConfig(image, "10.144.0.0/24", "data.pcd_identity_auth_scope.me.project_id", "2"),
				Check:  resource.ComposeAggregateTestCheckFunc(resource.TestCheckResourceAttrPtr(rn, "id", &id), testAccCheckMovedTo(t, "")),
			},
		},
	})
}

// TestAccComputeInstanceChangeTenant_refused asks Nova to move the instance to
// a project that does not exist: Nova refuses before it moves anything, and
// the action fails with Nova's message. The second step targets the
// instance's own project, so the action, should it run again, warns and sends
// nothing; its check confirms that the instance and its port are still in
// their project.
func TestAccComputeInstanceChangeTenant_refused(t *testing.T) {
	image := testAccChangeTenantImage(t)
	resource.Test(t, resource.TestCase{
		TerraformVersionChecks:   []tfversion.TerraformVersionCheck{tfversion.SkipBelow(tfversion.Version1_14_0)},
		PreCheck:                 func() { acctest.PreCheck(t) },
		ProtoV6ProviderFactories: acctest.ProtoV6ProviderFactories,
		CheckDestroy:             testAccCheckInstanceDestroy(t),
		Steps: []resource.TestStep{
			{
				Config:      testAccChangeTenantConfig(image, "10.145.0.0/24", `"0000000000000000000000000000dead"`, "1"),
				ExpectError: regexp.MustCompile(`Instance not moved`),
			},
			{
				Config: testAccChangeTenantConfig(image, "10.145.0.0/24", "data.pcd_identity_auth_scope.me.project_id", "1"),
				Check:  testAccCheckMovedTo(t, ""),
			},
		},
	})
}

func testAccChangeTenantImage(t *testing.T) string {
	t.Helper()
	if os.Getenv("PCD_ACC_ADMIN_COMPUTE") == "" {
		t.Skip("PCD_ACC_ADMIN_COMPUTE not set; skipping a change-tenant test")
	}
	return testAccBootImageName(t)
}

// testAccChangeTenantConfig boots tf-acc-tenant and runs the change-tenant
// action after it is created and whenever generation changes. project is an
// HCL expression for the target project.
func testAccChangeTenantConfig(image, cidr, project, generation string) string {
	return fmt.Sprintf(`
terraform {
  required_version = ">= 1.14"
}

data "pcd_identity_auth_scope" "me" {
  name = "tf-acc-tenant"
}

data "pcd_images_image" "boot" {
  name = %[1]q
}

resource "pcd_networking_network" "test" {
  name   = "tf-acc-tenant-net"
  shared = true
}

resource "pcd_networking_subnet" "test" {
  network_id = pcd_networking_network.test.id
  cidr       = %[2]q
}

resource "pcd_networking_port" "test" {
  name               = "tf-acc-tenant-port"
  network_id         = pcd_networking_network.test.id
  security_group_ids = []

  depends_on = [pcd_networking_subnet.test]
}

resource "pcd_compute_instance" "test" {
  name        = "tf-acc-tenant"
  image_id    = data.pcd_images_image.boot.id
  flavor_name = "m1.small"

  network {
    port = pcd_networking_port.test.id
  }
}

action "pcd_compute_instance_change_tenant" "test" {
  config {
    instance_id = pcd_compute_instance.test.id
    project_id  = %[3]s
  }
}

resource "terraform_data" "move" {
  input = %[4]q

  lifecycle {
    action_trigger {
      events  = [after_create, after_update]
      actions = [action.pcd_compute_instance_change_tenant.test]
    }
  }

  depends_on = [pcd_compute_instance.test]
}
`, image, cidr, project, generation)
}

// testAccCheckMovedTo checks that Nova reports pcd_compute_instance.test, and
// Neutron reports pcd_networking_port.test, in project want, or, when want is
// empty, in the project the provider's token is scoped to.
func testAccCheckMovedTo(t *testing.T, want string) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		project := want
		if project == "" {
			project = s.RootModule().Resources["data.pcd_identity_auth_scope.me"].Primary.Attributes["project_id"]
		}
		vm, port := s.RootModule().Resources["pcd_compute_instance.test"], s.RootModule().Resources["pcd_networking_port.test"]
		if vm == nil || port == nil || project == "" {
			return fmt.Errorf("instance, port or project missing from state")
		}
		cfg := acctest.LabConfig(t)
		compute, err := cfg.ComputeV2Client()
		if err != nil {
			return err
		}
		srv, err := clients.RequireObject(servers.Get(context.Background(), compute, vm.Primary.ID).Extract())
		if err == nil && srv.ID == "" {
			err = clients.ErrNoObject
		}
		if err != nil {
			return err
		}
		if srv.TenantID != project {
			return fmt.Errorf("instance %s is in project %s, want %s", srv.ID, srv.TenantID, project)
		}
		network, err := cfg.NetworkV2Client()
		if err != nil {
			return err
		}
		p, err := clients.RequireObject(ports.Get(context.Background(), network, port.Primary.ID).Extract())
		if err == nil && p.ID == "" {
			err = clients.ErrNoObject
		}
		if err != nil {
			return err
		}
		if p.ProjectID != project {
			return fmt.Errorf("port %s is in project %s, want %s; Nova moves an instance's ports with it", p.ID, p.ProjectID, project)
		}
		return nil
	}
}
