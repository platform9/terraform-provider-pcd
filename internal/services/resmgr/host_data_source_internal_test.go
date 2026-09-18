// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

// Package resmgr (not resmgr_test) to reach hostByName and hostDataFromAPI,
// which are unexported.
package resmgr

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// pcd_host resolves the resmgr host UUID from the hostname the host agent
// reports (PCD-9799). The match is exact: hosts usually report an FQDN, and a
// short name that silently matched one host today could match two tomorrow.
func TestHostByName(t *testing.T) {
	hosts := []hostAPI{
		{ID: "id-a", Info: &hostInfoAPI{Hostname: "hyp1.lab.local"}},
		{ID: "id-b", Info: &hostInfoAPI{Hostname: "hyp2.lab.local"}},
		{ID: "id-c"}, // authorized but has not reported yet: no info at all
	}

	t.Run("exact hostname resolves", func(t *testing.T) {
		got, err := hostByName(hosts, "hyp1.lab.local")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got.ID != "id-a" {
			t.Fatalf("id = %q, want id-a", got.ID)
		}
	})

	t.Run("short name does not match an FQDN", func(t *testing.T) {
		if _, err := hostByName(hosts, "hyp1"); err == nil {
			t.Fatal("hyp1 matched hyp1.lab.local; a prefix match would pick a host the user did not name")
		}
	})

	t.Run("unknown name lists the hostnames resmgr knows", func(t *testing.T) {
		_, err := hostByName(hosts, "nope")
		if err == nil {
			t.Fatal("expected an error for an unknown hostname")
		}
		for _, want := range []string{`"nope"`, "hyp1.lab.local", "hyp2.lab.local", "not reported"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q does not mention %s", err, want)
			}
		}
	})

	t.Run("a host without info is never matched", func(t *testing.T) {
		if _, err := hostByName(hosts, ""); err == nil {
			t.Fatal("an empty name matched the host that has no hostname yet")
		}
	})

	t.Run("duplicate hostnames are an error naming both ids", func(t *testing.T) {
		dup := append(hosts, hostAPI{ID: "id-d", Info: &hostInfoAPI{Hostname: "hyp1.lab.local"}})
		_, err := hostByName(dup, "hyp1.lab.local")
		if err == nil {
			t.Fatal("two hosts named hyp1.lab.local resolved silently")
		}
		for _, want := range []string{"id-a", "id-d"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q does not name %s", err, want)
			}
		}
	})
}

// The v2 host record nests hostname and responding under info, and omits info
// entirely for a host that has not reported yet. hostAPI is decoded from both
// the v1 and v2 endpoints by the role and assignment resources, so the new
// field must stay optional.
func TestHostAPIDecodesV2Record(t *testing.T) {
	raw := `[
	  {"id": "136fc11a-ec5a-4699-b097-f75796134f8d", "roles": ["hypervisor", "image-library"],
	   "hostconfig_id": "b1426c74-132a-47cc-a316-bc77945dab58", "role_status": "ok",
	   "info": {"hostname": "pcd-ce-lab-ubuntu-hyp1.localdomain", "responding": true, "os_family": "Linux"}},
	  {"id": "new-host", "roles": []}
	]`
	var hosts []hostAPI
	if err := json.Unmarshal([]byte(raw), &hosts); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	h := hosts[0]
	if h.Info == nil || h.Info.Hostname != "pcd-ce-lab-ubuntu-hyp1.localdomain" || !h.Info.Responding {
		t.Fatalf("info = %+v, want hostname and responding from the nested object", h.Info)
	}
	if h.HostConfigID != "b1426c74-132a-47cc-a316-bc77945dab58" || len(h.Roles) != 2 {
		t.Fatalf("hostconfig_id/roles = %q/%v, want the top-level values", h.HostConfigID, h.Roles)
	}
	if hosts[1].Info != nil {
		t.Fatalf("a record without info decoded as %+v, want nil", hosts[1].Info)
	}
}

// The data source state must be fully known: nil roles become an empty list
// (not null) and a host without info reads as not responding, so a
// configuration can use the values without null checks.
func TestHostDataFromAPI(t *testing.T) {
	ctx := context.Background()

	full, diags := hostDataFromAPI(ctx, hostAPI{
		ID: "id-a", Roles: []string{"hypervisor"}, HostConfigID: "hc-1",
		Info: &hostInfoAPI{Hostname: "hyp1.lab.local", Responding: true},
	})
	if diags.HasError() {
		t.Fatalf("diags: %v", diags)
	}
	if full.ID.ValueString() != "id-a" || full.Name.ValueString() != "hyp1.lab.local" ||
		full.HostConfigID.ValueString() != "hc-1" || !full.Responding.ValueBool() {
		t.Fatalf("model = %+v, want every field copied", full)
	}
	if len(full.Roles.Elements()) != 1 {
		t.Fatalf("roles = %v, want [hypervisor]", full.Roles)
	}

	bare, diags := hostDataFromAPI(ctx, hostAPI{ID: "id-c", Info: &hostInfoAPI{Hostname: "hyp3.lab.local"}})
	if diags.HasError() {
		t.Fatalf("diags: %v", diags)
	}
	if bare.Roles.IsNull() || len(bare.Roles.Elements()) != 0 {
		t.Fatalf("roles = %v, want an empty (not null) list", bare.Roles)
	}
	if bare.Responding.ValueBool() || bare.HostConfigID.ValueString() != "" {
		t.Fatalf("responding/host_config_id = %v/%q, want false and empty", bare.Responding, bare.HostConfigID)
	}
}
