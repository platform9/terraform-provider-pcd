// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package compute_test

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"testing"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/flavors"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"

	"github.com/platform9/terraform-provider-pcd/internal/acctest"
)

// TestAccComputeFlavorAccess_basic grants a project access to a private
// flavor, checks Nova's access list, imports the grant, and checks the revoke
// when the grant is removed. It creates its own project and flavor, so it
// touches nothing shared.
func TestAccComputeFlavorAccess_basic(t *testing.T) {
	const rn = "pcd_compute_flavor_access.test"
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { acctest.PreCheck(t) },
		ProtoV6ProviderFactories: acctest.ProtoV6ProviderFactories,
		// The last step removes the grant and checks the revoke, so CheckDestroy sees no grant in state.
		CheckDestroy: testAccCheckFlavorAccessDestroy(t),
		Steps: []resource.TestStep{
			{
				Config: testAccFlavorAccessConfig(false),
				Check: resource.ComposeAggregateTestCheckFunc(
					testAccCheckFlavorAccessExists(t, rn),
					resource.TestCheckResourceAttrPair(rn, "flavor_id", "pcd_compute_flavor.test", "id"),
					resource.TestCheckResourceAttrPair(rn, "tenant_id", "pcd_identity_project.test", "id"),
					resource.TestCheckResourceAttrSet(rn, "region"),
				),
			},
			{ResourceName: rn, ImportState: true, ImportStateVerify: true},
			{
				// The grant is removed while its flavor and project stay, so
				// the access list still exists and must no longer name the
				// project. Destroying everything at once could not show this:
				// a deleted flavor has no access list to check.
				Config: testAccFlavorAccessParentsConfig(false),
				Check: resource.ComposeTestCheckFunc(
					testAccCheckFlavorExists(t, "pcd_compute_flavor.test"),
					testAccCheckFlavorAccessRevoked(t, "pcd_compute_flavor.test", "pcd_identity_project.test"),
				),
			},
		},
	})
}

// TestAccComputeFlavorAccess_publicFlavor checks that a grant on a public
// flavor fails at apply with the provider's own message.
func TestAccComputeFlavorAccess_publicFlavor(t *testing.T) {
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { acctest.PreCheck(t) },
		ProtoV6ProviderFactories: acctest.ProtoV6ProviderFactories,
		CheckDestroy:             testAccCheckFlavorDestroy(t),
		Steps: []resource.TestStep{
			{
				Config:      testAccFlavorAccessConfig(true),
				ExpectError: regexp.MustCompile(`Flavor is public`),
			},
		},
	})
}

func testAccFlavorAccessConfig(public bool) string {
	return testAccFlavorAccessParentsConfig(public) + `
resource "pcd_compute_flavor_access" "test" {
  flavor_id = pcd_compute_flavor.test.id
  tenant_id = pcd_identity_project.test.id
}
`
}

// testAccFlavorAccessParentsConfig is the project and the flavor the grant
// joins, without the grant.
func testAccFlavorAccessParentsConfig(public bool) string {
	return fmt.Sprintf(`
resource "pcd_identity_project" "test" {
  name = "tf-acc-flavor-access"
}

resource "pcd_compute_flavor" "test" {
  name      = "tf-acc-flavor-access"
  ram       = 256
  vcpus     = 1
  disk      = 1
  is_public = %t
}
`, public)
}

// flavorAccessListed reports whether Nova lists tenantID on flavorID; a 404
// (the flavor is gone) counts as not listed.
func flavorAccessListed(t *testing.T, flavorID, tenantID string) (bool, error) {
	client, err := acctest.LabConfig(t).ComputeV2Client()
	if err != nil {
		return false, err
	}
	pages, err := flavors.ListAccesses(client, flavorID).AllPages(context.Background())
	if err != nil {
		if gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
			return false, nil
		}
		return false, err
	}
	accesses, err := flavors.ExtractAccesses(pages)
	if err != nil {
		return false, err
	}
	for _, a := range accesses {
		if a.TenantID == tenantID {
			return true, nil
		}
	}
	return false, nil
}

func testAccCheckFlavorAccessExists(t *testing.T, n string) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		rs := s.RootModule().Resources[n]
		if rs == nil {
			return fmt.Errorf("not found in state: %s", n)
		}
		flavorID, tenantID, ok := strings.Cut(rs.Primary.ID, "/")
		if !ok {
			return fmt.Errorf("id %q is not <flavor_id>/<tenant_id>", rs.Primary.ID)
		}
		listed, err := flavorAccessListed(t, flavorID, tenantID)
		if err != nil {
			return err
		}
		if !listed {
			return fmt.Errorf("project %s is not on flavor %s's access list", tenantID, flavorID)
		}
		return nil
	}
}

// testAccCheckFlavorAccessRevoked checks that the project in projectRN is not
// on the access list of the flavor in flavorRN. flavorAccessListed counts a
// deleted flavor as not listed, so run it after testAccCheckFlavorExists.
func testAccCheckFlavorAccessRevoked(t *testing.T, flavorRN, projectRN string) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		flavor, project := s.RootModule().Resources[flavorRN], s.RootModule().Resources[projectRN]
		if flavor == nil || project == nil {
			return fmt.Errorf("%s and %s must both be in state", flavorRN, projectRN)
		}
		listed, err := flavorAccessListed(t, flavor.Primary.ID, project.Primary.ID)
		if err != nil {
			return err
		}
		if listed {
			return fmt.Errorf("project %s still has access to flavor %s after the grant was removed",
				project.Primary.ID, flavor.Primary.ID)
		}
		return nil
	}
}

func testAccCheckFlavorAccessDestroy(t *testing.T) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		for _, rs := range s.RootModule().Resources {
			if rs.Type != "pcd_compute_flavor_access" {
				continue
			}
			flavorID, tenantID, _ := strings.Cut(rs.Primary.ID, "/")
			listed, err := flavorAccessListed(t, flavorID, tenantID)
			if err != nil {
				return err
			}
			if listed {
				return fmt.Errorf("project %s still has access to flavor %s", tenantID, flavorID)
			}
		}
		return nil
	}
}
