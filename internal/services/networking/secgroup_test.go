// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package networking_test

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/extensions/security/groups"
	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/extensions/security/rules"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"

	"github.com/platform9/terraform-provider-pcd/internal/acctest"
)

func TestAccNetworkingSecgroupAndRule_basic(t *testing.T) {
	const sgName = "pcd_networking_secgroup.test"
	const ruleName = "pcd_networking_secgroup_rule.test"

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { acctest.PreCheck(t) },
		ProtoV6ProviderFactories: acctest.ProtoV6ProviderFactories,
		CheckDestroy: resource.ComposeAggregateTestCheckFunc(
			testAccCheckSecgroupRuleDestroy(t),
			testAccCheckSecgroupDestroy(t),
		),
		Steps: []resource.TestStep{
			{
				Config: testAccSecgroupConfig("tf-acc-sg", "initial"),
				Check: resource.ComposeAggregateTestCheckFunc(
					testAccCheckSecgroupExists(t, sgName),
					testAccCheckSecgroupRuleExists(t, ruleName),
					resource.TestCheckResourceAttr(sgName, "name", "tf-acc-sg"),
					resource.TestCheckResourceAttr(sgName, "description", "initial"),
					resource.TestCheckResourceAttr(ruleName, "port_range_min", "22"),
					resource.TestCheckResourceAttr(ruleName, "protocol", "tcp"),
					resource.TestCheckResourceAttr(ruleName, "remote_ip_prefix", "0.0.0.0/0"),
				),
			},
			{
				Config: testAccSecgroupConfig("tf-acc-sg", "updated"),
				Check:  resource.TestCheckResourceAttr(sgName, "description", "updated"),
			},
			{ResourceName: sgName, ImportState: true, ImportStateVerify: true, ImportStateVerifyIgnore: []string{"delete_default_rules"}},
			{ResourceName: ruleName, ImportState: true, ImportStateVerify: true},
		},
	})
}

func testAccSecgroupConfig(name, description string) string {
	return fmt.Sprintf(`
resource "pcd_networking_secgroup" "test" {
  name        = %q
  description = %q
}

resource "pcd_networking_secgroup_rule" "test" {
  direction         = "ingress"
  ethertype         = "IPv4"
  protocol          = "tcp"
  port_range_min    = 22
  port_range_max    = 22
  remote_ip_prefix  = "0.0.0.0/0"
  security_group_id = pcd_networking_secgroup.test.id
}
`, name, description)
}

func testAccCheckSecgroupExists(t *testing.T, n string) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		rs := s.RootModule().Resources[n]
		if rs == nil {
			return fmt.Errorf("not found in state: %s", n)
		}
		client, err := acctest.LabConfig(t).NetworkV2Client()
		if err != nil {
			return err
		}
		if _, err := groups.Get(context.Background(), client, rs.Primary.ID).Extract(); err != nil {
			return fmt.Errorf("security group %s not found: %w", rs.Primary.ID, err)
		}
		return nil
	}
}

func testAccCheckSecgroupRuleExists(t *testing.T, n string) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		rs := s.RootModule().Resources[n]
		if rs == nil {
			return fmt.Errorf("not found in state: %s", n)
		}
		client, err := acctest.LabConfig(t).NetworkV2Client()
		if err != nil {
			return err
		}
		if _, err := rules.Get(context.Background(), client, rs.Primary.ID).Extract(); err != nil {
			return fmt.Errorf("security group rule %s not found: %w", rs.Primary.ID, err)
		}
		return nil
	}
}

func testAccCheckSecgroupDestroy(t *testing.T) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		client, err := acctest.LabConfig(t).NetworkV2Client()
		if err != nil {
			return err
		}
		for _, rs := range s.RootModule().Resources {
			if rs.Type != "pcd_networking_secgroup" {
				continue
			}
			_, err := groups.Get(context.Background(), client, rs.Primary.ID).Extract()
			if err == nil {
				return fmt.Errorf("security group %s still exists", rs.Primary.ID)
			}
			if !gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
				return err
			}
		}
		return nil
	}
}

func testAccCheckSecgroupRuleDestroy(t *testing.T) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		client, err := acctest.LabConfig(t).NetworkV2Client()
		if err != nil {
			return err
		}
		for _, rs := range s.RootModule().Resources {
			if rs.Type != "pcd_networking_secgroup_rule" {
				continue
			}
			_, err := rules.Get(context.Background(), client, rs.Primary.ID).Extract()
			if err == nil {
				return fmt.Errorf("security group rule %s still exists", rs.Primary.ID)
			}
			if !gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
				return err
			}
		}
		return nil
	}
}
