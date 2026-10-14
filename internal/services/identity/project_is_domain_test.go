// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package identity_test

import (
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"

	"github.com/platform9/terraform-provider-pcd/internal/acctest"
)

// TestAccIdentityProject_isDomainDestroy creates an enabled project that acts
// as a domain and destroys it. Keystone refuses to delete an enabled domain,
// so the destroy only succeeds because Delete disables it first.
func TestAccIdentityProject_isDomainDestroy(t *testing.T) {
	const resourceName = "pcd_identity_project.domain"

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { acctest.PreCheck(t) },
		ProtoV6ProviderFactories: acctest.ProtoV6ProviderFactories,
		CheckDestroy:             testAccCheckProjectDestroy(t),
		Steps: []resource.TestStep{
			{
				Config: `
resource "pcd_identity_project" "domain" {
  name      = "tf-acc-is-domain"
  is_domain = true
  enabled   = true
}
`,
				Check: resource.ComposeAggregateTestCheckFunc(
					testAccCheckProjectExists(t, resourceName),
					resource.TestCheckResourceAttr(resourceName, "is_domain", "true"),
					resource.TestCheckResourceAttr(resourceName, "enabled", "true"),
				),
			},
		},
	})
}
