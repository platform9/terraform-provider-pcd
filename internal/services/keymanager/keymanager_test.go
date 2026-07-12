// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package keymanager_test

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/keymanager/v1/secrets"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"

	"github.com/platform9/terraform-provider-pcd/internal/acctest"
)

// TestAccKeyManagerSecretAndContainer_basic creates a secret with a payload, a
// generic container referencing it, and reads the secret back via the data source.
func TestAccKeyManagerSecretAndContainer_basic(t *testing.T) {
	const secretName = "pcd_keymanager_secret.test"
	const containerName = "pcd_keymanager_container.test"

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { acctest.PreCheck(t) },
		ProtoV6ProviderFactories: acctest.ProtoV6ProviderFactories,
		CheckDestroy:             testAccCheckSecretDestroy(t),
		Steps: []resource.TestStep{
			{
				Config: testAccKeyManagerConfig,
				Check: resource.ComposeAggregateTestCheckFunc(
					testAccCheckSecretExists(t, secretName),
					resource.TestCheckResourceAttr(secretName, "name", "tf-acc-secret"),
					resource.TestCheckResourceAttr(secretName, "status", "ACTIVE"),
					resource.TestCheckResourceAttrSet(secretName, "secret_ref"),
					resource.TestCheckResourceAttr(containerName, "type", "generic"),
					resource.TestCheckResourceAttr(containerName, "secret_refs.#", "1"),
					resource.TestCheckResourceAttrSet(containerName, "container_ref"),
					resource.TestCheckResourceAttrPair("data.pcd_keymanager_secret.by_name", "id", secretName, "id"),
					resource.TestCheckResourceAttr("data.pcd_keymanager_secret.by_name", "payload", "s3cr3t-passphrase"),
				),
			},
			{
				ResourceName:            secretName,
				ImportState:             true,
				ImportStateVerify:       true,
				ImportStateVerifyIgnore: []string{"payload", "payload_content_type", "payload_content_encoding"},
			},
			{ResourceName: containerName, ImportState: true, ImportStateVerify: true},
		},
	})
}

const testAccKeyManagerConfig = `
resource "pcd_keymanager_secret" "test" {
  name                 = "tf-acc-secret"
  secret_type          = "passphrase"
  payload              = "s3cr3t-passphrase"
  payload_content_type = "text/plain"
}

resource "pcd_keymanager_container" "test" {
  name = "tf-acc-container"
  type = "generic"

  secret_refs {
    name       = "passphrase"
    secret_ref = pcd_keymanager_secret.test.secret_ref
  }
}

data "pcd_keymanager_secret" "by_name" {
  name                 = pcd_keymanager_secret.test.name
  payload_content_type = "text/plain"
}
`

func testAccCheckSecretExists(t *testing.T, n string) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		rs, ok := s.RootModule().Resources[n]
		if !ok {
			return fmt.Errorf("not found in state: %s", n)
		}
		client, err := acctest.LabConfig(t).KeyManagerV1Client()
		if err != nil {
			return err
		}
		if _, err := secrets.Get(context.Background(), client, rs.Primary.ID).Extract(); err != nil {
			return fmt.Errorf("secret %s not found via API: %w", rs.Primary.ID, err)
		}
		return nil
	}
}

func testAccCheckSecretDestroy(t *testing.T) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		client, err := acctest.LabConfig(t).KeyManagerV1Client()
		if err != nil {
			return err
		}
		for _, rs := range s.RootModule().Resources {
			if rs.Type != "pcd_keymanager_secret" {
				continue
			}
			_, err := secrets.Get(context.Background(), client, rs.Primary.ID).Extract()
			if err == nil {
				return fmt.Errorf("secret %s still exists", rs.Primary.ID)
			}
			if !gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
				return fmt.Errorf("unexpected error checking secret %s: %w", rs.Primary.ID, err)
			}
		}
		return nil
	}
}
