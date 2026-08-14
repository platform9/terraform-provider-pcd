// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

// This file is in package resmgr (not resmgr_test) to reach canonicalJSON,
// which is unexported.
package resmgr

import (
	"encoding/json"
	"testing"
)

// resmgr echoes storage backends with its own spacing and key order. Unless
// the read-back is canonicalised, storage_backends_json differs textually from
// the configured jsonencode() value and every plan reports an update that
// never converges.
func TestCanonicalJSON(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		want string
	}{
		{
			name: "resmgr spacing and insertion order match jsonencode output",
			in:   `{"synology": {"synology": {"config": {"driver_use_ssl": false, "target_protocol": "iscsi", "synology_admin_port": "5000"}, "driver": "SynoISCSIDriver"}}}`,
			want: `{"synology":{"synology":{"config":{"driver_use_ssl":false,"synology_admin_port":"5000","target_protocol":"iscsi"},"driver":"SynoISCSIDriver"}}}`,
		},
		{"already canonical is unchanged", `{"a":1,"b":2}`, `{"a":1,"b":2}`},
		{"keys are sorted", `{"c":3,"a":1,"b":2}`, `{"a":1,"b":2,"c":3}`},
		{"whitespace is stripped", "{\n  \"a\" : 1\n}", `{"a":1}`},
		{"array order is preserved", `{"x": [3, 1, 2]}`, `{"x":[3,1,2]}`},
		{"nested objects are sorted too", `{"o":{"z":1,"y":{"b":1,"a":2}}}`, `{"o":{"y":{"a":2,"b":1},"z":1}}`},
		{"types survive", `{"s":"1","n":1,"f":1.5,"b":true,"z":null}`, `{"b":true,"f":1.5,"n":1,"s":"1","z":null}`},

		// Never destroy a value we failed to understand.
		{"invalid JSON passes through untouched", `not json at all`, `not json at all`},
		{"empty input passes through", ``, ``},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := canonicalJSON(json.RawMessage(tc.in))
			if got != tc.want {
				t.Errorf("canonicalJSON(%q)\n got: %s\nwant: %s", tc.in, got, tc.want)
			}
		})
	}
}

// Canonicalising twice must equal canonicalising once, or state could oscillate.
func TestCanonicalJSONIsIdempotent(t *testing.T) {
	in := `{"synology": {"synology": {"config": {"b": 2, "a": 1}, "driver": "d"}}}`
	once := canonicalJSON(json.RawMessage(in))
	twice := canonicalJSON(json.RawMessage(once))
	if once != twice {
		t.Errorf("not idempotent:\n once: %s\ntwice: %s", once, twice)
	}
}
