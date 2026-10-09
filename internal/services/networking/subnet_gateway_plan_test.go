// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package networking_test

import (
	"context"
	"testing"

	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-go/tftypes"

	"github.com/platform9/terraform-provider-pcd/internal/acctest"
)

// These tests drive the real provider server through ValidateResourceConfig
// and PlanResourceChange for pcd_networking_subnet, offline: the provider
// is never configured, and neither validation nor ModifyPlan needs a client.
// They pin the plans no_gateway produces once gateway_ip's UseStateForUnknown
// has run: a plan that kept the prior gateway while the apply removed it would
// fail with "Provider produced inconsistent result after apply".

type subnetGatewayPlanner struct {
	t      *testing.T
	ctx    context.Context
	server tfprotov6.ProviderServer
	typ    tftypes.Object
}

func newSubnetGatewayPlanner(t *testing.T) *subnetGatewayPlanner {
	t.Helper()
	ctx := context.Background()
	server, err := acctest.ProtoV6ProviderFactories["pcd"]()
	if err != nil {
		t.Fatalf("provider server: %v", err)
	}
	schemas, err := server.GetProviderSchema(ctx, &tfprotov6.GetProviderSchemaRequest{})
	if err != nil {
		t.Fatalf("GetProviderSchema: %v", err)
	}
	rs, ok := schemas.ResourceSchemas["pcd_networking_subnet"]
	if !ok {
		t.Fatal("pcd_networking_subnet is not registered")
	}
	typ, ok := rs.ValueType().(tftypes.Object)
	if !ok {
		t.Fatalf("schema type is %T, want tftypes.Object", rs.ValueType())
	}
	return &subnetGatewayPlanner{t: t, ctx: ctx, server: server, typ: typ}
}

// object builds a subnet value from set; every other attribute is a typed null.
func (p *subnetGatewayPlanner) object(set map[string]tftypes.Value) tftypes.Value {
	vals := map[string]tftypes.Value{}
	for name, at := range p.typ.AttributeTypes {
		if v, ok := set[name]; ok {
			vals[name] = v
		} else {
			vals[name] = tftypes.NewValue(at, nil)
		}
	}
	return tftypes.NewValue(p.typ, vals)
}

// prior is a subnet as Read stores it, on cidr with the given gateway and the
// allocation pool Neutron derives (its addresses do not matter to these plans,
// only that the subnet has one).
func (p *subnetGatewayPlanner) prior(cidr, gateway string) tftypes.Value {
	poolType := p.typ.AttributeTypes["allocation_pools"].(tftypes.List).ElementType.(tftypes.Object)
	return p.object(map[string]tftypes.Value{
		"id":              tftypes.NewValue(tftypes.String, "sub-1"),
		"network_id":      tftypes.NewValue(tftypes.String, "net-1"),
		"name":            tftypes.NewValue(tftypes.String, "sub"),
		"description":     tftypes.NewValue(tftypes.String, ""),
		"cidr":            tftypes.NewValue(tftypes.String, cidr),
		"ip_version":      tftypes.NewValue(tftypes.Number, 4),
		"gateway_ip":      tftypes.NewValue(tftypes.String, gateway),
		"no_gateway":      tftypes.NewValue(tftypes.Bool, gateway == ""),
		"enable_dhcp":     tftypes.NewValue(tftypes.Bool, true),
		"dns_nameservers": tftypes.NewValue(tftypes.List{ElementType: tftypes.String}, []tftypes.Value{}),
		"allocation_pools": tftypes.NewValue(tftypes.List{ElementType: poolType}, []tftypes.Value{
			tftypes.NewValue(poolType, map[string]tftypes.Value{
				"start": tftypes.NewValue(tftypes.String, "10.0.0.2"),
				"end":   tftypes.NewValue(tftypes.String, "10.0.0.254"),
			}),
		}),
		"dns_publish_fixed_ip": tftypes.NewValue(tftypes.Bool, false),
		"tenant_id":            tftypes.NewValue(tftypes.String, "p-1"),
		"tags":                 tftypes.NewValue(tftypes.Set{ElementType: tftypes.String}, []tftypes.Value{}),
		"region":               tftypes.NewValue(tftypes.String, "region-one"),
	})
}

