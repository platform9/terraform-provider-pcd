// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package networking

import (
	"context"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

func dnsNetworkModel(dnsDomain types.String) *networkModel {
	return &networkModel{
		Name:         types.StringValue("net"),
		Description:  types.StringValue(""),
		AdminStateUp: types.BoolValue(true),
		Shared:       types.BoolNull(),
		External:     types.BoolNull(),
		PortSecurity: types.BoolNull(),
		Segments:     types.ListNull(types.ObjectType{}),
		Tags:         types.SetNull(types.StringType),
		DNSDomain:    dnsDomain,
	}
}

// networkCreateBody renders the wire body exactly as networks.Create would.
func networkCreateBody(t *testing.T, m *networkModel) map[string]any {
	t.Helper()
	var diags diag.Diagnostics
	opts := networkCreateOpts(context.Background(), m, &diags)
	if diags.HasError() {
		t.Fatalf("networkCreateOpts: %v", diags)
	}
	body, err := opts.ToNetworkCreateMap()
	if err != nil {
		t.Fatal(err)
	}
	return body["network"].(map[string]any)
}

func networkUpdateBody(t *testing.T, plan, state *networkModel) map[string]any {
	t.Helper()
	body, err := networkUpdateOpts(plan, state).ToNetworkUpdateMap()
	if err != nil {
		t.Fatal(err)
	}
	return body["network"].(map[string]any)
}

// dns_domain rides on the create body only when a zone is named: "" is the
// server default, and a Neutron without the dns extension rejects the key.
func TestNetworkCreateOptsDNSDomain(t *testing.T) {
	if got := networkCreateBody(t, dnsNetworkModel(types.StringValue("lab.example.com.")))["dns_domain"]; got != "lab.example.com." {
		t.Fatalf("dns_domain = %v; the zone association would not be created", got)
	}
	if _, sent := networkCreateBody(t, dnsNetworkModel(types.StringValue("")))["dns_domain"]; sent {
		t.Fatalf("dns_domain sent on create although empty")
	}
	if _, sent := networkCreateBody(t, dnsNetworkModel(types.StringNull()))["dns_domain"]; sent {
		t.Fatalf("dns_domain sent on create although null")
	}
	// The other extensions must still be wrapped underneath it. gophercloud's
	// external extension writes the *bool itself into the map, not its value.
	m := dnsNetworkModel(types.StringValue("lab.example.com."))
	m.External = types.BoolValue(true)
	ext, ok := networkCreateBody(t, m)["router:external"].(*bool)
	if !ok || ext == nil || !*ext {
		t.Fatalf("router:external = %v; wrapping dns_domain dropped the external extension", networkCreateBody(t, m)["router:external"])
	}
}

// An update sends dns_domain exactly when it changed, including the change
// to "" that removes the association (the ticket's stated way to detach).
func TestNetworkUpdateOptsDNSDomain(t *testing.T) {
	for _, tc := range []struct {
		name        string
		plan, state string
		wantSent    bool
	}{
		{name: "unchanged", plan: "a.example.com.", state: "a.example.com."},
		{name: "unchanged empty", plan: "", state: ""},
		{name: "set", plan: "a.example.com.", state: "", wantSent: true},
		{name: "changed", plan: "b.example.com.", state: "a.example.com.", wantSent: true},
		{name: "cleared", plan: "", state: "a.example.com.", wantSent: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := networkUpdateBody(t, dnsNetworkModel(types.StringValue(tc.plan)), dnsNetworkModel(types.StringValue(tc.state)))
			got, sent := body["dns_domain"]
			if sent != tc.wantSent {
				t.Fatalf("dns_domain sent = %v, want %v (body %v)", sent, tc.wantSent, body)
			}
			if sent && got != tc.plan {
				t.Fatalf("dns_domain = %v, want %q", got, tc.plan)
			}
		})
	}
	// A state written before the attribute existed reads back as null after
	// refresh fills it, but a null state must still be handled: send the value.
	if _, sent := networkUpdateBody(t, dnsNetworkModel(types.StringValue("a.example.com.")), dnsNetworkModel(types.StringNull()))["dns_domain"]; !sent {
		t.Fatalf("dns_domain not sent when state is null")
	}
}

// Neutron enforces these (neutron_lib validate_dns_domain, after lower-casing
// the value); catching them at plan time turns a 400 mid-apply, or a
// "inconsistent result after apply" on a mixed-case value, into a message that
// names the attribute.
func TestInvalidDNSDomain(t *testing.T) {
	label63 := strings.Repeat("a", 63)
	for _, tc := range []struct {
		in      string
		wantErr bool
	}{
		{in: ""},
		{in: "example.com."},
		{in: "sub.example.com."},
		{in: "a-b.example.com."},
		{in: label63 + ".example.com."},
		{in: "10.in-addr.arpa."},
		{in: strings.Repeat("a.", 120) + "example.com."},                // 252 characters with the dot
		{in: "example.com", wantErr: true},                              // no trailing dot
		{in: ".", wantErr: true},                                        // empty label
		{in: "a..example.com.", wantErr: true},                          // empty label
		{in: "App.Example.Com.", wantErr: true},                         // Neutron lower-cases; refuse rather than drift
		{in: "my_zone.example.com.", wantErr: true},                     // underscore
		{in: "-bad.example.com.", wantErr: true},                        // leading hyphen
		{in: "bad-.example.com.", wantErr: true},                        // trailing hyphen
		{in: label63 + "a.example.com.", wantErr: true},                 // 64-character label
		{in: "example.123.", wantErr: true},                             // all-numeric TLD
		{in: strings.Repeat("a.", 121) + "example.com.", wantErr: true}, // 254 characters with the dot
	} {
		if msg := invalidDNSDomain(tc.in); (msg != "") != tc.wantErr {
			t.Errorf("invalidDNSDomain(%q) = %q, wantErr %v", tc.in, msg, tc.wantErr)
		}
	}
}
