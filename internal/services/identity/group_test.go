// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package identity_test

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/identity/v3/groups"
	"github.com/gophercloud/gophercloud/v2/openstack/identity/v3/users"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"

	"github.com/platform9/terraform-provider-pcd/internal/acctest"
)

// TestAccIdentityGroup_basic creates a group, updates its description, reads it
// back through the data source, and imports it. Keystone needs no backend.
func TestAccIdentityGroup_basic(t *testing.T) {
	const rn = "pcd_identity_group.test"
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { acctest.PreCheck(t) },
		ProtoV6ProviderFactories: acctest.ProtoV6ProviderFactories,
		CheckDestroy:             testAccCheckGroupDestroy(t),
		Steps: []resource.TestStep{
			{
				Config: testAccGroupConfig("tf-acc-group", "first"),
				Check: resource.ComposeAggregateTestCheckFunc(
					testAccCheckGroupExists(t, rn),
					resource.TestCheckResourceAttr(rn, "name", "tf-acc-group"),
					resource.TestCheckResourceAttr(rn, "description", "first"),
					resource.TestCheckResourceAttrSet(rn, "domain_id"),
					resource.TestCheckResourceAttrPair("data.pcd_identity_group.by_name", "id", rn, "id"),
				),
			},
			{
				Config: testAccGroupConfig("tf-acc-group", "second"),
				Check:  resource.TestCheckResourceAttr(rn, "description", "second"),
			},
			{ResourceName: rn, ImportState: true, ImportStateVerify: true},
		},
	})
}

func testAccGroupConfig(name, desc string) string {
	return fmt.Sprintf(`
resource "pcd_identity_group" "test" {
  name        = %q
  description = %q
}

data "pcd_identity_group" "by_name" {
  name = pcd_identity_group.test.name
}
`, name, desc)
}

// TestAccIdentityGroupMembership_basic adds a user to a group and imports the
// membership.
func TestAccIdentityGroupMembership_basic(t *testing.T) {
	const rn = "pcd_identity_group_membership.test"
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { acctest.PreCheck(t) },
		ProtoV6ProviderFactories: acctest.ProtoV6ProviderFactories,
		CheckDestroy:             testAccCheckGroupMembershipDestroy(t),
		Steps: []resource.TestStep{
			{
				Config: testAccGroupMembershipConfig(),
				Check: resource.ComposeAggregateTestCheckFunc(
					testAccCheckGroupMembershipExists(t, rn),
					resource.TestCheckResourceAttrPair(rn, "group_id", "pcd_identity_group.test", "id"),
					resource.TestCheckResourceAttrPair(rn, "user_id", "pcd_identity_user.test", "id"),
				),
			},
			{ResourceName: rn, ImportState: true, ImportStateVerify: true},
		},
	})
}

func testAccGroupMembershipConfig() string {
	return `
resource "pcd_identity_group" "test" {
  name = "tf-acc-gm-group"
}

resource "pcd_identity_user" "test" {
  name     = "tf-acc-gm-user"
  password = "ChangeMe-123!"
}

resource "pcd_identity_group_membership" "test" {
  group_id = pcd_identity_group.test.id
  user_id  = pcd_identity_user.test.id
}
`
}

func testAccCheckGroupExists(t *testing.T, n string) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		rs, ok := s.RootModule().Resources[n]
		if !ok {
			return fmt.Errorf("not found in state: %s", n)
		}
		client, err := acctest.LabConfig(t).IdentityV3Client()
		if err != nil {
			return err
		}
		if _, err := groups.Get(context.Background(), client, rs.Primary.ID).Extract(); err != nil {
			return fmt.Errorf("group %s not found via API: %w", rs.Primary.ID, err)
		}
		return nil
	}
}

func testAccCheckGroupDestroy(t *testing.T) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		client, err := acctest.LabConfig(t).IdentityV3Client()
		if err != nil {
			return err
		}
		for _, rs := range s.RootModule().Resources {
			if rs.Type != "pcd_identity_group" {
				continue
			}
			_, err := groups.Get(context.Background(), client, rs.Primary.ID).Extract()
			if err == nil {
				return fmt.Errorf("group %s still exists", rs.Primary.ID)
			}
			if !gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
				return fmt.Errorf("unexpected error checking group %s: %w", rs.Primary.ID, err)
			}
		}
		return nil
	}
}

func testAccCheckGroupMembershipExists(t *testing.T, n string) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		rs, ok := s.RootModule().Resources[n]
		if !ok {
			return fmt.Errorf("not found in state: %s", n)
		}
		client, err := acctest.LabConfig(t).IdentityV3Client()
		if err != nil {
			return err
		}
		member, err := users.IsMemberOfGroup(context.Background(), client, rs.Primary.Attributes["group_id"], rs.Primary.Attributes["user_id"]).Extract()
		if err != nil {
			return err
		}
		if !member {
			return fmt.Errorf("membership %s not present via API", rs.Primary.ID)
		}
		return nil
	}
}

func testAccCheckGroupMembershipDestroy(t *testing.T) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		client, err := acctest.LabConfig(t).IdentityV3Client()
		if err != nil {
			return err
		}
		for _, rs := range s.RootModule().Resources {
			if rs.Type != "pcd_identity_group_membership" {
				continue
			}
			member, err := users.IsMemberOfGroup(context.Background(), client, rs.Primary.Attributes["group_id"], rs.Primary.Attributes["user_id"]).Extract()
			if err != nil {
				if gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
					continue
				}
				return err
			}
			if member {
				return fmt.Errorf("membership %s still exists", rs.Primary.ID)
			}
		}
		return nil
	}
}
