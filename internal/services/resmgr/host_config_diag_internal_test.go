// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

// Package resmgr (not resmgr_test) to reach hostConfigAPIDiagnostic, which is unexported.
package resmgr

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
)

func resmgrErr(code int, body string) error {
	return gophercloud.ErrUnexpectedResponseCode{Actual: code, Body: []byte(body), Expected: []int{200}, Method: "POST", URL: "https://pcd.example.com/resmgr/v2/hostconfigs"}
}

func resmgrMsg(code int, message string) error {
	return resmgrErr(code, fmt.Sprintf(`{"message": %q}`, message))
}

// Every body below is verbatim what resmgr answered on Community Edition 2026.4
// (probed 2026-09-15). The mapper turns each into an error on the attribute that
// fixes it; anything it does not recognize is left to the generic error, so a
// wrong mapping would send the user to the wrong attribute and a missing one
// only costs the hint.
func TestHostConfigAPIDiagnostic(t *testing.T) {
	for _, tc := range []struct {
		name    string
		err     error
		attr    string // "" means not mapped
		summary string
	}{
		{"blueprint omitted", resmgrMsg(404, "ClusterBlueprintNotFound: Cluster blueprint None is not found"), "cluster_name", "does not name a cluster blueprint"},
		{"blueprint unknown", resmgrMsg(404, "ClusterBlueprintNotFound: Cluster blueprint does-not-exist is not found"), "cluster_name", "does not name a cluster blueprint"},
		{"mgmt omitted", resmgrMsg(400, "HostconfBadConfigInput: Hostconfig parameter: mgmtInterface should have valid value for"), "mgmt_interface", "mgmt_interface must be set"},
		{"vm console omitted", resmgrMsg(400, "HostconfBadConfigInput: Hostconfig parameter: vmConsoleInterface should have valid value for"), "vm_console_interface", "vm_console_interface must be set"},
		{"host liveness omitted", resmgrMsg(400, "HostconfBadConfigInput: Hostconfig parameter: hostLivenessInterface should have valid value for"), "host_liveness_interface", "host_liveness_interface must be set"},
		{"tunneling omitted", resmgrMsg(400, "HostconfBadConfigInput: Hostconfig parameter: tunnelingInterface should have valid value for"), "tunneling_interface", "tunneling_interface must be set"},
		{"imagelib omitted", resmgrMsg(400, "HostconfBadConfigInput: Hostconfig parameter: imagelibInterface should have valid value for"), "imagelib_interface", "imagelib_interface must be set"},
		// Not required on 2026.4, but mapped in case a later release checks it.
		{"live migration omitted", resmgrMsg(400, "HostconfBadConfigInput: Hostconfig parameter: liveMigrationInterface should have valid value for"), "live_migration_interface", "live_migration_interface must be set"},
		{"labels omitted or empty", resmgrMsg(400, "HostconfBadConfigInput: Hostconfig parameter: networkLabels should have valid value for"), "network_labels", "at least one"},
		{"tunneling interface unlabeled", resmgrMsg(400, "HostconfBadConfigInput: Hostconfig parameter: physical net label for tunneling interface should have valid value for"), "network_labels", "tunneling interface"},
		{"gpu list rejected", resmgrMsg(400, "HostconfBadConfigInput: Hostconfig parameter: gpuPci items should not be empty should have valid value for"), "gpu_pci", "gpu_pci"},
		{"two labels on one interface", resmgrMsg(409, "HostIntfConflict:  Host Interface enp1s0 is already used for another physical network"), "network_labels", "at most one label"},
		{"label value changed on update", resmgrMsg(400, "HostconfUpdateFail: Hostconfig update failed, Changing Network Label physnet1 is not allowed"), "network_labels", "cannot be changed in place"},
		{"renamed on update", resmgrMsg(400, "HostconfUpdateFail: Hostconfig update failed, Changing Name is not allowed"), "name", "cannot be changed in place"},
		{"blueprint changed on update", resmgrMsg(400, "HostconfUpdateFail: Hostconfig update failed, Changing Cluster Name  is not allowed"), "cluster_name", "cannot be changed in place"},
		{"wrapped error still maps", fmt.Errorf("resmgr: %w", resmgrMsg(400, "HostconfBadConfigInput: Hostconfig parameter: mgmtInterface should have valid value for")), "mgmt_interface", "mgmt_interface must be set"},

		// Left to the generic error. A known message under another status code
		// is not trusted either: the rules match on both.
		{"server error", resmgrErr(500, `{"code": 500, "title": "Internal Server Error", "description": "Internal Server Error"}`), "", ""},
		{"blueprint message under 400", resmgrMsg(400, "ClusterBlueprintNotFound: Cluster blueprint None is not found"), "", ""},
		{"conflict message under 400", resmgrMsg(400, "HostIntfConflict:  Host Interface enp1s0 is already used for another physical network"), "", ""},
		{"unknown parameter", resmgrMsg(400, "HostconfBadConfigInput: Hostconfig parameter: fooBar should have valid value for"), "", ""},
		{"non-JSON body", resmgrErr(502, "<html>bad gateway</html>"), "", ""},
		{"not a response error", errors.New("dial tcp: connection refused"), "", ""},
		{"nil", nil, "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := hostConfigAPIDiagnostic("creating", tc.err)
			if tc.attr == "" {
				if d != nil {
					t.Fatalf("mapped %v to %s; the generic error was expected", tc.err, d.Summary())
				}
				return
			}
			if d == nil {
				t.Fatalf("not mapped: %v", tc.err)
			}
			withPath, ok := d.(diag.DiagnosticWithPath)
			if !ok || !withPath.Path().Equal(path.Root(tc.attr)) {
				t.Fatalf("diagnostic is not on %s: %T %v", tc.attr, d, d)
			}
			if d.Severity() != diag.SeverityError {
				t.Fatalf("severity = %v, want error", d.Severity())
			}
			if !strings.HasPrefix(d.Summary(), "resmgr: creating host config: ") || !strings.Contains(d.Summary(), tc.summary) {
				t.Fatalf("summary = %q, want the operation prefix and %q", d.Summary(), tc.summary)
			}
			var raw gophercloud.ErrUnexpectedResponseCode
			if errors.As(tc.err, &raw) {
				var body struct {
					Message string `json:"message"`
				}
				if err := json.Unmarshal(raw.Body, &body); err != nil {
					t.Fatalf("test body is not JSON: %v", err)
				}
				if !strings.Contains(d.Detail(), body.Message) {
					t.Fatalf("detail %q does not carry the resmgr message %q", d.Detail(), body.Message)
				}
			}
		})
	}
}

// The operation word is the caller's, so an update refusal reads as one.
func TestHostConfigAPIDiagnosticUsesOperation(t *testing.T) {
	d := hostConfigAPIDiagnostic("updating", resmgrMsg(409, "HostIntfConflict:  Host Interface enp1s0 is already used for another physical network"))
	if d == nil || !strings.HasPrefix(d.Summary(), "resmgr: updating host config: ") {
		t.Fatalf("summary = %v, want the updating prefix", d)
	}
}
