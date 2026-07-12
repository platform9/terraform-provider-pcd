// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package identity_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/identity/v3/roles"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"

	"github.com/platform9/terraform-provider-pcd/internal/acctest"
)

func TestAccIdentityRoleAssignmentResource_basic(t *testing.T) {
	const resourceName = "pcd_identity_role_assignment.test"

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { acctest.PreCheck(t) },
		ProtoV6ProviderFactories: acctest.ProtoV6ProviderFactories,
		CheckDestroy: resource.ComposeAggregateTestCheckFunc(
			testAccCheckRoleAssignmentDestroy(t),
			testAccCheckProjectDestroy(t),
			testAccCheckUserDestroy(t),
			testAccCheckRoleDestroy(t),
		),
		Steps: []resource.TestStep{
			{
				Config: testAccRoleAssignmentConfig,
				Check: resource.ComposeAggregateTestCheckFunc(
					testAccCheckRoleAssignmentExists(t, resourceName),
					resource.TestCheckResourceAttrSet(resourceName, "user_id"),
					resource.TestCheckResourceAttrSet(resourceName, "project_id"),
					resource.TestCheckResourceAttrSet(resourceName, "role_id"),
				),
			},
			{
				ResourceName:      resourceName,
				ImportState:       true,
				ImportStateVerify: true,
			},
		},
	})
}

const testAccRoleAssignmentConfig = `
resource "pcd_identity_project" "test" {
  name = "tf-acc-ra-project"
}

resource "pcd_identity_user" "test" {
  name     = "tf-acc-ra-user"
  password = "Tf-Acc-Passw0rd!"
}

resource "pcd_identity_role" "test" {
  name = "tf-acc-ra-role"
}

resource "pcd_identity_role_assignment" "test" {
  user_id    = pcd_identity_user.test.id
  project_id = pcd_identity_project.test.id
  role_id    = pcd_identity_role.test.id
}
`

func testAccCheckRoleAssignmentExists(t *testing.T, n string) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		rs, ok := s.RootModule().Resources[n]
		if !ok {
			return fmt.Errorf("not found in state: %s", n)
		}
		client, err := acctest.LabConfig(t).IdentityV3Client()
		if err != nil {
			return err
		}
		found, err := roleAssignmentPresent(client, rs.Primary.ID)
		if err != nil {
			return err
		}
		if !found {
			return fmt.Errorf("role assignment %s not found via API", rs.Primary.ID)
		}
		return nil
	}
}

func testAccCheckRoleAssignmentDestroy(t *testing.T) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		client, err := acctest.LabConfig(t).IdentityV3Client()
		if err != nil {
			return err
		}
		for _, rs := range s.RootModule().Resources {
			if rs.Type != "pcd_identity_role_assignment" {
				continue
			}
			found, err := roleAssignmentPresent(client, rs.Primary.ID)
			if err != nil {
				return err
			}
			if found {
				return fmt.Errorf("role assignment %s still exists", rs.Primary.ID)
			}
		}
		return nil
	}
}

// roleAssignmentPresent parses a composite id (domain/project/group/user/role)
// and reports whether that assignment currently exists.
func roleAssignmentPresent(client *gophercloud.ServiceClient, id string) (bool, error) {
	parts := strings.SplitN(id, "/", 5)
	if len(parts) != 5 {
		return false, fmt.Errorf("bad role assignment id: %q", id)
	}
	pages, err := roles.ListAssignments(client, roles.ListAssignmentsOpts{
		ScopeDomainID:  parts[0],
		ScopeProjectID: parts[1],
		GroupID:        parts[2],
		UserID:         parts[3],
		RoleID:         parts[4],
	}).AllPages(context.Background())
	if err != nil {
		return false, err
	}
	all, err := roles.ExtractRoleAssignments(pages)
	if err != nil {
		return false, err
	}
	return len(all) > 0, nil
}
