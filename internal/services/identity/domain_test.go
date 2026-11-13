// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package identity_test

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/identity/v3/domains"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"

	"github.com/platform9/terraform-provider-pcd/internal/acctest"
	"github.com/platform9/terraform-provider-pcd/internal/clients"
)

// TestAccIdentityDomain_basic creates a domain, renames and disables it in
// place, enables it again so destroy has to disable it before deleting it,
// looks it up through the data source, and imports it.
func TestAccIdentityDomain_basic(t *testing.T) {
	const rn = "pcd_identity_domain.test"
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { acctest.PreCheck(t) },
		ProtoV6ProviderFactories: acctest.ProtoV6ProviderFactories,
		CheckDestroy:             testAccCheckDomainDestroy(t),
		Steps: []resource.TestStep{
			{
				Config: testAccDomainConfig("tf-acc-domain", "first", true),
				Check: resource.ComposeAggregateTestCheckFunc(
					testAccCheckDomainEnabled(t, rn, true),
					resource.TestCheckResourceAttr(rn, "name", "tf-acc-domain"),
					resource.TestCheckResourceAttr(rn, "description", "first"),
					resource.TestCheckResourceAttrPair("data.pcd_identity_domain.by_name", "id", rn, "id"),
					resource.TestCheckResourceAttr("data.pcd_identity_domain.by_name", "enabled", "true"),
				),
			},
			{
				Config: testAccDomainConfig("tf-acc-domain-2", "second", false),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(rn, plancheck.ResourceActionUpdate)},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					testAccCheckDomainEnabled(t, rn, false),
					resource.TestCheckResourceAttr(rn, "name", "tf-acc-domain-2"),
					resource.TestCheckResourceAttr(rn, "description", "second"),
				),
			},
			{
				Config: testAccDomainConfig("tf-acc-domain-2", "second", true),
				Check:  testAccCheckDomainEnabled(t, rn, true),
			},
			{ResourceName: rn, ImportState: true, ImportStateVerify: true},
		},
	})
}

// TestAccIdentityDomain_withChildren destroys an enabled domain together with a
// project and a group created in it.
func TestAccIdentityDomain_withChildren(t *testing.T) {
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { acctest.PreCheck(t) },
		ProtoV6ProviderFactories: acctest.ProtoV6ProviderFactories,
		CheckDestroy: resource.ComposeAggregateTestCheckFunc(
			testAccCheckDomainDestroy(t),
			testAccCheckProjectDestroy(t),
			testAccCheckGroupDestroy(t),
		),
		Steps: []resource.TestStep{
			{
				Config: `
resource "pcd_identity_domain" "test" {
  name = "tf-acc-domain-children"
}

resource "pcd_identity_project" "test" {
  name      = "tf-acc-domain-child-project"
  domain_id = pcd_identity_domain.test.id
}

resource "pcd_identity_group" "test" {
  name      = "tf-acc-domain-child-group"
  domain_id = pcd_identity_domain.test.id
}
`,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrPair("pcd_identity_project.test", "domain_id", "pcd_identity_domain.test", "id"),
					resource.TestCheckResourceAttrPair("pcd_identity_group.test", "domain_id", "pcd_identity_domain.test", "id"),
				),
			},
		},
	})
}

// TestAccIdentityDomainDataSource_default reads Keystone's default domain by
// ID. It only reads.
func TestAccIdentityDomainDataSource_default(t *testing.T) {
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { acctest.PreCheck(t) },
		ProtoV6ProviderFactories: acctest.ProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: `
data "pcd_identity_domain" "default" {
  domain_id = "default"
}
`,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("data.pcd_identity_domain.default", "id", "default"),
					resource.TestCheckResourceAttr("data.pcd_identity_domain.default", "enabled", "true"),
					resource.TestCheckResourceAttrSet("data.pcd_identity_domain.default", "name"),
				),
			},
		},
	})
}

func testAccDomainConfig(name, description string, enabled bool) string {
	return fmt.Sprintf(`
resource "pcd_identity_domain" "test" {
  name        = %q
  description = %q
  enabled     = %t
}

data "pcd_identity_domain" "by_name" {
  name = pcd_identity_domain.test.name
}
`, name, description, enabled)
}

func testAccCheckDomainEnabled(t *testing.T, n string, want bool) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		rs, ok := s.RootModule().Resources[n]
		if !ok {
			return fmt.Errorf("not found in state: %s", n)
		}
		client, err := acctest.LabConfig(t).IdentityV3Client()
		if err != nil {
			return err
		}
		d, err := clients.RequireObject(domains.Get(context.Background(), client, rs.Primary.ID).Extract())
		if err != nil {
			return fmt.Errorf("domain %s not found via API: %w", rs.Primary.ID, err)
		}
		if d.Enabled != want {
			return fmt.Errorf("domain %s enabled = %v, want %v", rs.Primary.ID, d.Enabled, want)
		}
		return nil
	}
}

func testAccCheckDomainDestroy(t *testing.T) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		client, err := acctest.LabConfig(t).IdentityV3Client()
		if err != nil {
			return err
		}
		for _, rs := range s.RootModule().Resources {
			if rs.Type != "pcd_identity_domain" {
				continue
			}
			_, err := domains.Get(context.Background(), client, rs.Primary.ID).Extract()
			if err == nil {
				return fmt.Errorf("domain %s still exists", rs.Primary.ID)
			}
			if !gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
				return fmt.Errorf("unexpected error checking domain %s: %w", rs.Primary.ID, err)
			}
		}
		return nil
	}
}
