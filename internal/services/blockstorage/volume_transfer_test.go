// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package blockstorage_test

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"testing"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/blockstorage/v3/transfers"
	"github.com/gophercloud/gophercloud/v2/openstack/blockstorage/v3/volumes"
	"github.com/gophercloud/gophercloud/v2/openstack/identity/v3/projects"
	"github.com/gophercloud/gophercloud/v2/openstack/identity/v3/tokens"
	"github.com/hashicorp/terraform-plugin-framework/providerserver"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"

	"github.com/platform9/terraform-provider-pcd/internal/acctest"
	"github.com/platform9/terraform-provider-pcd/internal/clients"
	"github.com/platform9/terraform-provider-pcd/internal/provider"
)

// TestAccBlockStorageVolumeTransfer_cancel offers a volume, imports the
// offer, and destroys it, which cancels the transfer and returns the volume
// to "available". It needs only the test's own project.
func TestAccBlockStorageVolumeTransfer_cancel(t *testing.T) {
	const offer = "pcd_blockstorage_volume_transfer.test"
	const volume = "pcd_blockstorage_volume.test"
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { acctest.PreCheck(t) },
		ProtoV6ProviderFactories: acctest.ProtoV6ProviderFactories,
		CheckDestroy:             testAccCheckVolumeDestroy(t),
		Steps: []resource.TestStep{
			{
				Config: testAccVolumeTransferOfferConfig(),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrPair(offer, "volume_id", volume, "id"),
					resource.TestCheckResourceAttrSet(offer, "auth_key"),
					resource.TestCheckResourceAttrSet(offer, "source_project_id"),
					resource.TestCheckResourceAttr(offer, "accepted", "false"),
					testAccCheckVolumeStatus(t, volume, "awaiting-transfer"),
				),
			},
			{
				ResourceName:            offer,
				ImportState:             true,
				ImportStateVerify:       true,
				ImportStateVerifyIgnore: []string{"auth_key"},
			},
			{
				// Removing the offer from the configuration cancels it.
				Config: testAccVolumeTransferVolumeOnlyConfig(),
				Check:  testAccCheckVolumeStatus(t, volume, "available"),
			},
		},
	})
}

func testAccVolumeTransferVolumeOnlyConfig() string {
	return `
resource "pcd_blockstorage_volume" "test" {
  name = "tf-acc-xfer-vol"
  size = 1
}
`
}

func testAccVolumeTransferOfferConfig() string {
	return testAccVolumeTransferVolumeOnlyConfig() + `
resource "pcd_blockstorage_volume_transfer" "test" {
  volume_id = pcd_blockstorage_volume.test.id
  name      = "tf-acc-xfer"
}
`
}

// testAccCheckVolumeStatus reads the volume's status from Cinder.
func testAccCheckVolumeStatus(t *testing.T, n, want string) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		rs := s.RootModule().Resources[n]
		if rs == nil {
			return fmt.Errorf("not found in state: %s", n)
		}
		client, err := acctest.LabConfig(t).BlockStorageV3Client()
		if err != nil {
			return err
		}
		vol, err := volumes.Get(context.Background(), client, rs.Primary.ID).Extract()
		if err != nil {
			return fmt.Errorf("reading volume %s: %w", rs.Primary.ID, err)
		}
		if vol.Status != want {
			return fmt.Errorf("volume %s status is %q, want %q", rs.Primary.ID, vol.Status, want)
		}
		return nil
	}
}

