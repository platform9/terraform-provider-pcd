// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package compute_test

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"slices"
	"strconv"
	"testing"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/aggregates"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"

	"github.com/platform9/terraform-provider-pcd/internal/acctest"
	"github.com/platform9/terraform-provider-pcd/internal/clients"
)

// TestAccComputeAggregate_basic creates an aggregate with no hosts and no
// zone, renames it, changes its metadata, reads it through the data source,
// and imports it. It touches no hypervisor, but aggregates need an admin
// token, so it is opt-in like every aggregate test: PCD_ACC_ADMIN_COMPUTE=1.
func TestAccComputeAggregate_basic(t *testing.T) {
	if os.Getenv("PCD_ACC_ADMIN_COMPUTE") == "" {
		t.Skip("PCD_ACC_ADMIN_COMPUTE not set; aggregates need an admin token")
	}
	const rn = "pcd_compute_aggregate.test"
	var id string
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { acctest.PreCheck(t) },
		ProtoV6ProviderFactories: acctest.ProtoV6ProviderFactories,
		CheckDestroy:             testAccCheckAggregateDestroy(t),
		Steps: []resource.TestStep{
			{
				Config: testAccAggregateConfig("tf-acc-agg", `{ tf_acc = "1" }`),
				Check: resource.ComposeAggregateTestCheckFunc(
					testAccCaptureID(rn, &id),
					testAccCheckAggregateRemote(t, rn, "tf-acc-agg", "", nil, map[string]string{"tf_acc": "1"}),
					resource.TestCheckResourceAttr(rn, "metadata.%", "1"),
					resource.TestCheckResourceAttr(rn, "hosts.#", "0"),
					resource.TestCheckNoResourceAttr(rn, "zone"),
					resource.TestCheckResourceAttrPair("data.pcd_compute_aggregate.test", "id", rn, "id"),
				),
			},
			{
				Config: testAccAggregateConfig("tf-acc-agg-renamed", `{ tf_acc = "2", tf_acc_extra = "x" }`),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(rn, plancheck.ResourceActionUpdate)},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrPtr(rn, "id", &id),
					testAccCheckAggregateRemote(t, rn, "tf-acc-agg-renamed", "", nil, map[string]string{"tf_acc": "2", "tf_acc_extra": "x"}),
				),
			},
			{
				// Dropping a key deletes it in Nova (set_metadata with null).
				Config: testAccAggregateConfig("tf-acc-agg-renamed", `{ tf_acc = "2" }`),
				Check:  testAccCheckAggregateRemote(t, rn, "tf-acc-agg-renamed", "", nil, map[string]string{"tf_acc": "2"}),
			},
			{ResourceName: rn, ImportState: true, ImportStateVerify: true},
		},
	})
}

// TestAccComputeAggregate_hosts puts a hypervisor in an aggregate whose zone
// is the host's cluster, removes it, adds it back, and lets destroy strip it.
// It changes what the scheduler sees for that host, so it is opt-in:
// PCD_ACC_ADMIN_COMPUTE=1, PCD_ACC_HOST_ID (a hypervisor's resmgr UUID) and
// PCD_ACC_CLUSTER_NAME (that hypervisor's cluster). Do not run it next to
// TestAccComputeAggregate_failureDomainPair: both put the host in a user
// aggregate. Metadata keys stay inert (tf_acc_*): PCD's aggregate filters
// compare metadata with flavor extra specs, image properties and projects.
func TestAccComputeAggregate_hosts(t *testing.T) {
	hostID, cluster := testAccAdminComputeHost(t)
	const rn = "pcd_compute_aggregate.test"
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { acctest.PreCheck(t) },
		ProtoV6ProviderFactories: acctest.ProtoV6ProviderFactories,
		CheckDestroy:             testAccCheckAggregateDestroy(t),
		Steps: []resource.TestStep{
			{
				Config: testAccAggregateHostsConfig(cluster, fmt.Sprintf("[%q]", hostID)),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(rn, "zone", cluster),
					resource.TestCheckResourceAttr(rn, "hosts.#", "1"),
					testAccCheckAggregateRemote(t, rn, "tf-acc-agg-hosts", cluster, []string{hostID}, map[string]string{"tf_acc": "1"}),
				),
			},
			{
				Config: testAccAggregateHostsConfig(cluster, "[]"),
				Check:  testAccCheckAggregateRemote(t, rn, "tf-acc-agg-hosts", cluster, []string{}, map[string]string{"tf_acc": "1"}),
			},
			{
				Config: testAccAggregateHostsConfig(cluster, fmt.Sprintf("[%q]", hostID)),
				Check:  testAccCheckAggregateRemote(t, rn, "tf-acc-agg-hosts", cluster, []string{hostID}, map[string]string{"tf_acc": "1"}),
			},
			{ResourceName: rn, ImportState: true, ImportStateVerify: true},
		},
	})
}

