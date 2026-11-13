// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package identity_test

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"testing"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/identity/v3/osinherit"
	"github.com/gophercloud/gophercloud/v2/openstack/identity/v3/roles"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"

	"github.com/platform9/terraform-provider-pcd/internal/acctest"
)

// TestAccIdentityInheritRoleAssignment_user grants a throwaway role on a new
// domain to a new user in it, as the PCD UI's Domain-Level Access does, checks
// that a project in the domain inherits it, imports the grant, checks that
// pcd_identity_role_assignment no longer mistakes the inherited grant for a
// direct one, and then removes only the grant and checks that Keystone revoked
// it. Every object it touches is its own.
func TestAccIdentityInheritRoleAssignment_user(t *testing.T) {
	const rn = "pcd_identity_inherit_role_assignment.test"
	var grantID string
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { acctest.PreCheck(t) },
		ProtoV6ProviderFactories: acctest.ProtoV6ProviderFactories,
		CheckDestroy: resource.ComposeAggregateTestCheckFunc(
			testAccCheckInheritRoleAssignmentDestroy(t),
			testAccCheckProjectDestroy(t),
			testAccCheckUserDestroy(t),
			testAccCheckRoleDestroy(t),
			testAccCheckDomainDestroy(t),
		),
		Steps: []resource.TestStep{
			{
				Config: testAccInheritRoleAssignmentUserConfig,
				Check: resource.ComposeAggregateTestCheckFunc(
					testAccCheckInheritRoleAssignmentExists(t, rn),
					testAccCaptureInheritedGrantID(rn, &grantID),
					testAccCheckRoleEffectiveOnProject(t, rn, "pcd_identity_project.test"),
					resource.TestCheckResourceAttrPair(rn, "domain_id", "pcd_identity_domain.test", "id"),
					resource.TestCheckNoResourceAttr(rn, "project_id"),
					resource.TestCheckNoResourceAttr(rn, "group_id"),
				),
			},
			{ResourceName: rn, ImportState: true, ImportStateVerify: true},
			{
				// The same tuple as a direct grant does not exist, so importing it
				// into pcd_identity_role_assignment must fail.
				Config: testAccInheritRoleAssignmentUserConfig + `
resource "pcd_identity_role_assignment" "probe" {
  domain_id = pcd_identity_domain.test.id
  user_id   = pcd_identity_user.test.id
  role_id   = pcd_identity_role.test.id
}
`,
				ResourceName: "pcd_identity_role_assignment.probe",
				ImportState:  true,
				ImportStateIdFunc: func(s *terraform.State) (string, error) {
					return s.RootModule().Resources[rn].Primary.ID, nil
				},
				ExpectError: regexp.MustCompile(`Cannot import non-existent remote object`),
			},
			{
				// Removing only the grant revokes it; the domain, user and
				// project stay.
				Config: testAccInheritRoleAssignmentUserObjects,
				Check:  testAccCheckInheritedGrantRevoked(t, &grantID, "pcd_identity_project.test"),
			},
		},
	})
}

// TestAccIdentityInheritRoleAssignment_group grants the role on a new domain to
// a group in it, the form the PCD UI's SSO group mappings use, imports it, and
// then removes only the grant and checks that Keystone revoked it.
func TestAccIdentityInheritRoleAssignment_group(t *testing.T) {
	const rn = "pcd_identity_inherit_role_assignment.test"
	var grantID string
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { acctest.PreCheck(t) },
		ProtoV6ProviderFactories: acctest.ProtoV6ProviderFactories,
		CheckDestroy: resource.ComposeAggregateTestCheckFunc(
			testAccCheckInheritRoleAssignmentDestroy(t),
			testAccCheckGroupDestroy(t),
			testAccCheckRoleDestroy(t),
			testAccCheckDomainDestroy(t),
		),
		Steps: []resource.TestStep{
			{
				Config: testAccInheritRoleAssignmentGroupConfig,
				Check: resource.ComposeAggregateTestCheckFunc(
					testAccCheckInheritRoleAssignmentExists(t, rn),
					testAccCaptureInheritedGrantID(rn, &grantID),
				),
			},
			{ResourceName: rn, ImportState: true, ImportStateVerify: true},
			{
				// Removing only the grant revokes it; the domain and group stay.
				Config: testAccInheritRoleAssignmentGroupObjects,
				Check:  testAccCheckInheritedGrantRevoked(t, &grantID, ""),
			},
		},
	})
}

const testAccInheritRoleAssignmentUserObjects = `
resource "pcd_identity_domain" "test" {
  name = "tf-acc-inherit-domain"
}

resource "pcd_identity_role" "test" {
  name = "tf-acc-inherit-role"
}

resource "pcd_identity_user" "test" {
  name      = "tf-acc-inherit-user"
  password  = "Tf-Acc-Passw0rd!"
  domain_id = pcd_identity_domain.test.id
}

resource "pcd_identity_project" "test" {
  name      = "tf-acc-inherit-project"
  domain_id = pcd_identity_domain.test.id
}
`

const testAccInheritRoleAssignmentUserConfig = testAccInheritRoleAssignmentUserObjects + `
resource "pcd_identity_inherit_role_assignment" "test" {
  domain_id = pcd_identity_domain.test.id
  user_id   = pcd_identity_user.test.id
  role_id   = pcd_identity_role.test.id
}
`

const testAccInheritRoleAssignmentGroupObjects = `
resource "pcd_identity_domain" "test" {
  name = "tf-acc-inherit-group-domain"
}

resource "pcd_identity_role" "test" {
  name = "tf-acc-inherit-group-role"
}

resource "pcd_identity_group" "test" {
  name      = "tf-acc-inherit-group"
  domain_id = pcd_identity_domain.test.id
}
`

