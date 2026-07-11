// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package identity_test

import (
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"

	"github.com/platform9/terraform-provider-pcd/internal/acctest"
)

// TestAccIdentityAuthScopeDataSource_basic verifies the provider authenticates
// against the lab and the data source reports the current token scope. The
// provider is configured entirely from the OS_* environment.
func TestAccIdentityAuthScopeDataSource_basic(t *testing.T) {
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { acctest.PreCheck(t) },
		ProtoV6ProviderFactories: acctest.ProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: testAccAuthScopeConfigBasic,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("data.pcd_identity_auth_scope.current", "name", "current"),
					resource.TestCheckResourceAttr("data.pcd_identity_auth_scope.current", "id", "current"),
					resource.TestCheckResourceAttrSet("data.pcd_identity_auth_scope.current", "user_id"),
					resource.TestCheckResourceAttrSet("data.pcd_identity_auth_scope.current", "user_name"),
					resource.TestCheckResourceAttrSet("data.pcd_identity_auth_scope.current", "project_id"),
					resource.TestCheckResourceAttrSet("data.pcd_identity_auth_scope.current", "project_name"),
					resource.TestCheckResourceAttrSet("data.pcd_identity_auth_scope.current", "roles.0.role_name"),
				),
			},
		},
	})
}

const testAccAuthScopeConfigBasic = `
data "pcd_identity_auth_scope" "current" {
  name = "current"
}
`
