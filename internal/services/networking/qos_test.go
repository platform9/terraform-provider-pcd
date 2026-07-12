// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package networking_test

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/extensions/qos/policies"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"

	"github.com/platform9/terraform-provider-pcd/internal/acctest"
)

// TestAccNetworkingQoS_tree builds a QoS policy with all three rule types
// (bandwidth-limit, DSCP-marking, minimum-bandwidth), reads it back through the
// data source, then updates the policy name and the bandwidth-limit rate in
// place and imports the policy.
func TestAccNetworkingQoS_tree(t *testing.T) {
	const policyName = "pcd_networking_qos_policy.test"

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { acctest.PreCheck(t) },
		ProtoV6ProviderFactories: acctest.ProtoV6ProviderFactories,
		CheckDestroy: resource.ComposeAggregateTestCheckFunc(
			testAccCheckQoSPolicyDestroy(t),
		),
		Steps: []resource.TestStep{
			{
				Config: testAccQoSTreeConfig("tf-acc-qos", 3000),
				Check: resource.ComposeAggregateTestCheckFunc(
					testAccCheckQoSPolicyExists(t, policyName),
					resource.TestCheckResourceAttr(policyName, "name", "tf-acc-qos"),
					resource.TestCheckResourceAttr(policyName, "shared", "true"),
					resource.TestCheckResourceAttr("pcd_networking_qos_bandwidth_limit_rule.test", "max_kbps", "3000"),
					resource.TestCheckResourceAttr("pcd_networking_qos_dscp_marking_rule.test", "dscp_mark", "26"),
					resource.TestCheckResourceAttr("pcd_networking_qos_minimum_bandwidth_rule.test", "min_kbps", "1000"),
					resource.TestCheckResourceAttrPair("pcd_networking_qos_bandwidth_limit_rule.test", "qos_policy_id", policyName, "id"),
					resource.TestCheckResourceAttrPair("data.pcd_networking_qos_policy.by_name", "id", policyName, "id"),
				),
			},
			{
				Config: testAccQoSTreeConfig("tf-acc-qos-renamed", 5000),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(policyName, "name", "tf-acc-qos-renamed"),
					resource.TestCheckResourceAttr("pcd_networking_qos_bandwidth_limit_rule.test", "max_kbps", "5000"),
				),
			},
			{ResourceName: policyName, ImportState: true, ImportStateVerify: true},
		},
	})
}

func testAccQoSTreeConfig(name string, maxKBps int) string {
	return fmt.Sprintf(`
resource "pcd_networking_qos_policy" "test" {
  name   = %q
  shared = true
}

resource "pcd_networking_qos_bandwidth_limit_rule" "test" {
  qos_policy_id  = pcd_networking_qos_policy.test.id
  max_kbps       = %d
  max_burst_kbps = 300
}

resource "pcd_networking_qos_dscp_marking_rule" "test" {
  qos_policy_id = pcd_networking_qos_policy.test.id
  dscp_mark     = 26
}

resource "pcd_networking_qos_minimum_bandwidth_rule" "test" {
  qos_policy_id = pcd_networking_qos_policy.test.id
  min_kbps      = 1000
}

data "pcd_networking_qos_policy" "by_name" {
  name = pcd_networking_qos_policy.test.name
}
`, name, maxKBps)
}

func testAccCheckQoSPolicyExists(t *testing.T, n string) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		rs, ok := s.RootModule().Resources[n]
		if !ok {
			return fmt.Errorf("not found in state: %s", n)
		}
		client, err := acctest.LabConfig(t).NetworkV2Client()
		if err != nil {
			return err
		}
		if _, err := policies.Get(context.Background(), client, rs.Primary.ID).Extract(); err != nil {
			return fmt.Errorf("qos policy %s not found via API: %w", rs.Primary.ID, err)
		}
		return nil
	}
}

func testAccCheckQoSPolicyDestroy(t *testing.T) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		client, err := acctest.LabConfig(t).NetworkV2Client()
		if err != nil {
			return err
		}
		for _, rs := range s.RootModule().Resources {
			if rs.Type != "pcd_networking_qos_policy" {
				continue
			}
			_, err := policies.Get(context.Background(), client, rs.Primary.ID).Extract()
			if err == nil {
				return fmt.Errorf("qos policy %s still exists", rs.Primary.ID)
			}
			if !gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
				return fmt.Errorf("unexpected error checking qos policy %s: %w", rs.Primary.ID, err)
			}
		}
		return nil
	}
}