const testAccInheritRoleAssignmentGroupConfig = testAccInheritRoleAssignmentGroupObjects + `
resource "pcd_identity_inherit_role_assignment" "test" {
  domain_id = pcd_identity_domain.test.id
  group_id  = pcd_identity_group.test.id
  role_id   = pcd_identity_role.test.id
}
`

// inheritedGrantPresent asks Keystone (HEAD on the OS-INHERIT path) whether
// the grant with this composite ID exists.
func inheritedGrantPresent(client *gophercloud.ServiceClient, id string) (bool, error) {
	parts := strings.SplitN(id, "/", 5)
	if len(parts) != 5 {
		return false, fmt.Errorf("bad inherited role assignment id: %q", id)
	}
	err := osinherit.Validate(context.Background(), client, parts[4], osinherit.ValidateOpts{
		DomainID: parts[0], ProjectID: parts[1], GroupID: parts[2], UserID: parts[3],
	}).ExtractErr()
	if err == nil {
		return true, nil
	}
	if gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
		return false, nil
	}
	return false, err
}

func testAccCheckInheritRoleAssignmentExists(t *testing.T, n string) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		rs, ok := s.RootModule().Resources[n]
		if !ok {
			return fmt.Errorf("not found in state: %s", n)
		}
		client, err := acctest.LabConfig(t).IdentityV3Client()
		if err != nil {
			return err
		}
		found, err := inheritedGrantPresent(client, rs.Primary.ID)
		if err != nil {
			return err
		}
		if !found {
			return fmt.Errorf("inherited role assignment %s not found via API", rs.Primary.ID)
		}
		return nil
	}
}

// testAccCaptureInheritedGrantID stores the grant's ID, so that a later step
// can check the grant after it has left state.
func testAccCaptureInheritedGrantID(n string, id *string) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		rs, ok := s.RootModule().Resources[n]
		if !ok {
			return fmt.Errorf("not found in state: %s", n)
		}
		*id = rs.Primary.ID
		return nil
	}
}

// userRoleEffectiveOnProject reports whether the user holds the role on the
// project, directly or through an inherited grant (an effective assignment).
func userRoleEffectiveOnProject(client *gophercloud.ServiceClient, userID, projectID, roleID string) (bool, error) {
	if userID == "" || projectID == "" || roleID == "" {
		return false, fmt.Errorf("user %q, project %q and role %q must all be set", userID, projectID, roleID)
	}
	effective := true
	pages, err := roles.ListAssignments(client, roles.ListAssignmentsOpts{
		UserID: userID, ScopeProjectID: projectID, Effective: &effective,
	}).AllPages(context.Background())
	if err != nil {
		return false, err
	}
	all, err := roles.ExtractRoleAssignments(pages)
	if err != nil {
		return false, err
	}
	for _, ra := range all {
		if ra.Role.ID == roleID {
			return true, nil
		}
	}
	return false, nil
}

// testAccCheckRoleEffectiveOnProject checks that the user holds the role on the
// project through the domain grant (an effective assignment).
func testAccCheckRoleEffectiveOnProject(t *testing.T, assignment, project string) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		ars, ok := s.RootModule().Resources[assignment]
		if !ok {
			return fmt.Errorf("not found in state: %s", assignment)
		}
		prs, ok := s.RootModule().Resources[project]
		if !ok {
			return fmt.Errorf("not found in state: %s", project)
		}
		a, p := ars.Primary.Attributes, prs.Primary.ID
		client, err := acctest.LabConfig(t).IdentityV3Client()
		if err != nil {
			return err
		}
		found, err := userRoleEffectiveOnProject(client, a["user_id"], p, a["role_id"])
		if err != nil {
			return err
		}
		if !found {
			return fmt.Errorf("role %s is not effective for user %s on project %s", a["role_id"], a["user_id"], p)
		}
		return nil
	}
}

// testAccCheckInheritedGrantRevoked checks that the grant with this ID, which
// a step removed from the configuration, is gone from Keystone (the OS-INHERIT
// HEAD answers 404). With a project, it also checks that the project still
// exists and that the user no longer holds the role on it.
func testAccCheckInheritedGrantRevoked(t *testing.T, id *string, project string) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		client, err := acctest.LabConfig(t).IdentityV3Client()
		if err != nil {
			return err
		}
		found, err := inheritedGrantPresent(client, *id)
		if err != nil {
			return err
		}
		if found {
			return fmt.Errorf("inherited role assignment %s still exists after it left the configuration", *id)
		}
		if project == "" {
			return nil
		}
		if err := testAccCheckProjectExists(t, project)(s); err != nil {
			return err
		}
		parts := strings.SplitN(*id, "/", 5)
		p := s.RootModule().Resources[project].Primary.ID
		effective, err := userRoleEffectiveOnProject(client, parts[3], p, parts[4])
		if err != nil {
			return err
		}
		if effective {
			return fmt.Errorf("role %s is still effective for user %s on project %s after the grant was removed", parts[4], parts[3], p)
		}
		return nil
	}
}

func testAccCheckInheritRoleAssignmentDestroy(t *testing.T) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		client, err := acctest.LabConfig(t).IdentityV3Client()
		if err != nil {
			return err
		}
		for _, rs := range s.RootModule().Resources {
			if rs.Type != "pcd_identity_inherit_role_assignment" {
				continue
			}
			found, err := inheritedGrantPresent(client, rs.Primary.ID)
			if err != nil {
				return err
			}
			if found {
				return fmt.Errorf("inherited role assignment %s still exists", rs.Primary.ID)
			}
		}
		return nil
	}
}
