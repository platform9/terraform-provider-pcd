// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package images_test

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/gophercloud/gophercloud/v2/openstack/identity/v3/projects"
	"github.com/hashicorp/terraform-plugin-framework/providerserver"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"

	"github.com/platform9/terraform-provider-pcd/internal/acctest"
	"github.com/platform9/terraform-provider-pcd/internal/clients"
	"github.com/platform9/terraform-provider-pcd/internal/provider"
)

// TestAccImagesImageAccessAccept_selfShare follows upstream's test: the image
// is shared with the provider's own project, so one provider both shares and
// decides. It covers reject, accept and import, and checks the reject a
// destroy sends when the accept resource is removed.
func TestAccImagesImageAccessAccept_selfShare(t *testing.T) {
	const rn = "pcd_images_image_access_accept.test"
	imgFile := writeTempImage(t)
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { acctest.PreCheck(t) },
		ProtoV6ProviderFactories: acctest.ProtoV6ProviderFactories,
		// The last step removes the accept resource and checks the reject while
		// the image and the share still exist; destroying everything at once
		// could not show it, since the membership goes with the image.
		CheckDestroy: testAccCheckImageDestroy(t),
		Steps: []resource.TestStep{
			{
				Config: testAccImageAccessAcceptSelfConfig(imgFile, "rejected"),
				Check: resource.ComposeAggregateTestCheckFunc(
					testAccCheckImageMemberStatus(t, rn, "rejected"),
					resource.TestCheckResourceAttrPair(rn, "member_id", "data.pcd_identity_auth_scope.me", "project_id"),
				),
			},
			{
				// Accepted last, so the reject the removal sends below is a change
				// Glance must show.
				Config: testAccImageAccessAcceptSelfConfig(imgFile, "accepted"),
				Check:  testAccCheckImageMemberStatus(t, rn, "accepted"),
			},
			{
				ResourceName:      rn,
				ImportState:       true,
				ImportStateVerify: true,
				// Glance's PUT answer can carry updated_at a second before the stored, rounded value; Computed, it never plans a change.
				ImportStateVerifyIgnore: []string{"updated_at"},
			},
			{
				// The accept resource is removed while the image and the share stay,
				// so its destroy must leave the membership rejected.
				Config: testAccImageAccessAcceptSelfParentsConfig(imgFile),
				Check: resource.ComposeTestCheckFunc(
					testAccCheckImageExists(t, "pcd_images_image.shared"),
					testAccCheckImageMemberStatus(t, "pcd_images_image_access.share", "rejected"),
				),
			},
		},
	})
}

func testAccImageAccessAcceptSelfConfig(path, status string) string {
	return testAccImageAccessAcceptSelfParentsConfig(path) + fmt.Sprintf(`
resource "pcd_images_image_access_accept" "test" {
  image_id = pcd_images_image_access.share.image_id
  status   = %q
}
`, status)
}

// testAccImageAccessAcceptSelfParentsConfig is the image and its share with the
// provider's own project, without the accept resource.
func testAccImageAccessAcceptSelfParentsConfig(path string) string {
	return fmt.Sprintf(`
data "pcd_identity_auth_scope" "me" {
  name = "me"
}

resource "pcd_images_image" "shared" {
  name             = "tf-acc-image-accept"
  container_format = "bare"
  disk_format      = "raw"
  local_file_path  = %q
  visibility       = "shared"
}

resource "pcd_images_image_access" "share" {
  image_id  = pcd_images_image.shared.id
  member_id = data.pcd_identity_auth_scope.me.project_id
}
`, path)
}

