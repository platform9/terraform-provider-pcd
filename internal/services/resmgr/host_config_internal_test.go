// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

// Package resmgr (not resmgr_test) to reach labelValuesChanged, which is unexported.
package resmgr

import "testing"

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