// testAccAdminComputeHost gates the tests that change scheduling for a real
// hypervisor and returns the host's resmgr UUID and cluster.
func testAccAdminComputeHost(t *testing.T) (hostID, cluster string) {
	t.Helper()
	if os.Getenv("PCD_ACC_ADMIN_COMPUTE") == "" {
		t.Skip("PCD_ACC_ADMIN_COMPUTE not set; skipping a test that changes scheduling on a shared hypervisor")
	}
	hostID, cluster = os.Getenv("PCD_ACC_HOST_ID"), os.Getenv("PCD_ACC_CLUSTER_NAME")
	if hostID == "" || cluster == "" {
		t.Skip("PCD_ACC_HOST_ID and PCD_ACC_CLUSTER_NAME must name a hypervisor and its cluster")
	}
	return hostID, cluster
}

func testAccAggregateConfig(name, metadata string) string {
	return fmt.Sprintf(`
resource "pcd_compute_aggregate" "test" {
  name     = %q
  metadata = %s
}

data "pcd_compute_aggregate" "test" {
  name = pcd_compute_aggregate.test.name
}
`, name, metadata)
}

func testAccAggregateHostsConfig(cluster, hosts string) string {
	return fmt.Sprintf(`
resource "pcd_compute_aggregate" "test" {
  name     = "tf-acc-agg-hosts"
  zone     = %q
  hosts    = %s
  metadata = { tf_acc = "1" }
}
`, cluster, hosts)
}

// testAccCheckAggregateRemote reads the aggregate from Nova and compares it
// with what the step configured. A nil hosts skips the host comparison.
func testAccCheckAggregateRemote(t *testing.T, n, name, zone string, hosts []string, meta map[string]string) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		rs := s.RootModule().Resources[n]
		if rs == nil {
			return fmt.Errorf("not found in state: %s", n)
		}
		id, err := strconv.Atoi(rs.Primary.ID)
		if err != nil {
			return err
		}
		client, err := acctest.LabConfig(t).ComputeV2Client()
		if err != nil {
			return err
		}
		agg, err := clients.RequireObject(aggregates.Get(context.Background(), client, id).Extract())
		if err != nil {
			return fmt.Errorf("aggregate %d: %w", id, err)
		}
		if agg.Name != name || agg.AvailabilityZone != zone {
			return fmt.Errorf("aggregate %d is %q in zone %q, want %q in zone %q", id, agg.Name, agg.AvailabilityZone, name, zone)
		}
		if hosts != nil {
			got := slices.Clone(agg.Hosts)
			slices.Sort(got)
			want := slices.Clone(hosts)
			slices.Sort(want)
			if !slices.Equal(got, want) {
				return fmt.Errorf("aggregate %d hosts = %v, want %v", id, got, want)
			}
		}
		for k, v := range meta {
			if agg.Metadata[k] != v {
				return fmt.Errorf("aggregate %d metadata %s = %q, want %q (all: %v)", id, k, agg.Metadata[k], v, agg.Metadata)
			}
		}
		for k := range agg.Metadata {
			if _, ok := meta[k]; !ok && k != "availability_zone" {
				return fmt.Errorf("aggregate %d still has metadata key %s", id, k)
			}
		}
		return nil
	}
}