// TestAccImagesImageAccessAccept_crossProject shares an image from the
// provider's project with a second project and accepts it there, through a
// second provider configuration scoped to that project, the way two teams
// would. A configuration does that with a provider alias; this test uses
// pcdconsumer instead (see testAccImageAccessConsumerFactories). The second
// project must exist before the plan, with a role other than admin for the
// test user on it, because the consumer provider authenticates when Terraform
// configures it and Glance treats an admin differently (it ignores
// member_status and shows every member); its ID comes from
// PCD_ACC_SECOND_PROJECT_ID.
func TestAccImagesImageAccessAccept_crossProject(t *testing.T) {
	projectID := os.Getenv("PCD_ACC_SECOND_PROJECT_ID")
	if projectID == "" {
		t.Skip("PCD_ACC_SECOND_PROJECT_ID not set; skipping the cross-project image share")
	}
	if os.Getenv("OS_PROJECT_ID") != "" || os.Getenv("OS_TENANT_ID") != "" {
		t.Skip("OS_PROJECT_ID or OS_TENANT_ID is exported: the consumer provider would inherit it next to " +
			"tenant_name and fail to authenticate; unset both to run the cross-project image share")
	}
	acctest.PreCheck(t)
	identity, err := acctest.LabConfig(t).IdentityV3Client()
	if err != nil {
		t.Fatal(err)
	}
	project, err := clients.RequireObject(projects.Get(context.Background(), identity, projectID).Extract())
	if err != nil {
		t.Fatalf("reading PCD_ACC_SECOND_PROJECT_ID %s: %v", projectID, err)
	}
	const rn = "pcd_images_image_access_accept.consumer"
	imgFile := writeTempImage(t)
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { acctest.PreCheck(t) },
		ProtoV6ProviderFactories: testAccImageAccessConsumerFactories(),
		// The last step removes the accept resource and checks the reject while
		// the image and the share still exist; CheckDestroy then checks that the
		// share and the image are gone.
		CheckDestroy: resource.ComposeAggregateTestCheckFunc(
			testAccCheckImageMemberDestroy(t, "pcd_images_image_access"),
			testAccCheckImageDestroy(t),
		),
		Steps: []resource.TestStep{
			{
				// Shared, not yet decided: Glance lists only accepted shares by
				// default, so the consumer finds it by name with
				// member_status = "pending".
				Config: testAccImageAccessCrossConfig(imgFile, project.Name, projectID, "pending", ""),
				Check: resource.ComposeAggregateTestCheckFunc(
					testAccCheckImageMemberStatus(t, "pcd_images_image_access.share", "pending"),
					resource.TestCheckResourceAttrPair("data.pcd_images_image.found", "id", "pcd_images_image.shared", "id"),
				),
			},
			{
				// Accepted by the consumer, with member_id left to detection. The
				// lookup switches to "all": it is read at plan time, while the share
				// is still pending, and again after the accept, and "pending" would
				// no longer find the image then.
				Config: testAccImageAccessCrossConfig(imgFile, project.Name, projectID, "all", "accepted"),
				Check: resource.ComposeAggregateTestCheckFunc(
					testAccCheckImageMemberStatus(t, rn, "accepted"),
					resource.TestCheckResourceAttr(rn, "member_id", projectID),
				),
			},
			{
				// The consumer's accept resource is removed while the image and
				// the share stay, so its destroy must leave the membership
				// rejected. "all" still finds the image once it is rejected.
				Config: testAccImageAccessCrossConfig(imgFile, project.Name, projectID, "all", ""),
				Check: resource.ComposeTestCheckFunc(
					testAccCheckImageExists(t, "pcd_images_image.shared"),
					testAccCheckImageMemberStatus(t, "pcd_images_image_access.share", "rejected"),
				),
			},
		},
	})
}

// testAccImageAccessConsumerFactories serves the provider twice: as pcd for the
// owner, and as pcdconsumer, a second in-process server, for the member
// project. The test harness serves every configuration of one provider, the
// default and its aliases, from a single server, which keeps one configured
// client: the configuration set up last would supply the client for all of
// them. With a provider binary, Terraform starts one plugin process per
// configuration, so an alias works there.
func testAccImageAccessConsumerFactories() map[string]func() (tfprotov6.ProviderServer, error) {
	return map[string]func() (tfprotov6.ProviderServer, error){
		"pcd":         acctest.ProtoV6ProviderFactories["pcd"],
		"pcdconsumer": providerserver.NewProtocol6WithError(provider.New("test")()),
	}
}

// testAccImageAccessCrossConfig shares the image with the consumer project and,
// when acceptStatus is set, decides as that project. lookupStatus is the
// member_status the consumer's image lookup uses.
func testAccImageAccessCrossConfig(path, consumerName, consumerID, lookupStatus, acceptStatus string) string {
	accept := ""
	if acceptStatus != "" {
		accept = fmt.Sprintf(`
resource "pcd_images_image_access_accept" "consumer" {
  provider = pcdconsumer
  image_id = pcd_images_image_access.share.image_id
  status   = %q
}
`, acceptStatus)
	}
	return fmt.Sprintf(`
provider "pcd" {}

provider "pcdconsumer" {
  tenant_name = %q
}

resource "pcd_images_image" "shared" {
  name             = "tf-acc-image-cross"
  container_format = "bare"
  disk_format      = "raw"
  local_file_path  = %q
  visibility       = "shared"
}

resource "pcd_images_image_access" "share" {
  image_id  = pcd_images_image.shared.id
  member_id = %q
}

data "pcd_images_image" "found" {
  provider      = pcdconsumer
  name          = pcd_images_image.shared.name
  visibility    = "shared"
  member_status = %q
  depends_on    = [pcd_images_image_access.share]
}
%s`, consumerName, path, consumerID, lookupStatus, accept)
}
