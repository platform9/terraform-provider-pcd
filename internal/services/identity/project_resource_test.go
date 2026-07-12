// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package identity_test

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/identity/v3/projects"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"

	"github.com/platform9/terraform-provider-pcd/internal/acctest"
)

func TestAccIdentityProjectResource_basic(t *testing.T) {
	const resourceName = "pcd_identity_project.test"
	name := "tf-acc-identity-project"

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { acctest.PreCheck(t) },
		ProtoV6ProviderFactories: acctest.ProtoV6ProviderFactories,
		CheckDestroy:             testAccCheckProjectDestroy(t),
		Steps: []resource.TestStep{
			{
				Config: testAccProjectConfig(name, "initial description", true),
				Check: resource.ComposeAggregateTestCheckFunc(
					testAccCheckProjectExists(t, resourceName),
					resource.TestCheckResourceAttr(resourceName, "name", name),
					resource.TestCheckResourceAttr(resourceName, "description", "initial description"),
					resource.TestCheckResourceAttr(resourceName, "enabled", "true"),
					resource.TestCheckResourceAttrSet(resourceName, "id"),
					resource.TestCheckResourceAttrSet(resourceName, "domain_id"),
				),
			},
			{
				Config: testAccProjectConfig(name, "updated description", false),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(resourceName, "description", "updated description"),
					resource.TestCheckResourceAttr(resourceName, "enabled", "false"),
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

func testAccProjectConfig(name, description string, enabled bool) string {
	return fmt.Sprintf(`
resource "pcd_identity_project" "test" {
  name        = %q
  description = %q
  enabled     = %t
}
`, name, description, enabled)
}

func testAccCheckProjectExists(t *testing.T, n string) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		rs, ok := s.RootModule().Resources[n]
		if !ok {
			return fmt.Errorf("not found in state: %s", n)
		}
		if rs.Primary.ID == "" {
			return fmt.Errorf("no ID set for %s", n)
		}
		client, err := acctest.LabConfig(t).IdentityV3Client()
		if err != nil {
			return err
		}
		if _, err := projects.Get(context.Background(), client, rs.Primary.ID).Extract(); err != nil {
			return fmt.Errorf("project %s not found via API: %w", rs.Primary.ID, err)
		}
		return nil
	}
}

func testAccCheckProjectDestroy(t *testing.T) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		client, err := acctest.LabConfig(t).IdentityV3Client()
		if err != nil {
			return err
		}
		for _, rs := range s.RootModule().Resources {
			if rs.Type != "pcd_identity_project" {
				continue
			}
			_, err := projects.Get(context.Background(), client, rs.Primary.ID).Extract()
			if err == nil {
				return fmt.Errorf("project %s still exists", rs.Primary.ID)
			}
			if !gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
				return fmt.Errorf("unexpected error checking project %s: %w", rs.Primary.ID, err)
			}
		}
		return nil
	}
}
