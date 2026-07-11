// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package identity_test

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/identity/v3/applicationcredentials"
	"github.com/gophercloud/gophercloud/v2/openstack/identity/v3/tokens"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"

	"github.com/platform9/terraform-provider-pcd/internal/acctest"
)

func TestAccIdentityApplicationCredentialResource_basic(t *testing.T) {
	const resourceName = "pcd_identity_application_credential.test"
	name := "tf-acc-appcred"

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { acctest.PreCheck(t) },
		ProtoV6ProviderFactories: acctest.ProtoV6ProviderFactories,
		CheckDestroy:             testAccCheckAppCredDestroy(t),
		Steps: []resource.TestStep{
			{
				Config: fmt.Sprintf("resource \"pcd_identity_application_credential\" \"test\" {\n  name = %q\n}\n", name),
				Check: resource.ComposeAggregateTestCheckFunc(
					testAccCheckAppCredExists(t, resourceName),
					resource.TestCheckResourceAttr(resourceName, "name", name),
					resource.TestCheckResourceAttrSet(resourceName, "secret"),
					resource.TestCheckResourceAttrSet(resourceName, "project_id"),
				),
			},
			{
				ResourceName:            resourceName,
				ImportState:             true,
				ImportStateVerify:       true,
				ImportStateVerifyIgnore: []string{"secret", "expires_at"},
			},
		},
	})
}

func testAccCurrentUserID(client *gophercloud.ServiceClient) (string, error) {
	u, err := tokens.Get(context.Background(), client, client.Token()).ExtractUser()
	if err != nil {
		return "", err
	}
	return u.ID, nil
}

func testAccCheckAppCredExists(t *testing.T, n string) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		rs, ok := s.RootModule().Resources[n]
		if !ok {
			return fmt.Errorf("not found in state: %s", n)
		}
		client, err := acctest.LabConfig(t).IdentityV3Client()
		if err != nil {
			return err
		}
		uid, err := testAccCurrentUserID(client)
		if err != nil {
			return err
		}
		if _, err := applicationcredentials.Get(context.Background(), client, uid, rs.Primary.ID).Extract(); err != nil {
			return fmt.Errorf("application credential %s not found via API: %w", rs.Primary.ID, err)
		}
		return nil
	}
}

func testAccCheckAppCredDestroy(t *testing.T) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		client, err := acctest.LabConfig(t).IdentityV3Client()
		if err != nil {
			return err
		}
		uid, err := testAccCurrentUserID(client)
		if err != nil {
			return err
		}
		for _, rs := range s.RootModule().Resources {
			if rs.Type != "pcd_identity_application_credential" {
				continue
			}
			_, err := applicationcredentials.Get(context.Background(), client, uid, rs.Primary.ID).Extract()
			if err == nil {
				return fmt.Errorf("application credential %s still exists", rs.Primary.ID)
			}
			if !gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
				return fmt.Errorf("unexpected error checking application credential %s: %w", rs.Primary.ID, err)
			}
		}
		return nil
	}
}
