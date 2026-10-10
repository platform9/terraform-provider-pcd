// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package networking

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

func gatewaySubnetModel(noGateway types.Bool, gatewayIP types.String) *subnetModel {
	return &subnetModel{
		NetworkID:         types.StringValue("net-1"),
		Name:              types.StringValue("sub"),
		Description:       types.StringValue(""),
		CIDR:              types.StringValue("10.0.0.0/24"),
		IPVersion:         types.Int64Value(4),
		GatewayIP:         gatewayIP,
		NoGateway:         noGateway,
		EnableDHCP:        types.BoolValue(true),
		DNSNameservers:    types.ListNull(types.StringType),
		AllocationPools:   types.ListNull(poolObjType),
		DNSPublishFixedIP: types.BoolValue(false),
		Tags:              types.SetNull(types.StringType),
	}
}

// gatewayInBody reports whether the wire body carries gateway_ip, and its
// value (nil for JSON null, Neutron's "no gateway").
func gatewayInBody(t *testing.T, body map[string]any) (any, bool) {
	t.Helper()
	sub, ok := body["subnet"].(map[string]any)
	if !ok {
		t.Fatalf("body has no subnet object: %v", body)
	}
	v, sent := sub["gateway_ip"]
	return v, sent
}

// no_gateway = true goes out as "gateway_ip": null; otherwise a planned
// gateway is sent and an unknown or empty one is left to Neutron, as before.
func TestSubnetCreateOptsGateway(t *testing.T) {
	for _, tc := range []struct {
		name      string
		noGateway types.Bool
		gatewayIP types.String
		wantSent  bool
		wantValue any
	}{
		{name: "no gateway", noGateway: types.BoolValue(true), gatewayIP: types.StringUnknown(), wantSent: true, wantValue: nil},
		{name: "explicit gateway", noGateway: types.BoolValue(false), gatewayIP: types.StringValue("10.0.0.254"), wantSent: true, wantValue: "10.0.0.254"},
		{name: "unmanaged, explicit gateway", noGateway: types.BoolNull(), gatewayIP: types.StringValue("10.0.0.254"), wantSent: true, wantValue: "10.0.0.254"},
		{name: "Neutron default", noGateway: types.BoolValue(false), gatewayIP: types.StringUnknown()},
		{name: "unmanaged, Neutron default", noGateway: types.BoolUnknown(), gatewayIP: types.StringUnknown()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var diags diag.Diagnostics
			opts := subnetCreateOpts(context.Background(), gatewaySubnetModel(tc.noGateway, tc.gatewayIP), &diags)
			if diags.HasError() {
				t.Fatal(diags)
			}
			body, err := opts.ToSubnetCreateMap()
			if err != nil {
				t.Fatal(err)
			}
			v, sent := gatewayInBody(t, body)
			if sent != tc.wantSent || (sent && v != tc.wantValue) {
				t.Fatalf("gateway_ip sent=%v value=%v, want sent=%v value=%v", sent, v, tc.wantSent, tc.wantValue)
			}
		})
	}
}

// Update sends null whenever no_gateway is planned true and a planned gateway
// otherwise, so the subnet matches the plan even when state was stale. A plan
// for an unmanaged subnet without a gateway carries the no_gateway = true that
// ModifyPlan derives from its planned "", so an unrelated update sends null as
// "remove gateway" does, and Neutron treats it as unchanged. An empty gateway
// without a planned no_gateway = true is not sent.
func TestSubnetUpdateOptsGateway(t *testing.T) {
	for _, tc := range []struct {
		name      string
		noGateway types.Bool
		gatewayIP types.String
		wantSent  bool
		wantValue any
	}{
		{name: "remove gateway", noGateway: types.BoolValue(true), gatewayIP: types.StringValue(""), wantSent: true, wantValue: nil},
		{name: "restore default", noGateway: types.BoolValue(false), gatewayIP: types.StringValue("10.0.0.1"), wantSent: true, wantValue: "10.0.0.1"},
		{name: "unmanaged gateway kept", noGateway: types.BoolUnknown(), gatewayIP: types.StringValue("10.0.0.1"), wantSent: true, wantValue: "10.0.0.1"},
		{name: "empty gateway, unknown no_gateway", noGateway: types.BoolUnknown(), gatewayIP: types.StringValue("")},
		{name: "state from before no_gateway", noGateway: types.BoolNull(), gatewayIP: types.StringValue("")},
		{name: "unknown gateway", noGateway: types.BoolValue(false), gatewayIP: types.StringUnknown()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var diags diag.Diagnostics
			state := gatewaySubnetModel(types.BoolValue(false), types.StringValue("10.0.0.1"))
			opts := subnetUpdateOpts(context.Background(), gatewaySubnetModel(tc.noGateway, tc.gatewayIP), state, &diags)
			if diags.HasError() {
				t.Fatal(diags)
			}
			body, err := opts.ToSubnetUpdateMap()
			if err != nil {
				t.Fatal(err)
			}
			v, sent := gatewayInBody(t, body)
			if sent != tc.wantSent || (sent && v != tc.wantValue) {
				t.Fatalf("gateway_ip sent=%v value=%v, want sent=%v value=%v", sent, v, tc.wantSent, tc.wantValue)
			}
		})
	}
}

