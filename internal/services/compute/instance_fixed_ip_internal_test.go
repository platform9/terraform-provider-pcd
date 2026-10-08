// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package compute

import (
	"context"
	"slices"
	"testing"

	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/servers"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

func networkList(t *testing.T, nics ...instanceNetworkModel) types.List {
	t.Helper()
	s := instanceTestSchema(&instanceResource{})
	l, d := types.ListValueFrom(context.Background(), s.Blocks["network"].(schema.ListNestedBlock).NestedObject.Type(), nics)
	if d.HasError() {
		t.Fatalf("network: %v", d)
	}
	return l
}

// fixed_ip_v4 reaches Nova as networks[].fixed_ip, the field the PCD UI's
// create wizard sends for a Private IP; a NIC without one sends nothing extra.
func TestNetworksFromListSendsTheFixedIP(t *testing.T) {
	var diags diag.Diagnostics
	nets := networksFromList(context.Background(), networkList(t,
		instanceNetworkModel{UUID: types.StringValue("net-1"), Name: types.StringNull(), Port: types.StringNull(), FixedIPv4: types.StringValue("10.0.0.5")},
		instanceNetworkModel{UUID: types.StringNull(), Name: types.StringNull(), Port: types.StringValue("p-1"), FixedIPv4: types.StringNull()},
	), &diags)
	if diags.HasError() {
		t.Fatal(diags)
	}
	body, err := servers.CreateOpts{Name: "vm-1", FlavorRef: "flv-1", ImageRef: "img-1", Networks: nets}.ToServerCreateMap()
	if err != nil {
		t.Fatal(err)
	}
	got := body["server"].(map[string]any)["networks"].([]map[string]any)
	if len(got) != 2 || got[0]["uuid"] != "net-1" || got[0]["fixed_ip"] != "10.0.0.5" || len(got[0]) != 2 {
		t.Fatalf("networks[0] = %v, want {uuid: net-1, fixed_ip: 10.0.0.5}", got)
	}
	if _, sent := got[1]["fixed_ip"]; sent || got[1]["port"] != "p-1" {
		t.Fatalf("networks[1] = %v, want {port: p-1} only", got[1])
	}
}

// The create request carries the address, end to end through Create.
func TestCreateSendsTheFixedIP(t *testing.T) {
	shortPolls(t)
	nova, r := newInstanceNova(t, "ACTIVE")
	plan := instanceCreatePlan(t, r, types.StringUnknown())
	plan.Network = networkList(t, instanceNetworkModel{
		UUID: types.StringValue("net-1"), Name: types.StringNull(), Port: types.StringUnknown(), FixedIPv4: types.StringValue("10.117.0.50"),
	})
	got, resp := runInstanceCreate(t, r, plan)
	if resp.Diagnostics.HasError() {
		t.Fatalf("create: %v", resp.Diagnostics)
	}
	nets, _ := nova.createBody["server"].(map[string]any)["networks"].([]any)
	if len(nets) != 1 || nets[0].(map[string]any)["fixed_ip"] != "10.117.0.50" {
		t.Fatalf("POST /servers networks = %v, want fixed_ip 10.117.0.50", nets)
	}
	var blocks []instanceNetworkModel
	got.Network.ElementsAs(context.Background(), &blocks, false)
	if len(blocks) != 1 || blocks[0].FixedIPv4.ValueString() != "10.117.0.50" {
		t.Fatalf("state network = %v; fixed_ip_v4 must round-trip from the configuration", blocks)
	}
}

// Nova rejects fixed_ip next to a port, and the address needs the network's
// uuid; both are caught at plan time, counting an unknown value as set.
func TestValidateConfigChecksFixedIPv4(t *testing.T) {
	r := &instanceResource{}
	for _, tc := range []struct {
		name     string
		nic      instanceNetworkModel
		wantErrs []string // diagnostic summaries
	}{
		{"uuid and address", instanceNetworkModel{UUID: types.StringValue("net-1"), Port: types.StringNull(), FixedIPv4: types.StringValue("10.0.0.5")}, nil},
		{"uuid from another resource", instanceNetworkModel{UUID: types.StringUnknown(), Port: types.StringNull(), FixedIPv4: types.StringValue("10.0.0.5")}, nil},
		{"address from another resource", instanceNetworkModel{UUID: types.StringValue("net-1"), Port: types.StringNull(), FixedIPv4: types.StringUnknown()}, nil},
		{"empty address", instanceNetworkModel{UUID: types.StringValue("net-1"), Port: types.StringNull(), FixedIPv4: types.StringValue("")}, nil},
		{"with a port", instanceNetworkModel{UUID: types.StringNull(), Port: types.StringValue("p-1"), FixedIPv4: types.StringValue("10.0.0.5")},
			[]string{"fixed_ip_v4 with port", "fixed_ip_v4 without uuid"}},
		{"with a port created in the same apply", instanceNetworkModel{UUID: types.StringValue("net-1"), Port: types.StringUnknown(), FixedIPv4: types.StringValue("10.0.0.5")},
			[]string{"fixed_ip_v4 with port"}},
		{"without uuid", instanceNetworkModel{UUID: types.StringNull(), Port: types.StringNull(), FixedIPv4: types.StringValue("10.0.0.5")},
			[]string{"fixed_ip_v4 without uuid"}},
		{"IPv6", instanceNetworkModel{UUID: types.StringValue("net-1"), Port: types.StringNull(), FixedIPv4: types.StringValue("fd00::5")},
			[]string{"Invalid fixed_ip_v4"}},
		{"IPv4-mapped IPv6", instanceNetworkModel{UUID: types.StringValue("net-1"), Port: types.StringNull(), FixedIPv4: types.StringValue("::ffff:10.0.0.5")},
			[]string{"Invalid fixed_ip_v4"}},
		{"not an address", instanceNetworkModel{UUID: types.StringValue("net-1"), Port: types.StringNull(), FixedIPv4: types.StringValue("10.0.0")},
			[]string{"Invalid fixed_ip_v4"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.nic.Name = types.StringNull()
			d := validateInstanceConfig(t, r, instanceTestConfig(t, r, types.SetNull(types.StringType), tc.nic))
			var got []string
			for _, e := range d.Errors() {
				got = append(got, e.Summary())
			}
			if !slices.Equal(got, tc.wantErrs) {
				t.Fatalf("errors = %v, want %v", got, tc.wantErrs)
			}
		})
	}
}
