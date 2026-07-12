// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package identity_test

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/identity/v3/roles"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"

	"github.com/platform9/terraform-provider-pcd/internal/acctest"
)

func TestAccIdentityRoleResource_basic(t *testing.T) {
	const resourceName = "pcd_identity_role.test"
	name := "tf-acc-identity-role"

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { acctest.PreCheck(t) },
		ProtoV6ProviderFactories: acctest.ProtoV6ProviderFactories,
		CheckDestroy:             testAccCheckRoleDestroy(t),
		Steps: []resource.TestStep{
			{
				Config: fmt.Sprintf("resource \"pcd_identity_role\" \"test\" {\n  name = %q\n}\n", name),
				Check: resource.ComposeAggregateTestCheckFunc(
					testAccCheckRoleExists(t, resourceName),
					resource.TestCheckResourceAttr(resourceName, "name", name),
					resource.TestCheckResourceAttrSet(resourceName, "id"),
				),
			},
			{
				Config: fmt.Sprintf("resource \"pcd_identity_role\" \"test\" {\n  name = %q\n}\n", name+"-updated"),
				Check:  resource.TestCheckResourceAttr(resourceName, "name", name+"-updated"),
			},
			{
				ResourceName:      resourceName,
				ImportState:       true,
				ImportStateVerify: true,
			},
		},
	})
}

func testAccCheckRoleExists(t *testing.T, n string) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		rs, ok := s.RootModule().Resources[n]
		if !ok {
			return fmt.Errorf("not found in state: %s", n)
		}
		client, err := acctest.LabConfig(t).IdentityV3Client()
		if err != nil {
			return err
		}
		if _, err := roles.Get(context.Background(), client, rs.Primary.ID).Extract(); err != nil {
			return fmt.Errorf("role %s not found via API: %w", rs.Primary.ID, err)
		}
		return nil
	}
}

func testAccCheckRoleDestroy(t *testing.T) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		client, err := acctest.LabConfig(t).IdentityV3Client()
		if err != nil {
			return err
		}
		for _, rs := range s.RootModule().Resources {
			if rs.Type != "pcd_identity_role" {
				continue
			}
			_, err := roles.Get(context.Background(), client, rs.Primary.ID).Extract()
			if err == nil {
				return fmt.Errorf("role %s still exists", rs.Primary.ID)
			}
			if !gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
				return fmt.Errorf("unexpected error checking role %s: %w", rs.Primary.ID, err)
			}
		}
		return nil
	}
}
