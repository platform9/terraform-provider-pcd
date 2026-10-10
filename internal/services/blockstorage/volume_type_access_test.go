// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package blockstorage_test

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"testing"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/blockstorage/v3/volumetypes"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"

	"github.com/platform9/terraform-provider-pcd/internal/acctest"
)

// TestAccBlockStorageVolumeTypeAccess_basic grants a project access to a
// private volume type, checks Cinder's access list, imports the grant, and
// checks the revoke when the grant is removed. It creates its own project and
// type.
func TestAccBlockStorageVolumeTypeAccess_basic(t *testing.T) {
	const rn = "pcd_blockstorage_volume_type_access.test"
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { acctest.PreCheck(t) },
		ProtoV6ProviderFactories: acctest.ProtoV6ProviderFactories,
		// The last step removes the grant and checks the revoke, so CheckDestroy sees no grant in state.
		CheckDestroy: resource.ComposeAggregateTestCheckFunc(
			testAccCheckVolumeTypeAccessDestroy(t),
			testAccCheckVolumeTypeDestroy(t),
		),
		Steps: []resource.TestStep{
			{
				Config: testAccVolumeTypeAccessConfig(false),
				Check: resource.ComposeAggregateTestCheckFunc(
					testAccCheckVolumeTypeAccessExists(t, rn),
					resource.TestCheckResourceAttrPair(rn, "volume_type_id", "pcd_blockstorage_volume_type.test", "id"),
					resource.TestCheckResourceAttrPair(rn, "project_id", "pcd_identity_project.test", "id"),
				),
			},
			{ResourceName: rn, ImportState: true, ImportStateVerify: true},
			{
				// The grant is removed while its type and project stay, so the
				// type's access list still exists and must no longer name the
				// project. Destroying everything at once could not show this:
				// a deleted type has no access list to check.
				Config: testAccVolumeTypeAccessParentsConfig(false),
				Check: resource.ComposeTestCheckFunc(
					testAccCheckVolumeTypeExists(t, "pcd_blockstorage_volume_type.test"),
					testAccCheckVolumeTypeAccessRevoked(t, "pcd_blockstorage_volume_type.test", "pcd_identity_project.test"),
				),
			},
		},
	})
}

// TestAccBlockStorageVolumeTypeAccess_publicType checks the provider's
// message when Cinder refuses a grant on a public type.
func TestAccBlockStorageVolumeTypeAccess_publicType(t *testing.T) {
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { acctest.PreCheck(t) },
		ProtoV6ProviderFactories: acctest.ProtoV6ProviderFactories,
		CheckDestroy:             testAccCheckVolumeTypeDestroy(t),
		Steps: []resource.TestStep{
			{
				Config:      testAccVolumeTypeAccessConfig(true),
				ExpectError: regexp.MustCompile(`Volume type access refused`),
			},
		},
	})
}

func testAccVolumeTypeAccessConfig(public bool) string {
	return testAccVolumeTypeAccessParentsConfig(public) + `
resource "pcd_blockstorage_volume_type_access" "test" {
  volume_type_id = pcd_blockstorage_volume_type.test.id
  project_id     = pcd_identity_project.test.id
}
`
}

// testAccVolumeTypeAccessParentsConfig is the project and the volume type the
// grant joins, without the grant.
func testAccVolumeTypeAccessParentsConfig(public bool) string {
	return fmt.Sprintf(`
resource "pcd_identity_project" "test" {
  name = "tf-acc-vt-access"
}

resource "pcd_blockstorage_volume_type" "test" {
  name      = "tf-acc-vt-access"
  is_public = %t
}
`, public)
}

// volumeTypeAccessListed reports whether Cinder lists projectID on typeID; a
// 404 (the type is gone or public) counts as not listed.
func volumeTypeAccessListed(t *testing.T, typeID, projectID string) (bool, error) {
	client, err := acctest.LabConfig(t).BlockStorageV3Client()
	if err != nil {
		return false, err
	}
	pages, err := volumetypes.ListAccesses(client, typeID).AllPages(context.Background())
	if err != nil {
		if gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
			return false, nil
		}
		return false, err
	}
	accesses, err := volumetypes.ExtractAccesses(pages)
	if err != nil {
		return false, err
	}
	for _, a := range accesses {
		if a.ProjectID == projectID {
			return true, nil
		}
	}
	return false, nil
}

func testAccCheckVolumeTypeAccessExists(t *testing.T, n string) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		rs := s.RootModule().Resources[n]
		if rs == nil {
			return fmt.Errorf("not found in state: %s", n)
		}
		typeID, projectID, ok := strings.Cut(rs.Primary.ID, "/")
		if !ok {
			return fmt.Errorf("id %q is not <volume_type_id>/<project_id>", rs.Primary.ID)
		}
		listed, err := volumeTypeAccessListed(t, typeID, projectID)
		if err != nil {
			return err
		}
		if !listed {
			return fmt.Errorf("project %s is not on volume type %s's access list", projectID, typeID)
		}
		return nil
	}
}

// testAccCheckVolumeTypeAccessRevoked checks that the project in projectRN is
// not on the access list of the volume type in typeRN. volumeTypeAccessListed
// counts a deleted type as not listed, so run it after
// testAccCheckVolumeTypeExists.
func testAccCheckVolumeTypeAccessRevoked(t *testing.T, typeRN, projectRN string) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		vt, project := s.RootModule().Resources[typeRN], s.RootModule().Resources[projectRN]
		if vt == nil || project == nil {
			return fmt.Errorf("%s and %s must both be in state", typeRN, projectRN)
		}
		listed, err := volumeTypeAccessListed(t, vt.Primary.ID, project.Primary.ID)
		if err != nil {
			return err
		}
		if listed {
			return fmt.Errorf("project %s still has access to volume type %s after the grant was removed",
				project.Primary.ID, vt.Primary.ID)
		}
		return nil
	}
}

func testAccCheckVolumeTypeAccessDestroy(t *testing.T) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		for _, rs := range s.RootModule().Resources {
			if rs.Type != "pcd_blockstorage_volume_type_access" {
				continue
			}
			typeID, projectID, _ := strings.Cut(rs.Primary.ID, "/")
			listed, err := volumeTypeAccessListed(t, typeID, projectID)
			if err != nil {
				return err
			}
			if listed {
				return fmt.Errorf("project %s still has access to volume type %s", projectID, typeID)
			}
		}
		return nil
	}
}
