// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

// Package resmgr (not resmgr_test) to reach labelValuesChanged and
// hostConfigResource, which are unexported.
package resmgr

import (
	"context"
	"net/http"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

// resmgr refuses to change the interface an existing network label maps to but
// accepts labels being added or removed (probed on 2026.4). This helper is the
// whole of that rule: a wrong true recreates a host configuration resmgr would
// have updated, a wrong false plans an update resmgr answers with 400.
func TestLabelValuesChanged(t *testing.T) {
	for _, tc := range []struct {
		name        string
		state, plan map[string]string
		want        bool
	}{
		{name: "identical", state: map[string]string{"physnet1": "enp1s0"}, plan: map[string]string{"physnet1": "enp1s0"}},
		{name: "value changed", state: map[string]string{"physnet1": "enp1s0"}, plan: map[string]string{"physnet1": "enp2s0"}, want: true},
		{name: "key added", state: map[string]string{"physnet1": "enp1s0"}, plan: map[string]string{"physnet1": "enp1s0", "physnet2": "enp3s0"}},
		{name: "key removed", state: map[string]string{"physnet1": "enp1s0", "physnet2": "enp3s0"}, plan: map[string]string{"physnet1": "enp1s0"}},
		{name: "key renamed is a removal and an addition", state: map[string]string{"physnet2": "enp3s0"}, plan: map[string]string{"physnet9": "enp3s0"}},
		{name: "added and changed", state: map[string]string{"physnet1": "enp1s0"}, plan: map[string]string{"physnet1": "enp2s0", "physnet2": "enp3s0"}, want: true},
		{name: "both empty", state: map[string]string{}, plan: map[string]string{}},
		{name: "first labels", state: map[string]string{}, plan: map[string]string{"physnet1": "enp1s0"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := labelValuesChanged(tc.state, tc.plan); got != tc.want {
				t.Fatalf("labelValuesChanged(%v, %v) = %v, want %v", tc.state, tc.plan, got, tc.want)
			}
		})
	}
}

// An update whose read-back fails stays a warning, but must not return the
// plan's unknowns: gpu_pci has no UseStateForUnknown, so a config that leaves
// it unset plans it unknown on every update, and Terraform refuses it in
// state. The routedResmgr fake and its helpers are in
// failed_create_internal_test.go.
func TestHostConfigUpdateKeepsStateKnownWhenReadBackFails(t *testing.T) {
	t.Parallel()
	r := &hostConfigResource{config: newRoutedResmgr(t, resmgrRoutes{
		"PUT /v2/hostconfigs/hc-1": reply(http.StatusOK, ""),
		"GET /v2/hostconfigs/hc-1": badGateway,
	}).config}
	s := schemaOf(t, r)

	prior := hostConfigModel{
		ID:                     types.StringValue("hc-1"),
		Name:                   types.StringValue("hc"),
		MgmtInterface:          types.StringValue("enp1s0"),
		VMConsoleInterface:     types.StringValue("enp1s0"),
		HostLivenessInterface:  types.StringValue("enp1s0"),
		TunnelingInterface:     types.StringValue("enp1s0"),
		ImagelibInterface:      types.StringValue("enp1s0"),
		LiveMigrationInterface: types.StringValue(""),
		NetworkLabels:          types.MapValueMust(types.StringType, map[string]attr.Value{"physnet1": types.StringValue("enp1s0")}),
		ClusterName:            types.StringValue("bp-1"),
		GPUPci:                 types.ListValueMust(types.StringType, []attr.Value{}),
	}
	// Management traffic moves to enp2s0. The config leaves gpu_pci unset, so
	// the plan leaves it unknown.
	planned := prior
	planned.MgmtInterface = types.StringValue("enp2s0")
	planned.GPUPci = types.ListUnknown(types.StringType)

	resp := runUpdate(r, newPlan(t, s, &planned), newState(t, s, &prior))
	if resp.Diagnostics.HasError() || resp.Diagnostics.WarningsCount() != 1 {
		t.Fatalf("diagnostics = %v, want exactly one warning and no error", resp.Diagnostics)
	}
	if !resp.State.Raw.IsFullyKnown() {
		t.Fatalf("update state holds unknown values, which Terraform refuses: %v", resp.State.Raw)
	}
	var got hostConfigModel
	if d := resp.State.Get(context.Background(), &got); d.HasError() {
		t.Fatalf("reading the state: %v", d)
	}
	if got.MgmtInterface.ValueString() != "enp2s0" {
		t.Fatalf("update state mgmt_interface = %s, want enp2s0: the PUT applied it", got.MgmtInterface)
	}
}
