// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

// Internal package (not resmgr_test) to reach mergeObject / mergeConfiguredCluster.
package resmgr

import (
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

func rb(enabled attr.Value, strategy attr.Value, freq attr.Value) types.Object {
	o, d := types.ObjectValue(rebalanceAttrTypes, map[string]attr.Value{
		"enabled":                    enabled,
		"rebalancing_strategy":       strategy,
		"rebalancing_frequency_mins": freq,
	})
	if d.HasError() {
		panic(d)
	}
	return o
}

// What a live read-back yields once toAPI sends only configured leaves:
// resmgr stores exactly what it receives and applies its default frequency
// (10) only to an ABSENT key; it never returns a strategy it was not given,
// so setState decodes that leaf as null rather than "".
var serverDisabled = rb(types.BoolValue(false), types.StringNull(), types.Int64Value(10))
var serverEnabled = rb(types.BoolValue(true), types.StringValue("vm_workload_consolidation"), types.Int64Value(20))

func TestMergeObject_RebalancingShapes(t *testing.T) {
	for _, tc := range []struct {
		name       string
		configured types.Object
		server     types.Object
		want       types.Object
	}{
		{
			// Shape 1: user wrote { enabled = false }. `enabled` must be the
			// configured value; the leaves left null take the server's, so the
			// object is consistent with config AND carries the server defaults.
			name:       "partial block: enabled=false keeps configured enabled, adopts server default freq",
			configured: rb(types.BoolValue(false), types.StringNull(), types.Int64Null()),
			server:     serverDisabled,
			want:       rb(types.BoolValue(false), types.StringNull(), types.Int64Value(10)),
		},
		{
			// Shape 2: fully specified — every configured leaf wins.
			name:       "full block: all configured leaves win over server",
			configured: rb(types.BoolValue(true), types.StringValue("vm_workload_consolidation"), types.Int64Value(20)),
			server:     serverEnabled,
			want:       rb(types.BoolValue(true), types.StringValue("vm_workload_consolidation"), types.Int64Value(20)),
		},
		{
			// Shape 3: omitted entirely (Computed) — server value adopted whole.
			name:       "omitted block: server value adopted as-is",
			configured: types.ObjectNull(rebalanceAttrTypes),
			server:     serverDisabled,
			want:       serverDisabled,
		},
		{
			name:       "unknown configured (first plan) behaves like omitted",
			configured: types.ObjectUnknown(rebalanceAttrTypes),
			server:     serverDisabled,
			want:       serverDisabled,
		},
		{
			// A configured value must never be overwritten by a server one, even
			// when they disagree — that disagreement is a real diff the NEXT plan
			// should surface as an update, not silently absorbed into state.
			name:       "configured beats server on disagreement",
			configured: rb(types.BoolValue(true), types.StringValue("node_resource_consolidation"), types.Int64Value(30)),
			server:     serverEnabled,
			want:       rb(types.BoolValue(true), types.StringValue("node_resource_consolidation"), types.Int64Value(30)),
		},
		{
			name:       "null server object yields configured unchanged",
			configured: rb(types.BoolValue(false), types.StringNull(), types.Int64Null()),
			server:     types.ObjectNull(rebalanceAttrTypes),
			want:       rb(types.BoolValue(false), types.StringNull(), types.Int64Null()),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := mergeObject(tc.server, tc.configured)
			if !got.Equal(tc.want) {
				t.Errorf("mergeObject\n got:  %s\n want: %s", got, tc.want)
			}
		})
	}
}

// vm_high_availability has a single leaf, which is why it never showed the
// bug — but it must go through the same path and stay correct.
func TestMergeObject_SingleLeafHA(t *testing.T) {
	ha := func(v attr.Value) types.Object {
		o, _ := types.ObjectValue(haAttrTypes, map[string]attr.Value{"enabled": v})
		return o
	}
	got := mergeObject(ha(types.BoolValue(false)), ha(types.BoolValue(false)))
	if !got.Equal(ha(types.BoolValue(false))) {
		t.Errorf("HA merge changed a matching value: %s", got)
	}
	got = mergeObject(ha(types.BoolValue(true)), types.ObjectNull(haAttrTypes))
	if !got.Equal(ha(types.BoolValue(true))) {
		t.Errorf("omitted HA should adopt server: %s", got)
	}
}

// Merging must be idempotent: merging state with itself is a no-op, or a
// refresh loop would oscillate.
func TestMergeConfiguredCluster_Idempotent(t *testing.T) {
	m := clusterModel{
		Name:                    types.StringValue("c"),
		VMHighAvailability:      types.ObjectNull(haAttrTypes),
		AutoResourceRebalancing: rb(types.BoolValue(false), types.StringNull(), types.Int64Value(10)),
		GPU:                     types.ObjectNull(clusterGPUAttrTypes),
		CPU:                     types.ObjectNull(clusterCPUAttrTypes),
	}
	once := m
	mergeConfiguredCluster(&once, &m)
	twice := once
	mergeConfiguredCluster(&twice, &once)
	if !once.AutoResourceRebalancing.Equal(twice.AutoResourceRebalancing) {
		t.Errorf("not idempotent:\n once:  %s\n twice: %s", once.AutoResourceRebalancing, twice.AutoResourceRebalancing)
	}
}
