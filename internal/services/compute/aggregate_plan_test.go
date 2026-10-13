// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package compute_test

import (
	"context"
	"testing"

	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-go/tftypes"

	"github.com/platform9/terraform-provider-pcd/internal/acctest"
)

// Nova cannot remove an aggregate's availability zone (400 "does not support
// empty named availability zone"), so removing zone from the configuration
// must plan a replacement; setting a zone, or changing it, stays in place.
// This drives the real provider server's PlanResourceChange; no PCD is needed.
func TestAggregateZonePlan(t *testing.T) {
	ctx := context.Background()
	server, err := acctest.ProtoV6ProviderFactories["pcd"]()
	if err != nil {
		t.Fatalf("provider server: %v", err)
	}
	schemas, err := server.GetProviderSchema(ctx, &tfprotov6.GetProviderSchemaRequest{})
	if err != nil {
		t.Fatalf("GetProviderSchema: %v", err)
	}
	rs, ok := schemas.ResourceSchemas["pcd_compute_aggregate"]
	if !ok {
		t.Fatal("pcd_compute_aggregate is not registered")
	}
	typ := rs.ValueType().(tftypes.Object)
	s := func(v string) tftypes.Value { return tftypes.NewValue(tftypes.String, v) }
	nullStr := tftypes.NewValue(tftypes.String, nil)
	emptySet := tftypes.NewValue(tftypes.Set{ElementType: tftypes.String}, []tftypes.Value{})
	emptyMap := tftypes.NewValue(tftypes.Map{ElementType: tftypes.String}, map[string]tftypes.Value{})
	obj := func(zone tftypes.Value, computed bool) tftypes.Value {
		vals := map[string]tftypes.Value{
			"name": s("agg"), "zone": zone,
			"id": tftypes.NewValue(tftypes.String, nil), "region": tftypes.NewValue(tftypes.String, nil),
			"metadata": tftypes.NewValue(tftypes.Map{ElementType: tftypes.String}, nil),
			"hosts":    tftypes.NewValue(tftypes.Set{ElementType: tftypes.String}, nil),
		}
		if computed {
			vals["id"], vals["region"], vals["metadata"], vals["hosts"] = s("7"), s("region-one"), emptyMap, emptySet
		}
		return tftypes.NewValue(typ, vals)
	}
	dyn := func(v tftypes.Value) *tfprotov6.DynamicValue {
		dv, err := tfprotov6.NewDynamicValue(typ, v)
		if err != nil {
			t.Fatal(err)
		}
		return &dv
	}
	for _, tc := range []struct {
		name        string
		prior, conf tftypes.Value
		wantReplace bool
	}{
		{"zone removed", s("zone-1"), nullStr, true},
		{"zone set", nullStr, s("zone-1"), false},
		{"zone changed", s("zone-1"), s("zone-2"), false},
		{"zone unchanged", s("zone-1"), s("zone-1"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prior := obj(tc.prior, true)
			config := obj(tc.conf, false)
			// ProposedNewState: the configuration, with the computed attributes
			// the configuration leaves null taken from the prior state.
			proposed := obj(tc.conf, true)
			resp, err := server.PlanResourceChange(ctx, &tfprotov6.PlanResourceChangeRequest{
				TypeName: "pcd_compute_aggregate", PriorState: dyn(prior), ProposedNewState: dyn(proposed), Config: dyn(config),
			})
			if err != nil {
				t.Fatal(err)
			}
			for _, d := range resp.Diagnostics {
				if d.Severity == tfprotov6.DiagnosticSeverityError {
					t.Fatalf("plan diagnostic: %s: %s", d.Summary, d.Detail)
				}
			}
			replaced := false
			for _, p := range resp.RequiresReplace {
				if p.String() == tftypes.NewAttributePath().WithAttributeName("zone").String() {
					replaced = true
				}
			}
			if replaced != tc.wantReplace {
				t.Fatalf("zone %v -> %v: replace = %v, want %v (RequiresReplace %v)", tc.prior, tc.conf, replaced, tc.wantReplace, resp.RequiresReplace)
			}
		})
	}
}