// config is the minimal configuration for prior, plus extra attributes.
func (p *subnetGatewayPlanner) config(cidr string, extra map[string]tftypes.Value) tftypes.Value {
	set := map[string]tftypes.Value{
		"network_id": tftypes.NewValue(tftypes.String, "net-1"),
		"cidr":       tftypes.NewValue(tftypes.String, cidr),
	}
	for k, v := range extra {
		set[k] = v
	}
	return p.object(set)
}

func (p *subnetGatewayPlanner) dynamic(v tftypes.Value) *tfprotov6.DynamicValue {
	p.t.Helper()
	dv, err := tfprotov6.NewDynamicValue(p.typ, v)
	if err != nil {
		p.t.Fatalf("NewDynamicValue: %v", err)
	}
	return &dv
}

// plan runs PlanResourceChange from prior to config, with the proposed new
// state Terraform core builds (configuration, with nulls taken from prior,
// except allocation_pools), and returns the planned attributes.
func (p *subnetGatewayPlanner) plan(prior, config tftypes.Value) map[string]tftypes.Value {
	p.t.Helper()
	var priorAttrs, configAttrs map[string]tftypes.Value
	if err := prior.As(&priorAttrs); err != nil {
		p.t.Fatal(err)
	}
	if err := config.As(&configAttrs); err != nil {
		p.t.Fatal(err)
	}
	proposed := map[string]tftypes.Value{}
	for name, cv := range configAttrs {
		switch {
		case !cv.IsNull():
			proposed[name] = cv
		case name == "allocation_pools" && !priorAttrs[name].IsNull():
			// Core proposes null for an omitted Optional+Computed nested
			// attribute whose prior value holds non-computed fields (start and
			// end are Required), so the proposed state differs from prior and
			// the framework marks every computed attribute the configuration
			// omits unknown.
			proposed[name] = cv
		default:
			proposed[name] = priorAttrs[name]
		}
	}
	resp, err := p.server.PlanResourceChange(p.ctx, &tfprotov6.PlanResourceChangeRequest{
		TypeName:         "pcd_networking_subnet",
		PriorState:       p.dynamic(prior),
		ProposedNewState: p.dynamic(tftypes.NewValue(p.typ, proposed)),
		Config:           p.dynamic(config),
	})
	if err != nil {
		p.t.Fatalf("PlanResourceChange: %v", err)
	}
	for _, d := range resp.Diagnostics {
		if d.Severity == tfprotov6.DiagnosticSeverityError {
			p.t.Fatalf("PlanResourceChange diagnostic: %s: %s", d.Summary, d.Detail)
		}
	}
	planned, err := resp.PlannedState.Unmarshal(p.typ)
	if err != nil {
		p.t.Fatal(err)
	}
	var attrs map[string]tftypes.Value
	if err := planned.As(&attrs); err != nil {
		p.t.Fatal(err)
	}
	return attrs
}

func subnetGatewayWantString(t *testing.T, attrs map[string]tftypes.Value, name, want string) {
	t.Helper()
	v := attrs[name]
	if !v.IsKnown() {
		t.Fatalf("planned %s is unknown, want %q", name, want)
	}
	var got string
	if err := v.As(&got); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("planned %s = %q, want %q", name, got, want)
	}
}