func testAccCheckAggregateDestroy(t *testing.T) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		client, err := acctest.LabConfig(t).ComputeV2Client()
		if err != nil {
			return err
		}
		for _, rs := range s.RootModule().Resources {
			if rs.Type != "pcd_compute_aggregate" {
				continue
			}
			id, err := strconv.Atoi(rs.Primary.ID)
			if err != nil {
				return err
			}
			_, err = aggregates.Get(context.Background(), client, id).Extract()
			if err == nil {
				return fmt.Errorf("aggregate %d still exists", id)
			}
			if !gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
				return err
			}
		}
		return nil
	}
}

// TestAccComputeAggregate_failureDomainPair builds the pair of aggregates the
// PCD UI calls a stretched cluster (the Failure domains guide's recipe): both
// in the host's cluster zone, tagged pf9_failure_domain with labels A and B,
// the host in A and B empty (with one hypervisor there is nothing for B; the UI
// itself wants a host in each). Destroy must leave the host in its cluster's
// own aggregate. Opt-in like TestAccComputeAggregate_hosts, and never next to
// it; run it only on a cluster without a failure-domain pair of its own.
func TestAccComputeAggregate_failureDomainPair(t *testing.T) {
	hostID, cluster := testAccAdminComputeHost(t)
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { acctest.PreCheck(t) },
		ProtoV6ProviderFactories: acctest.ProtoV6ProviderFactories,
		CheckDestroy: resource.ComposeAggregateTestCheckFunc(
			testAccCheckAggregateDestroy(t),
			testAccCheckHostInClusterAggregate(t, cluster, hostID),
		),
		Steps: []resource.TestStep{{
			Config: testAccFailureDomainPairConfig(cluster, hostID),
			Check: resource.ComposeAggregateTestCheckFunc(
				testAccCheckAggregateRemote(t, "pcd_compute_aggregate.fd_a", "tf-acc-fd-a", cluster, []string{hostID},
					map[string]string{"pf9_failure_domain": "true", "pf9_fd_label": "A"}),
				testAccCheckAggregateRemote(t, "pcd_compute_aggregate.fd_b", "tf-acc-fd-b", cluster, []string{},
					map[string]string{"pf9_failure_domain": "true", "pf9_fd_label": "B"}),
			),
		}},
	})
}

func testAccFailureDomainPairConfig(cluster, hostID string) string {
	return fmt.Sprintf(`
resource "pcd_compute_aggregate" "fd_a" {
  name     = "tf-acc-fd-a"
  zone     = %[1]q
  hosts    = [%[2]q]
  metadata = { pf9_failure_domain = "true", pf9_fd_label = "A" }
}

resource "pcd_compute_aggregate" "fd_b" {
  name     = "tf-acc-fd-b"
  zone     = %[1]q
  metadata = { pf9_failure_domain = "true", pf9_fd_label = "B" }
}
`, cluster, hostID)
}

// testAccCheckHostInClusterAggregate confirms the host is still in the
// aggregate PCD keeps for its cluster, which is named after the cluster.
func testAccCheckHostInClusterAggregate(t *testing.T, cluster, hostID string) resource.TestCheckFunc {
	return func(*terraform.State) error {
		client, err := acctest.LabConfig(t).ComputeV2Client()
		if err != nil {
			return err
		}
		pages, err := aggregates.List(client).AllPages(context.Background())
		if err != nil {
			return err
		}
		all, err := aggregates.ExtractAggregates(pages)
		if err != nil {
			return err
		}
		for _, a := range all {
			if a.Name == cluster {
				if slices.Contains(a.Hosts, hostID) {
					return nil
				}
				return fmt.Errorf("host %s left cluster aggregate %s (hosts %v)", hostID, cluster, a.Hosts)
			}
		}
		return fmt.Errorf("no aggregate named %s", cluster)
	}
}
