// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package loadbalancer_test

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/loadbalancer/v2/loadbalancers"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"

	"github.com/platform9/terraform-provider-pcd/internal/acctest"
)

// TestAccLBLoadBalancer_tree builds a full Octavia tree (load balancer, listener,
// pool, member, and health monitor) on a network subnet, then updates the load
// balancer name in place and imports it.
func TestAccLBLoadBalancer_tree(t *testing.T) {
	const lbName = "pcd_lb_loadbalancer.test"

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { acctest.PreCheck(t) },
		ProtoV6ProviderFactories: acctest.ProtoV6ProviderFactories,
		CheckDestroy: resource.ComposeAggregateTestCheckFunc(
			testAccCheckLoadBalancerDestroy(t),
		),
		Steps: []resource.TestStep{
			{
				Config: testAccLBTreeConfig("tf-acc-lb"),
				Check: resource.ComposeAggregateTestCheckFunc(
					testAccCheckLoadBalancerExists(t, lbName),
					resource.TestCheckResourceAttr(lbName, "name", "tf-acc-lb"),
					resource.TestCheckResourceAttr(lbName, "provisioning_status", "ACTIVE"),
					resource.TestCheckResourceAttrSet(lbName, "vip_address"),
					resource.TestCheckResourceAttrSet(lbName, "vip_port_id"),
					resource.TestCheckResourceAttr(lbName, "loadbalancer_provider", "ovn"),
					resource.TestCheckResourceAttrPair("pcd_lb_listener.test", "loadbalancer_id", lbName, "id"),
					resource.TestCheckResourceAttr("pcd_lb_pool.test", "lb_method", "SOURCE_IP_PORT"),
					resource.TestCheckResourceAttr("pcd_lb_member.test", "protocol_port", "8080"),
					resource.TestCheckResourceAttr("pcd_lb_monitor.test", "type", "TCP"),
					resource.TestCheckResourceAttrPair("data.pcd_lb_loadbalancer.by_name", "id", lbName, "id"),
				),
			},
			{
				Config: testAccLBTreeConfig("tf-acc-lb-renamed"),
				Check:  resource.TestCheckResourceAttr(lbName, "name", "tf-acc-lb-renamed"),
			},
			{ResourceName: lbName, ImportState: true, ImportStateVerify: true},
		},
	})
}

func testAccLBTreeConfig(name string) string {
	return fmt.Sprintf(`
resource "pcd_networking_network" "test" {
  name = "tf-acc-lb-net"
}

resource "pcd_networking_subnet" "test" {
  name       = "tf-acc-lb-subnet"
  network_id = pcd_networking_network.test.id
  cidr       = "10.120.0.0/24"
}

resource "pcd_lb_loadbalancer" "test" {
  name                  = %q
  vip_subnet_id         = pcd_networking_subnet.test.id
  loadbalancer_provider = "ovn"
}

resource "pcd_lb_listener" "test" {
  name            = "tf-acc-lb-listener"
  loadbalancer_id = pcd_lb_loadbalancer.test.id
  protocol        = "TCP"
  protocol_port   = 80
}

resource "pcd_lb_pool" "test" {
  name        = "tf-acc-lb-pool"
  listener_id = pcd_lb_listener.test.id
  protocol    = "TCP"
  lb_method   = "SOURCE_IP_PORT"
}

resource "pcd_lb_member" "test" {
  pool_id       = pcd_lb_pool.test.id
  address       = "10.120.0.10"
  protocol_port = 8080
  subnet_id     = pcd_networking_subnet.test.id
}

resource "pcd_lb_monitor" "test" {
  pool_id     = pcd_lb_pool.test.id
  type        = "TCP"
  delay       = 10
  timeout     = 5
  max_retries = 3
}

data "pcd_lb_loadbalancer" "by_name" {
  name = pcd_lb_loadbalancer.test.name
}
`, name)
}

func testAccCheckLoadBalancerExists(t *testing.T, n string) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		rs, ok := s.RootModule().Resources[n]
		if !ok {
			return fmt.Errorf("not found in state: %s", n)
		}
		client, err := acctest.LabConfig(t).LoadBalancerV2Client()
		if err != nil {
			return err
		}
		if _, err := loadbalancers.Get(context.Background(), client, rs.Primary.ID).Extract(); err != nil {
			return fmt.Errorf("load balancer %s not found via API: %w", rs.Primary.ID, err)
		}
		return nil
	}
}

func testAccCheckLoadBalancerDestroy(t *testing.T) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		client, err := acctest.LabConfig(t).LoadBalancerV2Client()
		if err != nil {
			return err
		}
		for _, rs := range s.RootModule().Resources {
			if rs.Type != "pcd_lb_loadbalancer" {
				continue
			}
			_, err := loadbalancers.Get(context.Background(), client, rs.Primary.ID).Extract()
			if err == nil {
				return fmt.Errorf("load balancer %s still exists", rs.Primary.ID)
			}
			if !gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
				return fmt.Errorf("unexpected error checking load balancer %s: %w", rs.Primary.ID, err)
			}
		}
		return nil
	}
}
