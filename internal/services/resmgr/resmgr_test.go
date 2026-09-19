// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package resmgr_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"

	"github.com/platform9/terraform-provider-pcd/internal/acctest"
)

// TestAccResmgrBlueprintDataSource reads an existing blueprint by name. Set
// PCD_ACC_BLUEPRINT_NAME to an existing blueprint to run it (read-only).
func TestAccResmgrBlueprintDataSource(t *testing.T) {
	name := os.Getenv("PCD_ACC_BLUEPRINT_NAME")
	if name == "" {
		t.Skip("PCD_ACC_BLUEPRINT_NAME not set; skipping blueprint data source test")
	}
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { acctest.PreCheck(t) },
		ProtoV6ProviderFactories: acctest.ProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: fmt.Sprintf(`data "pcd_cluster_blueprint" "test" { name = %q }`, name),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("data.pcd_cluster_blueprint.test", "name", name),
					resource.TestCheckResourceAttrSet("data.pcd_cluster_blueprint.test", "networking_type"),
				),
			},
		},
	})
}

// TestAccResmgrHostDataSource resolves an onboarded host by the hostname it
// reports (read-only). Set PCD_ACC_HOST_NAME to the hostname as resmgr reports
// it (usually the FQDN); PCD_ACC_HOST_ID, when set, is asserted as the result.
func TestAccResmgrHostDataSource(t *testing.T) {
	name := os.Getenv("PCD_ACC_HOST_NAME")
	if name == "" {
		t.Skip("PCD_ACC_HOST_NAME not set; skipping host data source test")
	}
	const dn = "data.pcd_host.test"
	checks := []resource.TestCheckFunc{
		resource.TestCheckResourceAttr(dn, "name", name),
		resource.TestCheckResourceAttrSet(dn, "id"),
		resource.TestCheckResourceAttrSet(dn, "responding"),
		resource.TestCheckResourceAttrSet(dn, "roles.#"),
	}
	if id := os.Getenv("PCD_ACC_HOST_ID"); id != "" {
		checks = append(checks, resource.TestCheckResourceAttr(dn, "id", id))
	}
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { acctest.PreCheck(t) },
		ProtoV6ProviderFactories: acctest.ProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: fmt.Sprintf(`data "pcd_host" "test" { name = %q }`, name),
				Check:  resource.ComposeAggregateTestCheckFunc(checks...),
			},
		},
	})
}

// TestAccResmgrHostConfig creates a host configuration, adds a network label
// (an in-place update), moves it to another interface (a replacement: resmgr
// refuses to change the interface an existing label maps to, PCD-9803), and
// imports it. This mutates the control
// plane, so it is opt-in: set PCD_ACC_RESMGR=1 to run, and PCD_ACC_BLUEPRINT_NAME
// to the region's cluster blueprint: resmgr refuses to create a host
// configuration that does not name one (404 ClusterBlueprintNotFound) or that
// leaves a traffic interface other than live migration unset (400
// HostconfBadConfigInput), so the
// configuration under test names the blueprint and puts every traffic type on
// the one interface.
func TestAccResmgrHostConfig(t *testing.T) {
	if os.Getenv("PCD_ACC_RESMGR") == "" {
		t.Skip("PCD_ACC_RESMGR not set; skipping resmgr mutation test")
	}
	blueprint := os.Getenv("PCD_ACC_BLUEPRINT_NAME")
	if blueprint == "" {
		t.Skip("PCD_ACC_BLUEPRINT_NAME not set; resmgr needs a cluster blueprint to create a host config")
	}
	const rn = "pcd_host_config.test"
	var firstID string
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { acctest.PreCheck(t) },
		ProtoV6ProviderFactories: acctest.ProtoV6ProviderFactories,
		CheckDestroy:             testAccCheckHostConfigDestroy(t),
		Steps: []resource.TestStep{
			{
				Config: testAccHostConfigConfig("tf-acc-hc", blueprint, "enp1s0", map[string]string{"physnet1": "enp1s0"}),
				Check: resource.ComposeAggregateTestCheckFunc(
					testAccCheckHostConfigExists(t, rn),
					resource.TestCheckResourceAttr(rn, "name", "tf-acc-hc"),
					resource.TestCheckResourceAttr(rn, "mgmt_interface", "enp1s0"),
					resource.TestCheckResourceAttr(rn, "network_labels.physnet1", "enp1s0"),
					testAccRememberID(rn, &firstID),
				),
			},
			// A second label on another interface: resmgr accepts additions, so this
			// is an in-place update and the id stays.
			{
				Config: testAccHostConfigConfig("tf-acc-hc", blueprint, "enp1s0", map[string]string{"physnet1": "enp1s0", "physnet2": "enp3s0"}),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(rn, plancheck.ResourceActionUpdate)},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					testAccCheckHostConfigExists(t, rn),
					resource.TestCheckResourceAttr(rn, "network_labels.physnet2", "enp3s0"),
					testAccCheckIDUnchanged(rn, &firstID),
				),
			},
			// The host moves to another interface, label included (the PCD-9803
			// scenario: a bond renamed under a physnet). The interfaces alone would
			// update in place; the changed label value makes it a replacement, so
			// the plan must be one and the new configuration must carry a new id.
			// resmgr insists the tunneling interface carries a label, so the label
			// cannot move alone.
			{
				Config: testAccHostConfigConfig("tf-acc-hc", blueprint, "enp2s0", map[string]string{"physnet1": "enp2s0", "physnet2": "enp3s0"}),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(rn, plancheck.ResourceActionReplace)},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					testAccCheckHostConfigExists(t, rn),
					resource.TestCheckResourceAttr(rn, "mgmt_interface", "enp2s0"),
					resource.TestCheckResourceAttr(rn, "network_labels.physnet1", "enp2s0"),
					testAccCheckIDChanged(rn, &firstID),
				),
			},
			{ResourceName: rn, ImportState: true, ImportStateVerify: true},
		},
	})
}

