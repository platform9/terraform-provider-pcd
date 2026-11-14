// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package identity_test

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"testing"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/identity/v3/federation"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"

	"github.com/platform9/terraform-provider-pcd/internal/acctest"
)

// testAccSSOGate skips unless PCD_ACC_SSO is set: the federation and SSO
// tests write Keystone federation objects and, for the SSO configuration,
// restart Keystone.
func testAccSSOGate(t *testing.T) {
	t.Helper()
	if os.Getenv("PCD_ACC_SSO") == "" {
		t.Skip("PCD_ACC_SSO not set; skipping federation and SSO tests")
	}
}

// TestAccIdentityFederation_basic creates a mapping, an identity provider and
// its saml2 protocol, changes all three in place, replaces the identity
// provider (which deletes its protocols in Keystone) and checks that the
// protocol is created again with it, and imports them. The names
// are its own (never IDP1 or idp1_mapping), so a real SSO setup is untouched,
// and nothing here talks to hagrid or restarts Keystone.
func TestAccIdentityFederation_basic(t *testing.T) {
	const (
		mapping  = "pcd_identity_federation_mapping.first"
		idp      = "pcd_identity_federation_identity_provider.test"
		protocol = "pcd_identity_federation_protocol.test"
	)
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { acctest.PreCheck(t); testAccSSOGate(t) },
		ProtoV6ProviderFactories: acctest.ProtoV6ProviderFactories,
		CheckDestroy:             testAccCheckFederationDestroy(t),
		Steps: []resource.TestStep{
			{
				Config: testAccFederationConfig(false, "https://tf-acc.example.com/idp", true, "first", "first"),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(mapping, "id", "tf-acc-map"),
					resource.TestCheckResourceAttr(idp, "id", "tf-acc-idp"),
					resource.TestCheckResourceAttr(idp, "enabled", "true"),
					resource.TestCheckResourceAttr(idp, "remote_ids.#", "1"),
					resource.TestCheckResourceAttr(protocol, "id", "tf-acc-idp/saml2"),
					resource.TestCheckResourceAttr(protocol, "mapping_id", "tf-acc-map"),
					testAccCheckMappingExists(t, "tf-acc-map"),
				),
			},
			{
				Config: testAccFederationConfig(true, "https://tf-acc.example.com/idp-2", false, "second", "second"),
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
					plancheck.ExpectResourceAction(mapping, plancheck.ResourceActionUpdate),
					plancheck.ExpectResourceAction(idp, plancheck.ResourceActionUpdate),
					plancheck.ExpectResourceAction(protocol, plancheck.ResourceActionUpdate),
				}},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(idp, "enabled", "false"),
					resource.TestCheckResourceAttr(idp, "description", "second"),
					resource.TestCheckTypeSetElemAttr(idp, "remote_ids.*", "https://tf-acc.example.com/idp-2"),
					resource.TestCheckResourceAttr(protocol, "mapping_id", "tf-acc-map-2"),
				),
			},
			{
				// Keystone deletes the protocol with the identity provider; the
				// protocol takes the identity provider's id, which is unknown
				// during the replacement, so it is replaced too.
				Config: testAccFederationConfig(true, "https://tf-acc.example.com/idp-2", false, "second", "second"),
				Taint:  []string{idp},
				Check:  testAccCheckProtocolExists(t, "tf-acc-idp", "saml2", "tf-acc-map-2"),
			},
			{ResourceName: mapping, ImportState: true, ImportStateVerify: true},
			{ResourceName: idp, ImportState: true, ImportStateVerify: true},
			{ResourceName: protocol, ImportState: true, ImportStateVerify: true},
		},
	})
}

// testAccFederationConfig builds the federation objects. groupsRule adds a
// second remote condition to the first mapping's rule; protocolMapping picks
// which mapping the protocol applies.
func testAccFederationConfig(groupsRule bool, remoteID string, enabled bool, description, protocolMapping string) string {
	remote := `[{ type = "MAIL" }]`
	if groupsRule {
		remote = `[{ type = "MAIL" }, { type = "groups", any_one_of = ["tf-acc"] }]`
	}
	return fmt.Sprintf(`
resource "pcd_identity_group" "sso" {
  name = "tf-acc-sso-group"
}

resource "pcd_identity_federation_mapping" "first" {
  name = "tf-acc-map"
  rules = jsonencode([{
    local  = [{ user = { name = "{0}" }, group = { id = pcd_identity_group.sso.id } }]
    remote = %s
  }])
}

resource "pcd_identity_federation_mapping" "second" {
  name = "tf-acc-map-2"
  rules = jsonencode([{
    local  = [{ user = { name = "{0}" }, group = { id = pcd_identity_group.sso.id } }]
    remote = [{ type = "MAIL" }]
  }])
}

resource "pcd_identity_federation_identity_provider" "test" {
  name        = "tf-acc-idp"
  domain_id   = "default"
  remote_ids  = [%q]
  enabled     = %t
  description = %q
}

resource "pcd_identity_federation_protocol" "test" {
  identity_provider = pcd_identity_federation_identity_provider.test.id
  name              = "saml2"
  mapping_id        = pcd_identity_federation_mapping.%s.name
}
`, remote, remoteID, enabled, description, protocolMapping)
}

func testAccCheckMappingExists(t *testing.T, name string) resource.TestCheckFunc {
	return func(*terraform.State) error {
		client, err := acctest.LabConfig(t).IdentityV3Client()
		if err != nil {
			return err
		}
		if err := federation.GetMapping(context.Background(), client, name).Err; err != nil {
			return fmt.Errorf("mapping %s not found via API: %w", name, err)
		}
		return nil
	}
}

// testAccCheckProtocolExists checks out of band that Keystone has the
// protocol idp/name and that it applies mapping.
func testAccCheckProtocolExists(t *testing.T, idp, name, mapping string) resource.TestCheckFunc {
	return func(*terraform.State) error {
		client, err := acctest.LabConfig(t).IdentityV3Client()
		if err != nil {
			return err
		}
		var out struct {
			Protocol struct {
				MappingID string `json:"mapping_id"`
			} `json:"protocol"`
		}
		url := client.ServiceURL("OS-FEDERATION", "identity_providers", idp, "protocols", name)
		if _, err := client.Get(context.Background(), url, &out, nil); err != nil {
			return fmt.Errorf("protocol %s/%s not found via API: %w", idp, name, err)
		}
		if out.Protocol.MappingID != mapping {
			return fmt.Errorf("protocol %s/%s applies mapping %q, want %q", idp, name, out.Protocol.MappingID, mapping)
		}
		return nil
	}
}

func testAccCheckFederationDestroy(t *testing.T) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		client, err := acctest.LabConfig(t).IdentityV3Client()
		if err != nil {
			return err
		}
		for _, rs := range s.RootModule().Resources {
			var err error
			switch rs.Type {
			case "pcd_identity_federation_mapping":
				err = federation.GetMapping(context.Background(), client, rs.Primary.ID).Err
			case "pcd_identity_federation_identity_provider":
				_, err = client.Get(context.Background(), client.ServiceURL("OS-FEDERATION", "identity_providers", rs.Primary.ID), nil, nil)
			default:
				continue
			}
			if err == nil {
				return fmt.Errorf("%s %s still exists", rs.Type, rs.Primary.ID)
			}
			if !gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
				return fmt.Errorf("checking %s %s: %w", rs.Type, rs.Primary.ID, err)
			}
		}
		return nil
	}
}