// TestAccBlockStorageVolumeTransfer_crossProject offers a volume from the
// test's project and accepts it in a second project, through a second
// provider configuration scoped to that project, then checks that the volume
// moved and that the offer reads as accepted with nothing left to plan. A
// configuration does that with a provider alias; this test uses pcdconsumer
// instead (see testAccVolumeTransferConsumerFactories). It needs
// PCD_ACC_SECOND_PROJECT_ID: an existing project in which the test user has a
// role. A transfer within one project cannot be told apart from a canceled
// offer, so it cannot stand in.
func TestAccBlockStorageVolumeTransfer_crossProject(t *testing.T) {
	if os.Getenv("TF_ACC") == "" {
		t.Skip("TF_ACC not set; acceptance tests skipped")
	}
	destID := os.Getenv("PCD_ACC_SECOND_PROJECT_ID")
	if destID == "" {
		t.Skip("PCD_ACC_SECOND_PROJECT_ID not set; skipping the cross-project volume transfer test")
	}
	acctest.PreCheck(t)
	cfg := acctest.LabConfig(t)
	// After the accept only an admin can still read the moved volume. That
	// keeps it in state for step 3's empty plan, and lets the destroy delete
	// it from the second project. With member credentials Terraform would drop
	// the volume and plan a new one, which reads like a provider defect.
	auth, ok := cfg.Provider.GetAuthResult().(tokens.CreateResult)
	if !ok {
		t.Fatalf("unexpected auth result %T", cfg.Provider.GetAuthResult())
	}
	roles, err := auth.ExtractRoles()
	if err != nil {
		t.Fatalf("reading the token's roles: %v", err)
	}
	admin := false
	for _, r := range roles {
		admin = admin || r.Name == "admin"
	}
	if !admin {
		t.Skip("the cross-project transfer test needs admin credentials in the offering project: a member cannot read the moved volume")
	}
	identity, err := cfg.IdentityV3Client()
	if err != nil {
		t.Fatalf("identity client: %v", err)
	}
	dest, err := clients.RequireObject(projects.Get(context.Background(), identity, destID).Extract())
	if err != nil {
		t.Fatalf("reading project %s: %v", destID, err)
	}

	const offer = "pcd_blockstorage_volume_transfer.test"
	const accept = "pcd_blockstorage_volume_transfer_accept.test"
	const volume = "pcd_blockstorage_volume.test"
	const scope = "data.pcd_identity_auth_scope.consumer"

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { acctest.PreCheck(t) },
		ProtoV6ProviderFactories: testAccVolumeTransferConsumerFactories(),
		CheckDestroy:             testAccCheckVolumeDestroy(t),
		Steps: []resource.TestStep{
			{
				Config: testAccVolumeTransferOfferConfig(),
				Check:  testAccCheckVolumeStatus(t, volume, "awaiting-transfer"),
			},
			{
				Config: testAccVolumeTransferOfferConfig() + testAccVolumeTransferAcceptConfig(dest.Name),
				Check: resource.ComposeAggregateTestCheckFunc(
					// The accept ran as the second project, not the offering one.
					resource.TestCheckResourceAttr(scope, "project_id", destID),
					resource.TestCheckResourceAttrPair(accept, "volume_id", volume, "id"),
					resource.TestCheckResourceAttrPair(accept, "id", offer, "id"),
					testAccCheckVolumeProject(t, volume, destID),
					testAccCheckTransferGone(t, offer),
				),
			},
			{
				// The refresh finds the transfer gone and the volume in the
				// second project: the offer is accepted and nothing is planned.
				Config: testAccVolumeTransferOfferConfig() + testAccVolumeTransferAcceptConfig(dest.Name),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(scope, "project_id", destID),
					resource.TestCheckResourceAttr(offer, "accepted", "true"),
				),
			},
		},
	})
}

// testAccVolumeTransferAcceptConfig accepts the offer as the same user, signed
// in to the second project through pcdconsumer. Only the project changes; the
// other credentials come from the OS_* environment. The auth scope lookup
// shows which project pcdconsumer signed in to.
func testAccVolumeTransferAcceptConfig(destProjectName string) string {
	return fmt.Sprintf(`
provider "pcd" {}

provider "pcdconsumer" {
  tenant_name = %q
}

data "pcd_identity_auth_scope" "consumer" {
  provider = pcdconsumer
  name     = "consumer"
}

resource "pcd_blockstorage_volume_transfer_accept" "test" {
  provider    = pcdconsumer
  transfer_id = pcd_blockstorage_volume_transfer.test.id
  auth_key    = pcd_blockstorage_volume_transfer.test.auth_key
}
`, destProjectName)
}

// testAccVolumeTransferConsumerFactories serves the provider twice: as pcd for
// the offering project, and as pcdconsumer, a second in-process server, for
// the receiving project. The test harness serves every configuration of one
// provider, the default and its aliases, from a single server, which keeps one
// configured client: the configuration set up last would supply the client
// for all of them. With a provider binary, Terraform starts one plugin process
// per configuration, so an alias works there.
func testAccVolumeTransferConsumerFactories() map[string]func() (tfprotov6.ProviderServer, error) {
	return map[string]func() (tfprotov6.ProviderServer, error){
		"pcd":         acctest.ProtoV6ProviderFactories["pcd"],
		"pcdconsumer": providerserver.NewProtocol6WithError(provider.New("test")()),
	}
}

// testAccCheckVolumeProject reads the volume's owning project with the
// test's (admin) credentials, which see every project's volumes.
func testAccCheckVolumeProject(t *testing.T, n, want string) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		rs := s.RootModule().Resources[n]
		if rs == nil {
			return fmt.Errorf("not found in state: %s", n)
		}
		client, err := acctest.LabConfig(t).BlockStorageV3Client()
		if err != nil {
			return err
		}
		vol, err := volumes.Get(context.Background(), client, rs.Primary.ID).Extract()
		if err != nil {
			return fmt.Errorf("reading volume %s: %w", rs.Primary.ID, err)
		}
		if vol.TenantID != want {
			return fmt.Errorf("volume %s belongs to project %q, want %q", rs.Primary.ID, vol.TenantID, want)
		}
		return nil
	}
}

// testAccCheckTransferGone confirms Cinder no longer reports the transfer.
func testAccCheckTransferGone(t *testing.T, n string) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		rs := s.RootModule().Resources[n]
		if rs == nil {
			return fmt.Errorf("not found in state: %s", n)
		}
		client, err := acctest.LabConfig(t).BlockStorageV3Client()
		if err != nil {
			return err
		}
		_, err = transfers.Get(context.Background(), client, rs.Primary.ID).Extract()
		if err == nil {
			return fmt.Errorf("transfer %s still exists after the accept", rs.Primary.ID)
		}
		if !gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
			return fmt.Errorf("reading transfer %s: %w", rs.Primary.ID, err)
		}
		return nil
	}
}
