// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package networking

import (
	"context"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

func dnsSubnetModel(publish types.Bool) *subnetModel {
	return &subnetModel{
		NetworkID:         types.StringValue("net-1"),
		Name:              types.StringValue("sub"),
		Description:       types.StringValue(""),
		CIDR:              types.StringValue("10.0.0.0/24"),
		IPVersion:         types.Int64Value(4),
		GatewayIP:         types.StringValue(""),
		EnableDHCP:        types.BoolValue(true),
		DNSNameservers:    types.ListNull(types.StringType),
		AllocationPools:   types.ListNull(poolObjType),
		Tags:              types.SetNull(types.StringType),
		DNSPublishFixedIP: publish,
	}
}

// dns_publish_fixed_ip is sent on create only when true: false is the server
// default, and a Neutron without the subnet-dns-publish-fixed-ip extension
// rejects the key.
func TestSubnetCreateOptsDNSPublishFixedIP(t *testing.T) {
	var diags diag.Diagnostics
	opts := subnetCreateOpts(context.Background(), dnsSubnetModel(types.BoolValue(true)), &diags)
	if diags.HasError() {
		t.Fatal(diags)
	}
	if opts.DNSPublishFixedIP == nil || !*opts.DNSPublishFixedIP {
		t.Fatalf("dns_publish_fixed_ip = %v; records for this subnet's fixed IPs would never be published", opts.DNSPublishFixedIP)
	}
	if opts.NetworkID != "net-1" || opts.CIDR != "10.0.0.0/24" || opts.EnableDHCP == nil || !*opts.EnableDHCP {
		t.Fatalf("base options changed while moving them into subnetCreateOpts: %+v", opts)
	}
	opts = subnetCreateOpts(context.Background(), dnsSubnetModel(types.BoolValue(false)), &diags)
	if opts.DNSPublishFixedIP != nil {
		t.Fatalf("dns_publish_fixed_ip sent on create although false")
	}
}

// An update sends the flag exactly when it changed, in both directions.
func TestSubnetUpdateOptsDNSPublishFixedIP(t *testing.T) {
	for _, tc := range []struct {
		name        string
		plan, state bool
		want        *bool
	}{
		{name: "unchanged false", plan: false, state: false},
		{name: "unchanged true", plan: true, state: true},
		{name: "enable", plan: true, state: false, want: boolPtr(true)},
		{name: "disable", plan: false, state: true, want: boolPtr(false)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var diags diag.Diagnostics
			opts := subnetUpdateOpts(context.Background(), dnsSubnetModel(types.BoolValue(tc.plan)), dnsSubnetModel(types.BoolValue(tc.state)), &diags)
			if diags.HasError() {
				t.Fatal(diags)
			}
			switch {
			case tc.want == nil && opts.DNSPublishFixedIP != nil:
				t.Fatalf("dns_publish_fixed_ip sent (%v) although unchanged", *opts.DNSPublishFixedIP)
			case tc.want != nil && (opts.DNSPublishFixedIP == nil || *opts.DNSPublishFixedIP != *tc.want):
				t.Fatalf("dns_publish_fixed_ip = %v, want %v", opts.DNSPublishFixedIP, *tc.want)
			}
		})
	}
}

func boolPtr(b bool) *bool { return &b }
