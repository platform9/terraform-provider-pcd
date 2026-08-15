// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package resmgr

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

// resmgr stores whatever it is sent and applies defaults only to absent keys,
// so an unconfigured leaf must be omitted from the request body — never sent
// as a Go zero value, which the server would persist as "" / 0 and echo back
// forever as the "server value".
func TestClusterToAPI_OmitsUnconfiguredRebalancingLeaves(t *testing.T) {
	r := &clusterResource{}
	var diags diag.Diagnostics
	partial, _ := types.ObjectValue(rebalanceAttrTypes, map[string]attr.Value{
		"enabled": types.BoolValue(false), "rebalancing_strategy": types.StringNull(), "rebalancing_frequency_mins": types.Int64Null()})
	m := clusterModel{Name: types.StringValue("c"), AutoResourceRebalancing: partial,
		VMHighAvailability: types.ObjectNull(haAttrTypes), GPU: types.ObjectNull(clusterGPUAttrTypes), CPU: types.ObjectNull(clusterCPUAttrTypes)}
	b, _ := json.Marshal(r.toAPI(context.Background(), &m, &diags))
	body := string(b)
	if strings.Contains(body, "rebalancingStrategy") || strings.Contains(body, "rebalancingFrequencyMins") {
		t.Fatalf("unconfigured leaves must be ABSENT from the body, got: %s", body)
	}
	if !strings.Contains(body, `"autoResourceRebalancing":{"enabled":false}`) {
		t.Fatalf("expected enabled-only rebalancing object, got: %s", body)
	}
	// Every group must still be present: resmgr answers 500 to a missing group.
	for _, k := range []string{"vmHighAvailability", "autoResourceRebalancing", "gpu", "cpu"} {
		if !strings.Contains(body, `"`+k+`"`) {
			t.Fatalf("group %q must always be sent, got: %s", k, body)
		}
	}
}

func TestClusterToAPI_SendsConfiguredRebalancingLeaves(t *testing.T) {
	r := &clusterResource{}
	var diags diag.Diagnostics
	full, _ := types.ObjectValue(rebalanceAttrTypes, map[string]attr.Value{
		"enabled": types.BoolValue(true), "rebalancing_strategy": types.StringValue("vm_workload_consolidation"), "rebalancing_frequency_mins": types.Int64Value(20)})
	m := clusterModel{Name: types.StringValue("c"), AutoResourceRebalancing: full,
		VMHighAvailability: types.ObjectNull(haAttrTypes), GPU: types.ObjectNull(clusterGPUAttrTypes), CPU: types.ObjectNull(clusterCPUAttrTypes)}
	b, _ := json.Marshal(r.toAPI(context.Background(), &m, &diags))
	want := `"autoResourceRebalancing":{"enabled":true,"rebalancingStrategy":"vm_workload_consolidation","rebalancingFrequencyMins":20}`
	if !strings.Contains(string(b), want) {
		t.Fatalf("configured leaves must all be sent\n got:  %s\n want: …%s…", b, want)
	}
}