func TestSubnetPlanNoGateway(t *testing.T) {
	p := newSubnetGatewayPlanner(t)
	yes := map[string]tftypes.Value{"no_gateway": tftypes.NewValue(tftypes.Bool, true)}
	no := map[string]tftypes.Value{"no_gateway": tftypes.NewValue(tftypes.Bool, false)}

	// Disabling the gateway in place must plan "", not the prior 10.0.0.1 that
	// UseStateForUnknown would carry.
	subnetGatewayWantString(t, p.plan(p.prior("10.0.0.0/24", "10.0.0.1"), p.config("10.0.0.0/24", yes)), "gateway_ip", "")

	// Restoring it plans Neutron's default for each family.
	subnetGatewayWantString(t, p.plan(p.prior("10.0.0.0/24", ""), p.config("10.0.0.0/24", no)), "gateway_ip", "10.0.0.1")
	subnetGatewayWantString(t, p.plan(p.prior("2001:db8::/64", ""), p.config("2001:db8::/64", no)), "gateway_ip", "2001:db8::")

	// no_gateway = false on a subnet that has a gateway keeps it.
	subnetGatewayWantString(t, p.plan(p.prior("10.0.0.0/24", "10.0.0.254"), p.config("10.0.0.0/24", no)), "gateway_ip", "10.0.0.254")

	// gateway_ip = "" next to no_gateway = true, the form an imported subnet
	// without a gateway reads back as, plans the same as no_gateway = true.
	both := map[string]tftypes.Value{
		"no_gateway": tftypes.NewValue(tftypes.Bool, true),
		"gateway_ip": tftypes.NewValue(tftypes.String, ""),
	}
	subnetGatewayWantString(t, p.plan(p.prior("10.0.0.0/24", "10.0.0.1"), p.config("10.0.0.0/24", both)), "gateway_ip", "")

	// Omitted on a subnet with a gateway, no_gateway plans a known false: an
	// unknown would show a change on every plan of a subnet that omits
	// allocation_pools.
	withGateway := p.plan(p.prior("10.0.0.0/24", "10.0.0.1"), p.config("10.0.0.0/24", nil))
	subnetGatewayWantString(t, withGateway, "gateway_ip", "10.0.0.1")
	var hasNoGateway bool
	if err := withGateway["no_gateway"].As(&hasNoGateway); err != nil || hasNoGateway {
		t.Fatalf("planned no_gateway = %v (%v), want false carried from state", withGateway["no_gateway"], err)
	}

	// Omitted, a gateway removed outside Terraform stays removed: nothing is
	// planned to change.
	attrs := p.plan(p.prior("10.0.0.0/24", ""), p.config("10.0.0.0/24", nil))
	subnetGatewayWantString(t, attrs, "gateway_ip", "")
	var noGateway bool
	if err := attrs["no_gateway"].As(&noGateway); err != nil || !noGateway {
		t.Fatalf("planned no_gateway = %v (%v), want true carried from state", attrs["no_gateway"], err)
	}
}

func TestSubnetValidateGateway(t *testing.T) {
	p := newSubnetGatewayPlanner(t)
	for name, tc := range map[string]struct {
		extra       map[string]tftypes.Value
		wantSummary string
	}{
		"empty gateway_ip": {
			extra:       map[string]tftypes.Value{"gateway_ip": tftypes.NewValue(tftypes.String, "")},
			wantSummary: "Invalid gateway_ip",
		},
		"gateway_ip with no_gateway": {
			extra: map[string]tftypes.Value{
				"gateway_ip": tftypes.NewValue(tftypes.String, "10.0.0.1"),
				"no_gateway": tftypes.NewValue(tftypes.Bool, true),
			},
			wantSummary: "Conflicting gateway settings",
		},
		"no_gateway alone": {
			extra: map[string]tftypes.Value{"no_gateway": tftypes.NewValue(tftypes.Bool, true)},
		},
		"empty gateway_ip with no_gateway": {
			extra: map[string]tftypes.Value{
				"gateway_ip": tftypes.NewValue(tftypes.String, ""),
				"no_gateway": tftypes.NewValue(tftypes.Bool, true),
			},
		},
	} {
		t.Run(name, func(t *testing.T) {
			resp, err := p.server.ValidateResourceConfig(p.ctx, &tfprotov6.ValidateResourceConfigRequest{
				TypeName: "pcd_networking_subnet",
				Config:   p.dynamic(p.config("10.0.0.0/24", tc.extra)),
			})
			if err != nil {
				t.Fatal(err)
			}
			var summaries []string
			for _, d := range resp.Diagnostics {
				if d.Severity == tfprotov6.DiagnosticSeverityError {
					summaries = append(summaries, d.Summary)
				}
			}
			if tc.wantSummary == "" {
				if len(summaries) != 0 {
					t.Fatalf("errors %v, want none", summaries)
				}
				return
			}
			if len(summaries) != 1 || summaries[0] != tc.wantSummary {
				t.Fatalf("errors %v, want [%s]", summaries, tc.wantSummary)
			}
		})
	}
}