// TestAccResmgrHostConfigRefusals applies one configuration per rule resmgr
// enforces at creation and expects the provider to name the attribute to fix.
// Each step leaves nothing behind (the create is refused), so there is no
// CheckDestroy. Opt-in like TestAccResmgrHostConfig: PCD_ACC_RESMGR=1 and
// PCD_ACC_BLUEPRINT_NAME.
func TestAccResmgrHostConfigRefusals(t *testing.T) {
	if os.Getenv("PCD_ACC_RESMGR") == "" {
		t.Skip("PCD_ACC_RESMGR not set; skipping resmgr mutation test")
	}
	blueprint := os.Getenv("PCD_ACC_BLUEPRINT_NAME")
	if blueprint == "" {
		t.Skip("PCD_ACC_BLUEPRINT_NAME not set; resmgr needs a cluster blueprint to create a host config")
	}
	config := func(blueprint, vmConsole string, labels map[string]string) string {
		keys := make([]string, 0, len(labels))
		for k := range labels {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var b strings.Builder
		for _, k := range keys {
			fmt.Fprintf(&b, "    %s = %q\n", k, labels[k])
		}
		vm := ""
		if vmConsole != "" {
			vm = fmt.Sprintf("  vm_console_interface     = %q\n", vmConsole)
		}
		return fmt.Sprintf(`
resource "pcd_host_config" "refused" {
  name         = "tf-acc-hc-refused"
  cluster_name = %q

  mgmt_interface           = "enp1s0"
%s  host_liveness_interface  = "enp1s0"
  tunneling_interface      = "enp1s0"
  imagelib_interface       = "enp1s0"
  live_migration_interface = "enp1s0"

  network_labels = {
%s  }
}
`, blueprint, vm, b.String())
	}
	one := map[string]string{"physnet1": "enp1s0"}
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { acctest.PreCheck(t) },
		ProtoV6ProviderFactories: acctest.ProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{Config: config("tf-acc-no-such-blueprint", "enp1s0", one), ExpectError: regexp.MustCompile(`cluster_name does not name a cluster blueprint`)},
			{Config: config(blueprint, "", one), ExpectError: regexp.MustCompile(`vm_console_interface must be set`)},
			{Config: config(blueprint, "enp1s0", map[string]string{"physnet1": "enp3s0"}), ExpectError: regexp.MustCompile(`no entry for the tunneling interface`)},
			{Config: config(blueprint, "enp1s0", map[string]string{"physnet1": "enp1s0", "physnet2": "enp1s0"}), ExpectError: regexp.MustCompile(`at most one label`)},
			// An empty map is dropped from the request, which resmgr treats as no labels.
			{Config: config(blueprint, "enp1s0", nil), ExpectError: regexp.MustCompile(`network_labels must map at least one label`)},
		},
	})
}

