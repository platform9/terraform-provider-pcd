// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package identity_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/gophercloud/gophercloud/v2/openstack/identity/v3/users"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"

	"github.com/platform9/terraform-provider-pcd/internal/acctest"
	"github.com/platform9/terraform-provider-pcd/internal/clients"
)

// TestAccIdentityUserResource_emailDisplayName sets the email and display name
// the PCD UI shows, changes one and clears the other, reads both through the
// data source, then drops both from the configuration (Terraform leaves the
// values alone) and imports the user.
func TestAccIdentityUserResource_emailDisplayName(t *testing.T) {
	const rn = "pcd_identity_user.test"
	const ds = "data.pcd_identity_user.test"

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { acctest.PreCheck(t) },
		ProtoV6ProviderFactories: acctest.ProtoV6ProviderFactories,
		CheckDestroy:             testAccCheckUserDestroy(t),
		Steps: []resource.TestStep{
			{
				Config: testAccUserExtrasConfig(`
  email        = "tf-acc-user-extras@example.com"
  display_name = "TF Acc User"`),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(rn, "email", "tf-acc-user-extras@example.com"),
					resource.TestCheckResourceAttr(rn, "display_name", "TF Acc User"),
					resource.TestCheckResourceAttr(ds, "email", "tf-acc-user-extras@example.com"),
					resource.TestCheckResourceAttr(ds, "display_name", "TF Acc User"),
					testAccCheckUserExtra(t, rn, "email", "tf-acc-user-extras@example.com"),
					testAccCheckUserExtra(t, rn, "displayname", "TF Acc User"),
				),
			},
			{
				Config: testAccUserExtrasConfig(`
  email        = ""
  display_name = "TF Acc User Renamed"`),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(rn, "email", ""),
					resource.TestCheckResourceAttr(rn, "display_name", "TF Acc User Renamed"),
					testAccCheckUserExtra(t, rn, "email", ""),
					testAccCheckUserExtra(t, rn, "displayname", "TF Acc User Renamed"),
				),
			},
			{
				// Unset attributes are not managed: no change is planned and the
				// server keeps its values.
				Config: testAccUserExtrasConfig(""),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()},
				},
				Check: testAccCheckUserExtra(t, rn, "displayname", "TF Acc User Renamed"),
			},
			{
				ResourceName:            rn,
				ImportState:             true,
				ImportStateVerify:       true,
				ImportStateVerifyIgnore: []string{"password"}, // write-only, never read back
			},
		},
	})
}

func testAccUserExtrasConfig(extras string) string {
	return fmt.Sprintf(`
resource "pcd_identity_user" "test" {
  name     = "tf-acc-user-extras"
  password = "Tf-Acc-Passw0rd!"%s
}

data "pcd_identity_user" "test" {
  user_id = pcd_identity_user.test.id
}
`, extras)
}

// testAccCheckUserExtra reads the user's extra attribute key from Keystone.
func testAccCheckUserExtra(t *testing.T, n, key, want string) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		rs, ok := s.RootModule().Resources[n]
		if !ok {
			return fmt.Errorf("not found in state: %s", n)
		}
		client, err := acctest.LabConfig(t).IdentityV3Client()
		if err != nil {
			return err
		}
		u, err := clients.RequireObject(users.Get(context.Background(), client, rs.Primary.ID).Extract())
		if err != nil {
			return fmt.Errorf("user %s not found via API: %w", rs.Primary.ID, err)
		}
		if got, _ := u.Extra[key].(string); got != want {
			return fmt.Errorf("user %s extra %s = %q, want %q", rs.Primary.ID, key, got, want)
		}
		return nil
	}
}