// Neutron's own default: network + 1 for IPv4, the network address itself for
// IPv6 (default IPv6 pools start at ::1, so ::1 would collide with them).
func TestNeutronDefaultGateway(t *testing.T) {
	for _, tc := range []struct {
		cidr    string
		want    string
		wantErr bool
	}{
		{cidr: "10.0.0.0/24", want: "10.0.0.1"},
		{cidr: "10.0.0.5/24", want: "10.0.0.1"},
		{cidr: "192.168.10.0/30", want: "192.168.10.1"},
		{cidr: "10.0.0.7/32", wantErr: true},
		{cidr: "2001:db8::/64", want: "2001:db8::"},
		{cidr: "2001:db8:0:1::5/64", want: "2001:db8:0:1::"},
		{cidr: "not-a-cidr", wantErr: true},
		{cidr: "", wantErr: true},
	} {
		got, err := neutronDefaultGateway(tc.cidr)
		if (err != nil) != tc.wantErr || got != tc.want {
			t.Errorf("neutronDefaultGateway(%q) = %q, %v; want %q, error %v", tc.cidr, got, err, tc.want, tc.wantErr)
		}
	}
}

// plannedGatewayIP only acts on a configured no_gateway and never overrides a
// configured gateway_ip.
func TestPlannedGatewayIP(t *testing.T) {
	cidr4 := types.StringValue("10.0.0.0/24")
	for _, tc := range []struct {
		name         string
		cfgNoGateway types.Bool
		cfgGateway   types.String
		prior        types.String
		cidr         types.String
		want         types.String
		wantOverride bool
	}{
		{name: "unmanaged", cfgNoGateway: types.BoolNull(), cfgGateway: types.StringNull(), prior: types.StringValue(""), cidr: cidr4},
		{name: "configured gateway wins", cfgNoGateway: types.BoolValue(false), cfgGateway: types.StringValue("10.0.0.254"), prior: types.StringValue(""), cidr: cidr4},
		{name: "remove on update", cfgNoGateway: types.BoolValue(true), cfgGateway: types.StringNull(), prior: types.StringValue("10.0.0.1"), cidr: cidr4, want: types.StringValue(""), wantOverride: true},
		{name: "none on create", cfgNoGateway: types.BoolValue(true), cfgGateway: types.StringNull(), prior: types.StringNull(), cidr: cidr4, want: types.StringValue(""), wantOverride: true},
		{name: "unknown no_gateway", cfgNoGateway: types.BoolUnknown(), cfgGateway: types.StringNull(), prior: types.StringValue("10.0.0.1"), cidr: cidr4, want: types.StringUnknown(), wantOverride: true},
		{name: "restore IPv4 default", cfgNoGateway: types.BoolValue(false), cfgGateway: types.StringNull(), prior: types.StringValue(""), cidr: cidr4, want: types.StringValue("10.0.0.1"), wantOverride: true},
		{name: "restore IPv6 default", cfgNoGateway: types.BoolValue(false), cfgGateway: types.StringNull(), prior: types.StringValue(""), cidr: types.StringValue("2001:db8::/64"), want: types.StringValue("2001:db8::"), wantOverride: true},
		{name: "keep existing gateway", cfgNoGateway: types.BoolValue(false), cfgGateway: types.StringNull(), prior: types.StringValue("10.0.0.254"), cidr: cidr4},
		{name: "default on create left to Neutron", cfgNoGateway: types.BoolValue(false), cfgGateway: types.StringNull(), prior: types.StringNull(), cidr: cidr4},
		{name: "restore with unknown cidr", cfgNoGateway: types.BoolValue(false), cfgGateway: types.StringNull(), prior: types.StringValue(""), cidr: types.StringUnknown(), want: types.StringUnknown(), wantOverride: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, override := plannedGatewayIP(tc.cfgNoGateway, tc.cfgGateway, tc.prior, tc.cidr)
			if override != tc.wantOverride {
				t.Fatalf("override = %v, want %v", override, tc.wantOverride)
			}
			if override && !got.Equal(tc.want) {
				t.Fatalf("planned gateway_ip = %v, want %v", got, tc.want)
			}
		})
	}
}

