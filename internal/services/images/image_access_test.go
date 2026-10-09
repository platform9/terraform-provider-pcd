// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package images_test

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"testing"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/image/v2/members"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"

	"github.com/platform9/terraform-provider-pcd/internal/acctest"
	"github.com/platform9/terraform-provider-pcd/internal/clients"
)

// TestAccImagesImageAccess_basic shares an image with a project of its own,
// lets the membership sit pending, then accepts it as the owner (an admin
// may), imports it, and checks the revoke when the member is removed. It
// touches only objects it creates.
func TestAccImagesImageAccess_basic(t *testing.T) {
	const rn = "pcd_images_image_access.test"
	imgFile := writeTempImage(t)
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { acctest.PreCheck(t) },
		ProtoV6ProviderFactories: acctest.ProtoV6ProviderFactories,
		// The last step removes the member and checks the revoke while the image
		// still exists, so CheckDestroy sees no membership in state.
		CheckDestroy: resource.ComposeAggregateTestCheckFunc(
			testAccCheckImageMemberDestroy(t, "pcd_images_image_access"),
			testAccCheckImageDestroy(t),
		),
		Steps: []resource.TestStep{
			{
				Config: testAccImageAccessConfig(imgFile, "shared", ""),
				Check: resource.ComposeAggregateTestCheckFunc(
					testAccCheckImageMemberStatus(t, rn, "pending"),
					resource.TestCheckResourceAttr(rn, "status", "pending"),
					resource.TestCheckResourceAttrPair(rn, "member_id", "pcd_identity_project.member", "id"),
					resource.TestCheckResourceAttrSet(rn, "created_at"),
				),
			},
			{
				Config: testAccImageAccessConfig(imgFile, "shared", `status = "accepted"`),
				Check: resource.ComposeAggregateTestCheckFunc(
					testAccCheckImageMemberStatus(t, rn, "accepted"),
					resource.TestCheckResourceAttr(rn, "status", "accepted"),
				),
			},
			{
				ResourceName:      rn,
				ImportState:       true,
				ImportStateVerify: true,
				// Glance's PUT answer can carry updated_at a second before the stored, rounded value; Computed, it never plans a change.
				ImportStateVerifyIgnore: []string{"updated_at"},
			},
			{
				// The member is removed while the image and the project stay,
				// so the image is still shared and Glance must no longer list
				// the project. Destroying everything at once could not show
				// this: Glance answers 404 for every member of a deleted image.
				Config: testAccImageAccessParentsConfig(imgFile, "shared"),
				Check: resource.ComposeTestCheckFunc(
					testAccCheckImageExists(t, "pcd_images_image.shared"),
					resource.TestCheckResourceAttr("pcd_images_image.shared", "visibility", "shared"),
					testAccCheckImageMemberRemoved(t, "pcd_images_image.shared", "pcd_identity_project.member"),
				),
			},
		},
	})
}

// TestAccImagesImageAccess_privateImage checks the provider's message when
// Glance refuses a member because the image is not shared.
func TestAccImagesImageAccess_privateImage(t *testing.T) {
	imgFile := writeTempImage(t)
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { acctest.PreCheck(t) },
		ProtoV6ProviderFactories: acctest.ProtoV6ProviderFactories,
		CheckDestroy:             testAccCheckImageDestroy(t),
		Steps: []resource.TestStep{
			{
				Config:      testAccImageAccessConfig(imgFile, "private", ""),
				ExpectError: regexp.MustCompile(`Image cannot be shared`),
			},
		},
	})
}

func testAccImageAccessConfig(path, visibility, statusLine string) string {
	return testAccImageAccessParentsConfig(path, visibility) + fmt.Sprintf(`
resource "pcd_images_image_access" "test" {
  image_id  = pcd_images_image.shared.id
  member_id = pcd_identity_project.member.id
  %s
}
`, statusLine)
}

// testAccImageAccessParentsConfig is the project and the image the membership
// joins, without the membership.
func testAccImageAccessParentsConfig(path, visibility string) string {
	return fmt.Sprintf(`
resource "pcd_identity_project" "member" {
  name = "tf-acc-image-member"
}

resource "pcd_images_image" "shared" {
  name             = "tf-acc-image-access"
  container_format = "bare"
  disk_format      = "raw"
  local_file_path  = %q
  visibility       = %q
}
`, path, visibility)
}

// testAccImageMember reads one membership as the provider's credentials see it.
func testAccImageMember(t *testing.T, imageID, memberID string) (*members.Member, error) {
	client, err := acctest.LabConfig(t).ImageV2Client()
	if err != nil {
		return nil, err
	}
	return clients.RequireObject(members.Get(context.Background(), client, imageID, memberID).Extract())
}

func testAccCheckImageMemberStatus(t *testing.T, n, want string) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		rs := s.RootModule().Resources[n]
		if rs == nil {
			return fmt.Errorf("not found in state: %s", n)
		}
		imageID, memberID, _ := strings.Cut(rs.Primary.ID, "/")
		m, err := testAccImageMember(t, imageID, memberID)
		if err != nil {
			return fmt.Errorf("member %s of image %s: %w", memberID, imageID, err)
		}
		if m.Status != want {
			return fmt.Errorf("member %s of image %s is %s in Glance, want %s", memberID, imageID, m.Status, want)
		}
		return nil
	}
}

// testAccCheckImageMemberRemoved checks that the project in projectRN is no
// longer a member of the image in imageRN. Glance also answers 404 for every
// member of a deleted image or one that is no longer shared, so run it after
// checking that the image exists and is still shared.
func testAccCheckImageMemberRemoved(t *testing.T, imageRN, projectRN string) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		img, project := s.RootModule().Resources[imageRN], s.RootModule().Resources[projectRN]
		if img == nil || project == nil {
			return fmt.Errorf("%s and %s must both be in state", imageRN, projectRN)
		}
		_, err := testAccImageMember(t, img.Primary.ID, project.Primary.ID)
		switch {
		case err == nil:
			return fmt.Errorf("project %s is still a member of image %s after the member was removed",
				project.Primary.ID, img.Primary.ID)
		case !gophercloud.ResponseCodeIs(err, http.StatusNotFound):
			return fmt.Errorf("checking member %s of image %s: %w", project.Primary.ID, img.Primary.ID, err)
		}
		return nil
	}
}

// testAccCheckImageMemberDestroy checks that memberships of resourceType are
// gone: Glance answers 404 for a removed member and for a deleted image.
func testAccCheckImageMemberDestroy(t *testing.T, resourceType string) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		for _, rs := range s.RootModule().Resources {
			if rs.Type != resourceType {
				continue
			}
			imageID, memberID, _ := strings.Cut(rs.Primary.ID, "/")
			_, err := testAccImageMember(t, imageID, memberID)
			if err == nil {
				return fmt.Errorf("project %s is still a member of image %s", memberID, imageID)
			}
			if !gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
				return fmt.Errorf("checking member %s of image %s: %w", memberID, imageID, err)
			}
		}
		return nil
	}
}
