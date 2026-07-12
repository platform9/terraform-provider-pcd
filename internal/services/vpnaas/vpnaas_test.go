// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package vpnaas_test

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/extensions/vpnaas/services"
	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/extensions/vpnaas/siteconnections"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"

	"github.com/platform9/terraform-provider-pcd/internal/acctest"
)

// TestAccVPNaaS_tree builds a full VPNaaS stack (service, IKE/IPsec policies,
// local and peer endpoint groups, and a site connection) on a router/subnet,
// verifies the service and connection against the API, renames the service in
// place, and imports the service.
func TestAccVPNaaS_tree(t *testing.T) {
	const svcName = "pcd_vpnaas_service.test"

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { acctest.PreCheck(t) },
		ProtoV6ProviderFactories: acctest.ProtoV6ProviderFactories,
		CheckDestroy: resource.ComposeAggregateTestCheckFunc(
			testAccCheckVPNServiceDestroy(t),
		),
		Steps: []resource.TestStep{
			{
				Config: testAccVPNaaSTreeConfig("tf-acc-vpn"),
				Check: resource.ComposeAggregateTestCheckFunc(
					testAccCheckVPNServiceExists(t, svcName),
					resource.TestCheckResourceAttr(svcName, "name", "tf-acc-vpn"),
					resource.TestCheckResourceAttrSet(svcName, "status"),
					resource.TestCheckResourceAttr("pcd_vpnaas_ike_policy.test", "auth_algorithm", "sha256"),
					resource.TestCheckResourceAttr("pcd_vpnaas_ipsec_policy.test", "encryption_algorithm", "aes-256"),
					resource.TestCheckResourceAttr("pcd_vpnaas_endpoint_group.peer", "type", "cidr"),
					testAccCheckSiteConnectionExists(t, "pcd_vpnaas_site_connection.test"),
					resource.TestCheckResourceAttrPair("pcd_vpnaas_site_connection.test", "vpn_service_id", svcName, "id"),
				),
			},
			{
				Config: testAccVPNaaSTreeConfig("tf-acc-vpn-renamed"),
				Check:  resource.TestCheckResourceAttr(svcName, "name", "tf-acc-vpn-renamed"),
			},
			{ResourceName: svcName, ImportState: true, ImportStateVerify: true},
		},
	})
}

func testAccVPNaaSTreeConfig(svcName string) string {
	return fmt.Sprintf(`
resource "pcd_networking_network" "test" {
  name = "tf-acc-vpn-net"
}

resource "pcd_networking_subnet" "test" {
  name       = "tf-acc-vpn-subnet"
  network_id = pcd_networking_network.test.id
  cidr       = "10.130.0.0/24"
}

resource "pcd_networking_router" "test" {
  name = "tf-acc-vpn-router"
}

resource "pcd_vpnaas_service" "test" {
  name      = %q
  router_id = pcd_networking_router.test.id
}

resource "pcd_vpnaas_ike_policy" "test" {
  name           = "tf-acc-ike"
  auth_algorithm = "sha256"
}

resource "pcd_vpnaas_ipsec_policy" "test" {
  name                 = "tf-acc-ipsec"
  encryption_algorithm = "aes-256"
}

resource "pcd_vpnaas_endpoint_group" "local" {
  name      = "tf-acc-local-eps"
  type      = "subnet"
  endpoints = [pcd_networking_subnet.test.id]
}

resource "pcd_vpnaas_endpoint_group" "peer" {
  name      = "tf-acc-peer-eps"
  type      = "cidr"
  endpoints = ["10.140.0.0/24"]
}

resource "pcd_vpnaas_site_connection" "test" {
  name              = "tf-acc-conn"
  ike_policy_id     = pcd_vpnaas_ike_policy.test.id
  ipsec_policy_id   = pcd_vpnaas_ipsec_policy.test.id
  vpn_service_id    = pcd_vpnaas_service.test.id
  local_ep_group_id = pcd_vpnaas_endpoint_group.local.id
  peer_ep_group_id  = pcd_vpnaas_endpoint_group.peer.id
  peer_address      = "172.24.4.233"
  peer_id           = "172.24.4.233"
  psk               = "tf-acc-secret"
}
`, svcName)
}

func testAccCheckVPNServiceExists(t *testing.T, n string) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		rs, ok := s.RootModule().Resources[n]
		if !ok {
			return fmt.Errorf("not found in state: %s", n)
		}
		client, err := acctest.LabConfig(t).NetworkV2Client()
		if err != nil {
			return err
		}
		if _, err := services.Get(context.Background(), client, rs.Primary.ID).Extract(); err != nil {
			return fmt.Errorf("VPN service %s not found via API: %w", rs.Primary.ID, err)
		}
		return nil
	}
}

func testAccCheckSiteConnectionExists(t *testing.T, n string) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		rs, ok := s.RootModule().Resources[n]
		if !ok {
			return fmt.Errorf("not found in state: %s", n)
		}
		client, err := acctest.LabConfig(t).NetworkV2Client()
		if err != nil {
			return err
		}
		if _, err := siteconnections.Get(context.Background(), client, rs.Primary.ID).Extract(); err != nil {
			return fmt.Errorf("site connection %s not found via API: %w", rs.Primary.ID, err)
		}
		return nil
	}
}

func testAccCheckVPNServiceDestroy(t *testing.T) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		client, err := acctest.LabConfig(t).NetworkV2Client()
		if err != nil {
			return err
		}
		for _, rs := range s.RootModule().Resources {
			if rs.Type != "pcd_vpnaas_service" {
				continue
			}
			_, err := services.Get(context.Background(), client, rs.Primary.ID).Extract()
			if err == nil {
				return fmt.Errorf("VPN service %s still exists", rs.Primary.ID)
			}
			if !gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
				return fmt.Errorf("unexpected error checking VPN service %s: %w", rs.Primary.ID, err)
			}
		}
		return nil
	}
}