// gateway_ip = "" used to plan "" and apply Neutron's default ("inconsistent
// result after apply"); it is now refused at plan time unless no_gateway = true
// agrees with it, as is a non-empty gateway_ip next to no_gateway = true.
// no_gateway = false with a gateway_ip is fine.
func TestGatewayConfigDiags(t *testing.T) {
	for _, tc := range []struct {
		name        string
		noGateway   types.Bool
		gatewayIP   types.String
		wantSummary string
	}{
		{name: "empty gateway", noGateway: types.BoolNull(), gatewayIP: types.StringValue(""), wantSummary: "Invalid gateway_ip"},
		{name: "empty gateway with no_gateway = false", noGateway: types.BoolValue(false), gatewayIP: types.StringValue(""), wantSummary: "Invalid gateway_ip"},
		{name: "empty gateway with no_gateway", noGateway: types.BoolValue(true), gatewayIP: types.StringValue("")},
		{name: "empty gateway, unknown no_gateway", noGateway: types.BoolUnknown(), gatewayIP: types.StringValue("")},
		{name: "conflict", noGateway: types.BoolValue(true), gatewayIP: types.StringValue("10.0.0.1"), wantSummary: "Conflicting gateway settings"},
		{name: "false with gateway", noGateway: types.BoolValue(false), gatewayIP: types.StringValue("10.0.0.1")},
		{name: "true alone", noGateway: types.BoolValue(true), gatewayIP: types.StringNull()},
		{name: "unknown gateway", noGateway: types.BoolValue(true), gatewayIP: types.StringUnknown()},
		{name: "unknown no_gateway", noGateway: types.BoolUnknown(), gatewayIP: types.StringValue("10.0.0.1")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			diags := gatewayConfigDiags(tc.noGateway, tc.gatewayIP)
			if tc.wantSummary == "" {
				if diags.HasError() {
					t.Fatalf("unexpected error: %v", diags)
				}
				return
			}
			if !diags.HasError() || diags.Errors()[0].Summary() != tc.wantSummary {
				t.Fatalf("diagnostics = %v, want an error %q", diags, tc.wantSummary)
			}
		})
	}
}

// readInto derives no_gateway from the gateway Neutron reports: a subnet
// without one (gateway_ip null) reads true, one with a gateway reads false.
func TestSubnetReadIntoNoGateway(t *testing.T) {
	const withGateway = `"gateway_ip": "10.20.0.1"`
	if !strings.Contains(subnetJSON, withGateway) {
		t.Fatalf("subnetJSON no longer holds %s", withGateway)
	}
	for _, tc := range []struct {
		name    string
		gateway string
		want    bool
	}{
		{name: "gateway", gateway: withGateway, want: false},
		{name: "no gateway", gateway: `"gateway_ip": null`, want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := strings.Replace(subnetJSON, withGateway, tc.gateway, 1)
			routes := subnetRoutes()
			routes["GET "+subnetPath] = reply(http.StatusOK, `{"subnet": `+body+`}`)
			r := &subnetResource{config: newFakeNeutron(t, routes).config}
			client, err := r.config.NetworkV2Client()
			if err != nil {
				t.Fatalf("building the fake Neutron client: %v", err)
			}
			m := subnetModel{Region: types.StringNull()}
			notFound, diags := r.readInto(context.Background(), client, subnetID, &m)
			if notFound || diags.HasError() {
				t.Fatalf("readInto: not found %v, diagnostics %v", notFound, diags)
			}
			if m.NoGateway.IsNull() || m.NoGateway.IsUnknown() || m.NoGateway.ValueBool() != tc.want {
				t.Fatalf("no_gateway = %v with gateway_ip %q, want %v", m.NoGateway, m.GatewayIP.ValueString(), tc.want)
			}
		})
	}
}
