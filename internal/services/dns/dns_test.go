// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package dns_test

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/dns/v2/zones"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"

	"github.com/platform9/terraform-provider-pcd/internal/acctest"
)

// TestAccDNSZoneAndRecordSet_basic creates a PRIMARY zone and an A recordset in
// it, updates the recordset, and imports both.
func TestAccDNSZoneAndRecordSet_basic(t *testing.T) {
	const zoneName = "pcd_dns_zone.test"
	const rrName = "pcd_dns_recordset.test"

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { acctest.PreCheck(t) },
		ProtoV6ProviderFactories: acctest.ProtoV6ProviderFactories,
		CheckDestroy:             testAccCheckZoneDestroy(t),
		Steps: []resource.TestStep{
			{
				Config: testAccDNSConfig(`["10.1.0.1", "10.1.0.2"]`),
				Check: resource.ComposeAggregateTestCheckFunc(
					testAccCheckZoneExists(t, zoneName),
					resource.TestCheckResourceAttr(zoneName, "name", "tf-acc-example.com."),
					resource.TestCheckResourceAttr(zoneName, "status", "ACTIVE"),
					resource.TestCheckResourceAttrSet(zoneName, "serial"),
					resource.TestCheckResourceAttr(rrName, "type", "A"),
					resource.TestCheckResourceAttr(rrName, "records.#", "2"),
					resource.TestCheckResourceAttrPair(rrName, "zone_id", zoneName, "id"),
					resource.TestCheckResourceAttrPair("data.pcd_dns_zone.by_name", "id", zoneName, "id"),
				),
			},
			{
				Config: testAccDNSConfig(`["10.1.0.3"]`),
				Check:  resource.TestCheckResourceAttr(rrName, "records.#", "1"),
			},
			{ResourceName: zoneName, ImportState: true, ImportStateVerify: true},
			{
				ResourceName:      rrName,
				ImportState:       true,
				ImportStateVerify: true,
				ImportStateIdFunc: func(s *terraform.State) (string, error) {
					rs := s.RootModule().Resources[rrName]
					if rs == nil {
						return "", fmt.Errorf("not found in state: %s", rrName)
					}
					return rs.Primary.Attributes["zone_id"] + "/" + rs.Primary.ID, nil
				},
			},
		},
	})
}

func testAccDNSConfig(records string) string {
	return fmt.Sprintf(`
resource "pcd_dns_zone" "test" {
  name  = "tf-acc-example.com."
  email = "admin@tf-acc-example.com"
  ttl   = 3600
}

resource "pcd_dns_recordset" "test" {
  zone_id = pcd_dns_zone.test.id
  name    = "www.tf-acc-example.com."
  type    = "A"
  ttl     = 300
  records = %s
}

data "pcd_dns_zone" "by_name" {
  name = pcd_dns_zone.test.name
}
`, records)
}

func testAccCheckZoneExists(t *testing.T, n string) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		rs, ok := s.RootModule().Resources[n]
		if !ok {
			return fmt.Errorf("not found in state: %s", n)
		}
		client, err := acctest.LabConfig(t).DNSV2Client()
		if err != nil {
			return err
		}
		if _, err := zones.Get(context.Background(), client, rs.Primary.ID).Extract(); err != nil {
			return fmt.Errorf("zone %s not found via API: %w", rs.Primary.ID, err)
		}
		return nil
	}
}

func testAccCheckZoneDestroy(t *testing.T) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		client, err := acctest.LabConfig(t).DNSV2Client()
		if err != nil {
			return err
		}
		for _, rs := range s.RootModule().Resources {
			if rs.Type != "pcd_dns_zone" {
				continue
			}
			_, err := zones.Get(context.Background(), client, rs.Primary.ID).Extract()
			if err == nil {
				return fmt.Errorf("zone %s still exists", rs.Primary.ID)
			}
			if !gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
				return fmt.Errorf("unexpected error checking zone %s: %w", rs.Primary.ID, err)
			}
		}
		return nil
	}
}
