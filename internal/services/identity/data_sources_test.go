// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package identity_test

import (
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"

	"github.com/platform9/terraform-provider-pcd/internal/acctest"
)

// TestAccIdentityDataSources_byName creates a project, role, and user, then looks
// each up by name and asserts the data source resolves to the same ID.
func TestAccIdentityDataSources_byName(t *testing.T) {
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { acctest.PreCheck(t) },
		ProtoV6ProviderFactories: acctest.ProtoV6ProviderFactories,
		CheckDestroy: resource.ComposeAggregateTestCheckFunc(
			testAccCheckProjectDestroy(t),
			testAccCheckRoleDestroy(t),
			testAccCheckUserDestroy(t),
		),
		Steps: []resource.TestStep{
			{
				Config: testAccIdentityDataSourcesConfig,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrPair(
						"data.pcd_identity_project.by_name", "id",
						"pcd_identity_project.test", "id"),
					resource.TestCheckResourceAttrPair(
						"data.pcd_identity_role.by_name", "id",
						"pcd_identity_role.test", "id"),
					resource.TestCheckResourceAttrPair(
						"data.pcd_identity_user.by_name", "id",
						"pcd_identity_user.test", "id"),
				),
			},
		},
	})
}

const testAccIdentityDataSourcesConfig = `
resource "pcd_identity_project" "test" {
  name = "tf-acc-ds-project"
}

data "pcd_identity_project" "by_name" {
  name = pcd_identity_project.test.name
}

resource "pcd_identity_role" "test" {
  name = "tf-acc-ds-role"
}

data "pcd_identity_role" "by_name" {
  name = pcd_identity_role.test.name
}

resource "pcd_identity_user" "test" {
  name     = "tf-acc-ds-user"
  password = "Tf-Acc-Passw0rd!"
}

data "pcd_identity_user" "by_name" {
  name = pcd_identity_user.test.name
}
`
