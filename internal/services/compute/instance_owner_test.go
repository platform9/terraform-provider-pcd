// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package compute_test

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/servers"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"

	"github.com/platform9/terraform-provider-pcd/internal/acctest"
	"github.com/platform9/terraform-provider-pcd/internal/clients"
)

// TestAccComputeInstance_owner boots an instance owned by a new user who
// holds the member role in the provider's project, then omits user_id (the
// plan is empty and Nova keeps that owner), gives the instance back to the
// provider's user in place, and imports it. Changing an owner needs an admin
// token, so it is opt-in: PCD_ACC_ADMIN_COMPUTE=1 and
// PCD_ACC_IMAGE_NAME.
func TestAccComputeInstance_owner(t *testing.T) {
	if os.Getenv("PCD_ACC_ADMIN_COMPUTE") == "" {
		t.Skip("PCD_ACC_ADMIN_COMPUTE not set; skipping the change-owner test")
	}
	imageName := testAccBootImageName(t)
	const rn = "pcd_compute_instance.test"
	var instanceID, owner string
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { acctest.PreCheck(t) },
		ProtoV6ProviderFactories: acctest.ProtoV6ProviderFactories,
		CheckDestroy:             testAccCheckInstanceDestroy(t),
		Steps: []resource.TestStep{
			{
				Config: testAccInstanceOwnerConfig(imageName, "pcd_identity_user.owner.id"),
				Check: resource.ComposeAggregateTestCheckFunc(
					testAccCaptureID(rn, &instanceID),
					resource.TestCheckResourceAttrPair(rn, "user_id", "pcd_identity_user.owner", "id"),
					testAccCheckInstanceOwner(t, rn, "pcd_identity_user.owner", "id"),
				),
			},
			// Omitting user_id leaves the owner alone: the plan is empty and Nova
			// keeps the other owner.
			{
				Config: testAccInstanceOwnerConfig(imageName, "null"),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrPtr(rn, "id", &instanceID),
					resource.TestCheckResourceAttrPair(rn, "user_id", "pcd_identity_user.owner", "id"),
					testAccCheckInstanceOwner(t, rn, "pcd_identity_user.owner", "id"),
				),
			},
			{
				Config: testAccInstanceOwnerConfig(imageName, "data.pcd_identity_auth_scope.me.user_id"),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrPtr(rn, "id", &instanceID),
					resource.TestCheckResourceAttrPair(rn, "user_id", "data.pcd_identity_auth_scope.me", "user_id"),
					testAccCheckInstanceOwner(t, rn, "data.pcd_identity_auth_scope.me", "user_id"),
					testAccCaptureAttr(rn, "user_id", &owner),
				),
			},
			// Import recovers the owner. (Verifying every attribute would trip over
			// the create-only inputs Nova does not report back.)
			{ResourceName: rn, ImportState: true, ImportStateCheck: func(is []*terraform.InstanceState) error {
				if len(is) != 1 || is[0].Attributes["user_id"] != owner {
					return fmt.Errorf("imported user_id = %v, want %s", is, owner)
				}
				return nil
			}},
		},
	})
}

func testAccInstanceOwnerConfig(imageName, owner string) string {
	return fmt.Sprintf(`
data "pcd_identity_auth_scope" "me" {
  name = "tf-acc-owner"
}

data "pcd_identity_role" "member" {
  name = "member"
}

resource "pcd_identity_user" "owner" {
  name = "tf-acc-owner"
}

resource "pcd_identity_role_assignment" "owner" {
  role_id    = data.pcd_identity_role.member.id
  user_id    = pcd_identity_user.owner.id
  project_id = data.pcd_identity_auth_scope.me.project_id
}

data "pcd_images_image" "boot" {
  name = %[1]q
}

resource "pcd_networking_network" "test" {
  name = "tf-acc-owner-net"
}

resource "pcd_networking_subnet" "test" {
  network_id = pcd_networking_network.test.id
  cidr       = "10.142.0.0/24"
}

resource "pcd_compute_instance" "test" {
  name        = "tf-acc-owner"
  image_id    = data.pcd_images_image.boot.id
  flavor_name = "m1.small"
  user_id     = %[2]s

  network {
    uuid = pcd_networking_network.test.id
  }

  depends_on = [pcd_networking_subnet.test, pcd_identity_role_assignment.owner]
}
`, imageName, owner)
}

// testAccCheckInstanceOwner compares Nova's user_id for the instance with an
// attribute of another object in state.
func testAccCheckInstanceOwner(t *testing.T, n, other, attr string) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		rs, want := s.RootModule().Resources[n], s.RootModule().Resources[other]
		if rs == nil || want == nil {
			return fmt.Errorf("not found in state: %s or %s", n, other)
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
		if srv.UserID != want.Primary.Attributes[attr] {
			return fmt.Errorf("instance %s is owned by %s, want %s", rs.Primary.ID, srv.UserID, want.Primary.Attributes[attr])
		}
		return nil
	}
}