// testAccHostConfigConfig puts every traffic type on iface and renders labels
// (sorted, so the configuration is stable across steps).
func testAccHostConfigConfig(name, blueprint, iface string, labels map[string]string) string {
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		fmt.Fprintf(&b, "    %s = %q\n", k, labels[k])
	}
	return fmt.Sprintf(`
resource "pcd_host_config" "test" {
  name         = %q
  cluster_name = %q

  mgmt_interface           = %[3]q
  vm_console_interface     = %[3]q
  host_liveness_interface  = %[3]q
  tunneling_interface      = %[3]q
  imagelib_interface       = %[3]q
  live_migration_interface = %[3]q

  network_labels = {
%s  }
}
`, name, blueprint, iface, b.String())
}

// testAccRememberID stores the resource's current id for a later step to compare against.
func testAccRememberID(n string, into *string) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		rs, ok := s.RootModule().Resources[n]
		if !ok {
			return fmt.Errorf("not found in state: %s", n)
		}
		*into = rs.Primary.ID
		return nil
	}
}

// testAccCheckIDUnchanged fails when the resource lost the id an earlier step remembered.
func testAccCheckIDUnchanged(n string, was *string) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		rs, ok := s.RootModule().Resources[n]
		if !ok {
			return fmt.Errorf("not found in state: %s", n)
		}
		if rs.Primary.ID != *was {
			return fmt.Errorf("%s changed id from %s to %s on a label addition; it should have been updated in place", n, *was, rs.Primary.ID)
		}
		return nil
	}
}

// testAccCheckIDChanged fails when the resource kept the id an earlier step remembered.
func testAccCheckIDChanged(n string, was *string) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		rs, ok := s.RootModule().Resources[n]
		if !ok {
			return fmt.Errorf("not found in state: %s", n)
		}
		if rs.Primary.ID == *was {
			return fmt.Errorf("%s kept id %s across a network_labels change; it should have been replaced", n, *was)
		}
		return nil
	}
}

func testAccCheckHostConfigExists(t *testing.T, n string) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		rs, ok := s.RootModule().Resources[n]
		if !ok {
			return fmt.Errorf("not found in state: %s", n)
		}
		client, err := acctest.LabConfig(t).ResmgrV2Client()
		if err != nil {
			return err
		}
		var hc map[string]any
		if _, err := client.Get(context.Background(), client.ServiceURL("hostconfigs", rs.Primary.ID), &hc, &gophercloud.RequestOpts{OkCodes: []int{200}}); err != nil {
			return fmt.Errorf("host config %s not found via API: %w", rs.Primary.ID, err)
		}
		return nil
	}
}

func testAccCheckHostConfigDestroy(t *testing.T) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		client, err := acctest.LabConfig(t).ResmgrV2Client()
		if err != nil {
			return err
		}
		for _, rs := range s.RootModule().Resources {
			if rs.Type != "pcd_host_config" {
				continue
			}
			var hc map[string]any
			_, err := client.Get(context.Background(), client.ServiceURL("hostconfigs", rs.Primary.ID), &hc, &gophercloud.RequestOpts{OkCodes: []int{200}})
			if err == nil {
				return fmt.Errorf("host config %s still exists", rs.Primary.ID)
			}
			if !gophercloud.ResponseCodeIs(err, 404) {
				return fmt.Errorf("unexpected error checking host config %s: %w", rs.Primary.ID, err)
			}
		}
		return nil
	}
}

// TestAccResmgrHostClusterRoleImport assigns a cluster role, then imports it
// into a fresh working directory and lets ImportStateVerify compare every
// imported attribute against the state created in the first step. Before the
// fix, the imported state left wait_until_converged null while the created
// state held false, so ImportStateVerify failed on the pre-fix code and
// passes on the fixed code.
// Mutates the control plane, so it is opt-in: PCD_ACC_RESMGR=1 and
// PCD_ACC_HOST_ID=<host uuid> (an onboarded host that has a host configuration
// assigned). PCD_ACC_CLUSTER_ROLE picks the role (default image-library); the
// test adds it and removes it again.
func TestAccResmgrHostClusterRoleImport(t *testing.T) {
	if os.Getenv("PCD_ACC_RESMGR") == "" {
		t.Skip("PCD_ACC_RESMGR not set; skipping resmgr mutation test")
	}
	hostID := os.Getenv("PCD_ACC_HOST_ID")
	if hostID == "" {
		t.Skip("PCD_ACC_HOST_ID not set; skipping cluster role import test")
	}
	role := os.Getenv("PCD_ACC_CLUSTER_ROLE")
	if role == "" {
		role = "image-library"
	}
	const rn = "pcd_host_cluster_role.test"
	cfg := fmt.Sprintf(`
resource "pcd_host_cluster_role" "test" {
  host_id = %q
  role    = %q
}
`, hostID, role)
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { acctest.PreCheck(t) },
		ProtoV6ProviderFactories: acctest.ProtoV6ProviderFactories,
		CheckDestroy:             testAccCheckHostClusterRoleDestroy(t, hostID, role),
		Steps: []resource.TestStep{
			{
				Config: cfg,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(rn, "id", hostID+"/"+role),
					resource.TestCheckResourceAttr(rn, "wait_until_converged", "false"),
				),
			},
			// ImportState runs in its own, fresh working directory; ImportStateVerify
			// then diffs every attribute of that imported state against the state
			// created above. That diff is the regression guard: before the fix, the
			// imported state left wait_until_converged null while the created state
			// held false, so this comparison failed.
			{ResourceName: rn, ImportState: true, ImportStateId: hostID + "/" + role, ImportStateVerify: true},
		},
	})
}

