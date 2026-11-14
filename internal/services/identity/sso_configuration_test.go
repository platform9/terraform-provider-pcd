// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package identity_test

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"

	"github.com/platform9/terraform-provider-pcd/internal/acctest"
)

// TestAccIdentitySSOConfiguration_basic creates, updates, imports and
// destroys an SSO configuration under its own name, tf-acc-sso, so IDP1 is
// untouched. The create, the update (attribute_map_xml only) and the destroy
// each restart Keystone for a minute or more: run it alone (-parallel 1), on
// a deployment nobody else is using. It needs PCD_ACC_SSO_METADATA_URL, the
// metadata URL of a SAML identity provider that PCD can download, and
// PCD_ACC_SSO_ENTITY_ID, that provider's entity ID.
func TestAccIdentitySSOConfiguration_basic(t *testing.T) {
	const rn = "pcd_identity_sso_configuration.test"
	metadataURL, entityID := os.Getenv("PCD_ACC_SSO_METADATA_URL"), os.Getenv("PCD_ACC_SSO_ENTITY_ID")
	resource.Test(t, resource.TestCase{
		PreCheck: func() {
			acctest.PreCheck(t)
			testAccSSOGate(t)
			if metadataURL == "" || entityID == "" {
				t.Skip("PCD_ACC_SSO_METADATA_URL or PCD_ACC_SSO_ENTITY_ID not set; skipping the SSO configuration test (it restarts Keystone)")
			}
		},
		ProtoV6ProviderFactories: acctest.ProtoV6ProviderFactories,
		CheckDestroy:             testAccCheckSSOConfigurationDestroy(t),
		Steps: []resource.TestStep{
			{
				Config: testAccSSOConfigurationConfig(entityID, metadataURL, "mail"),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(rn, "id", "tf-acc-sso"),
					resource.TestCheckResourceAttr(rn, "login_url", "/sso/tf-acc-sso/Shibboleth.sso/Login"),
					resource.TestCheckResourceAttrWith(rn, "acs_url", func(v string) error {
						if !strings.HasSuffix(v, "/sso/tf-acc-sso/Shibboleth.sso/SAML2/POST") {
							return fmt.Errorf("acs_url = %s", v)
						}
						return nil
					}),
					resource.TestCheckResourceAttrWith(rn, "sp_entity_id", func(v string) error {
						if !strings.HasSuffix(v, "/keystone") {
							return fmt.Errorf("sp_entity_id = %s", v)
						}
						return nil
					}),
					testAccCheckSSOConfigurationInHagrid(t, "tf-acc-sso", entityID),
				),
			},
			{
				Config: testAccSSOConfigurationConfig(entityID, metadataURL, "email"),
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
					plancheck.ExpectResourceAction(rn, plancheck.ResourceActionUpdate),
				}},
				Check: resource.TestCheckResourceAttrWith(rn, "attribute_map_xml", func(v string) error {
					if !strings.Contains(v, `id="email"`) {
						return fmt.Errorf("attribute_map_xml = %s", v)
					}
					return nil
				}),
			},
			{ResourceName: rn, ImportState: true, ImportStateVerify: true},
		},
	})
}

func testAccSSOConfigurationConfig(entityID, metadataURL, mailID string) string {
	return fmt.Sprintf(`
resource "pcd_identity_sso_configuration" "test" {
  identity_provider = "tf-acc-sso"
  sso_provider      = "tf-acc"
  entity_id         = %q
  metadata_url      = %q
  attribute_map_xml = <<-EOT
    <Attributes xmlns="urn:mace:shibboleth:2.0:attribute-map" xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance">
      <Attribute name="urn:oid:0.9.2342.19200300.100.1.3" id="%s"/>
    </Attributes>
  EOT
}
`, entityID, metadataURL, mailID)
}

func testAccHagridGet(t *testing.T, idp string, out any) error {
	t.Helper()
	client, err := acctest.LabConfig(t).HagridClient()
	if err != nil {
		return err
	}
	_, err = client.Get(context.Background(), client.ServiceURL("ssoconfig", idp), out, &gophercloud.RequestOpts{OkCodes: []int{200}})
	return err
}

func testAccCheckSSOConfigurationInHagrid(t *testing.T, idp, entityID string) resource.TestCheckFunc {
	return func(*terraform.State) error {
		var got struct {
			EntityID string `json:"entity_id"`
		}
		if err := testAccHagridGet(t, idp, &got); err != nil {
			return fmt.Errorf("reading the SSO configuration from hagrid: %w", err)
		}
		if got.EntityID != entityID {
			return fmt.Errorf("hagrid holds entity_id %q, want %q", got.EntityID, entityID)
		}
		return nil
	}
}

// testAccCheckSSOConfigurationDestroy expects hagrid's "nothing configured":
// a 404, or, in current PCD releases, a 500 whose message says "no sso entry
// for domain".
func testAccCheckSSOConfigurationDestroy(t *testing.T) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		for _, rs := range s.RootModule().Resources {
			if rs.Type != "pcd_identity_sso_configuration" {
				continue
			}
			var got map[string]any
			err := testAccHagridGet(t, rs.Primary.ID, &got)
			if err == nil {
				return fmt.Errorf("SSO configuration %s still exists", rs.Primary.ID)
			}
			notConfigured := gophercloud.ResponseCodeIs(err, http.StatusNotFound) ||
				(gophercloud.ResponseCodeIs(err, http.StatusInternalServerError) && strings.Contains(err.Error(), "no sso entry for domain"))
			if !notConfigured {
				return fmt.Errorf("checking SSO configuration %s: %w", rs.Primary.ID, err)
			}
		}
		return nil
	}
}
