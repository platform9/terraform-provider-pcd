// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package identity_test

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/identity/v3/users"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"

	"github.com/platform9/terraform-provider-pcd/internal/acctest"
)

func TestAccIdentityUserResource_basic(t *testing.T) {
	const resourceName = "pcd_identity_user.test"
	name := "tf-acc-identity-user"

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { acctest.PreCheck(t) },
		ProtoV6ProviderFactories: acctest.ProtoV6ProviderFactories,
		CheckDestroy:             testAccCheckUserDestroy(t),
		Steps: []resource.TestStep{
			{
				Config: testAccUserConfig(name, "initial", true),
				Check: resource.ComposeAggregateTestCheckFunc(
					testAccCheckUserExists(t, resourceName),
					resource.TestCheckResourceAttr(resourceName, "name", name),
					resource.TestCheckResourceAttr(resourceName, "description", "initial"),
					resource.TestCheckResourceAttr(resourceName, "enabled", "true"),
				),
			},
			{
				Config: testAccUserConfig(name, "updated", false),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(resourceName, "description", "updated"),
					resource.TestCheckResourceAttr(resourceName, "enabled", "false"),
				),
			},
			{
				ResourceName:            resourceName,
				ImportState:             true,
				ImportStateVerify:       true,
				ImportStateVerifyIgnore: []string{"password"}, // write-only, never read back
			},
		},
	})
}

func testAccUserConfig(name, description string, enabled bool) string {
	return fmt.Sprintf(`
resource "pcd_identity_user" "test" {
  name        = %q
  description = %q
  enabled     = %t
  password    = "Tf-Acc-Passw0rd!"
}
`, name, description, enabled)
}

func testAccCheckUserExists(t *testing.T, n string) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		rs, ok := s.RootModule().Resources[n]
		if !ok {
			return fmt.Errorf("not found in state: %s", n)
		}
		client, err := acctest.LabConfig(t).IdentityV3Client()
		if err != nil {
			return err
		}
		if _, err := users.Get(context.Background(), client, rs.Primary.ID).Extract(); err != nil {
			return fmt.Errorf("user %s not found via API: %w", rs.Primary.ID, err)
		}
		return nil
	}
}

func testAccCheckUserDestroy(t *testing.T) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		client, err := acctest.LabConfig(t).IdentityV3Client()
		if err != nil {
			return err
		}
		for _, rs := range s.RootModule().Resources {
			if rs.Type != "pcd_identity_user" {
				continue
			}
			_, err := users.Get(context.Background(), client, rs.Primary.ID).Extract()
			if err == nil {
				return fmt.Errorf("user %s still exists", rs.Primary.ID)
			}
			if !gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
				return fmt.Errorf("unexpected error checking user %s: %w", rs.Primary.ID, err)
			}
		}
		return nil
	}
}