// testAccCheckHostClusterRoleDestroy polls the host record until role is no
// longer among its roles, or the host itself is gone. Deauthorising a role is
// asynchronous, so a single check right after destroy can observe the role
// still present; this polls instead of asserting once.
func testAccCheckHostClusterRoleDestroy(t *testing.T, hostID, role string) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		client, err := acctest.LabConfig(t).ResmgrV2Client()
		if err != nil {
			return err
		}
		const (
			interval = 10 * time.Second
			timeout  = 5 * time.Minute
		)
		ctx := context.Background()
		deadline := time.Now().Add(timeout)
		for {
			var host struct {
				Roles []string `json:"roles"`
			}
			_, err := client.Get(ctx, client.ServiceURL("hosts", hostID), &host, &gophercloud.RequestOpts{OkCodes: []int{200}})
			switch {
			case gophercloud.ResponseCodeIs(err, 404):
				return nil // the host itself is gone, so the role is certainly gone
			case err != nil:
				return fmt.Errorf("checking host %s for role %s: %w", hostID, role, err)
			}
			cleared := true
			for _, r := range host.Roles {
				if r == role {
					cleared = false
					break
				}
			}
			if cleared {
				return nil
			}
			if time.Now().After(deadline) {
				return fmt.Errorf("role %s still assigned to host %s after %s", role, hostID, timeout)
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(interval):
			}
		}
	}
}

// TestAccResmgrHostClusterRoleDNSSettings assigns the dns cluster role with a
// listen override, checks resmgr's pf9-designate settings carry it while the
// role's other default settings survive the merge, imports (settings are not
// importable, by design), and removes the role, checking that the granular
// pf9-designate role goes with it. Opt-in: PCD_ACC_RESMGR=1 and PCD_ACC_HOST_ID.
// The role installs Designate services on the host and removing it
// deauthorizes them, so expect several minutes, and run it on a lab host.
func TestAccResmgrHostClusterRoleDNSSettings(t *testing.T) {
	if os.Getenv("PCD_ACC_RESMGR") == "" {
		t.Skip("PCD_ACC_RESMGR not set; skipping resmgr mutation test")
	}
	hostID := os.Getenv("PCD_ACC_HOST_ID")
	if hostID == "" {
		t.Skip("PCD_ACC_HOST_ID not set; skipping dns settings test")
	}
	const rn = "pcd_host_cluster_role.dns"
	cfg := func(listen string) string {
		return fmt.Sprintf(`
resource "pcd_host_cluster_role" "dns" {
  host_id = %q
  role    = "dns"
  settings = {
    listen = %q
  }
}
`, hostID, listen)
	}
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { acctest.PreCheck(t) },
		ProtoV6ProviderFactories: acctest.ProtoV6ProviderFactories,
		CheckDestroy: resource.ComposeAggregateTestCheckFunc(
			testAccCheckHostClusterRoleDestroy(t, hostID, "dns"),
			testAccCheckGranularRoleGone(t, hostID, "pf9-designate"),
		),
		Steps: []resource.TestStep{
			{
				Config: cfg("[::]:5354"),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(rn, "settings.listen", "[::]:5354"),
					testAccCheckRoleSetting(t, hostID, "pf9-designate", "listen", "[::]:5354"),
					testAccCheckUnmanagedDefaultsKept(t, hostID, "pf9-designate", "listen"),
				),
			},
			{
				Config: cfg("0.0.0.0:5354"),
				Check:  testAccCheckRoleSetting(t, hostID, "pf9-designate", "listen", "0.0.0.0:5354"),
			},
			{ResourceName: rn, ImportState: true, ImportStateId: hostID + "/dns", ImportStateVerify: true, ImportStateVerifyIgnore: []string{"settings"}},
		},
	})
}

// testAccCheckRoleSetting reads the granular role's settings through resmgr v1.
func testAccCheckRoleSetting(t *testing.T, hostID, role, key, want string) resource.TestCheckFunc {
	return func(_ *terraform.State) error {
		client, err := acctest.LabConfig(t).ResmgrV1Client()
		if err != nil {
			return err
		}
		var settings map[string]any
		if _, err := client.Get(context.Background(), client.ServiceURL("hosts", hostID, "roles", role), &settings, &gophercloud.RequestOpts{OkCodes: []int{200}}); err != nil {
			return fmt.Errorf("reading %s settings: %w", role, err)
		}
		if got := fmt.Sprint(settings[key]); got != want {
			return fmt.Errorf("%s.%s = %q, want %q (all: %v)", role, key, got, want, settings)
		}
		return nil
	}
}

// testAccCheckUnmanagedDefaultsKept guards the merge: every default setting in
// the role definition that the configuration does not manage must still be on
// the host's role, with its default value. A PUT that carried only the managed
// keys would drop them. It refuses to pass on a definition with no unmanaged
// defaults (or a response it could not find them in), which would prove
// nothing about the merge.
func testAccCheckUnmanagedDefaultsKept(t *testing.T, hostID, role string, managed ...string) resource.TestCheckFunc {
	return func(_ *terraform.State) error {
		client, err := acctest.LabConfig(t).ResmgrV1Client()
		if err != nil {
			return err
		}
		ctx := context.Background()
		var def struct {
			DefaultSettings map[string]any `json:"default_settings"`
		}
		if _, err := client.Get(ctx, client.ServiceURL("roles", role), &def, &gophercloud.RequestOpts{OkCodes: []int{200}}); err != nil {
			return fmt.Errorf("reading the %s role definition: %w", role, err)
		}
		var settings map[string]any
		if _, err := client.Get(ctx, client.ServiceURL("hosts", hostID, "roles", role), &settings, &gophercloud.RequestOpts{OkCodes: []int{200}}); err != nil {
			return fmt.Errorf("reading %s settings: %w", role, err)
		}
		checked := 0
		for k, want := range def.DefaultSettings {
			if slices.Contains(managed, k) {
				continue
			}
			got, ok := settings[k]
			if !ok {
				return fmt.Errorf("%s lost the unmanaged setting %q; the merge dropped it: %v", role, k, settings)
			}
			if fmt.Sprint(got) != fmt.Sprint(want) {
				return fmt.Errorf("%s.%s = %v, want its default %v; the merge changed an unmanaged setting: %v", role, k, got, want, settings)
			}
			checked++
		}
		if checked == 0 {
			return fmt.Errorf("the %s role definition has no default settings besides %v, so the merge cannot be checked: %v", role, managed, def.DefaultSettings)
		}
		return nil
	}
}

// testAccCheckGranularRoleGone polls the host's granular role through resmgr
// v1 until it is gone: a 404 (which also covers a host that is itself gone) or
// a 200 with no body. The v2 host view reports only the cluster role, so it
// cannot show a granular role, with the settings written onto it, left behind.
func testAccCheckGranularRoleGone(t *testing.T, hostID, role string) resource.TestCheckFunc {
	return func(_ *terraform.State) error {
		client, err := acctest.LabConfig(t).ResmgrV1Client()
		if err != nil {
			return err
		}
		const (
			interval = 10 * time.Second
			timeout  = 5 * time.Minute
		)
		ctx := context.Background()
		url := client.ServiceURL("hosts", hostID, "roles", role)
		deadline := time.Now().Add(timeout)
		for {
			var raw json.RawMessage
			_, err := client.Get(ctx, url, &raw, &gophercloud.RequestOpts{OkCodes: []int{200}})
			switch {
			case gophercloud.ResponseCodeIs(err, 404), errors.Is(err, io.EOF):
				return nil // io.EOF is a 200 with an empty body
			case err != nil:
				return fmt.Errorf("checking host %s for granular role %s: %w", hostID, role, err)
			}
			if b := bytes.TrimSpace(raw); len(b) == 0 || bytes.Equal(b, []byte("null")) {
				return nil
			}
			if time.Now().After(deadline) {
				return fmt.Errorf("granular role %s still on host %s %s after the cluster role was removed: %s", role, hostID, timeout, raw)
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(interval):
			}
		}
	}
}
