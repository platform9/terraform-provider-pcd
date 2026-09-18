// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package resmgr_test

import (
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

// validate runs ValidateResourceConfig for pcd_host_config and returns the
// error diagnostics, each as "<attribute path>: <summary>".
func (p *hostConfigPlanner) validate(config tftypes.Value) []string {
	p.t.Helper()
	resp, err := p.server.ValidateResourceConfig(p.ctx, &tfprotov6.ValidateResourceConfigRequest{
		TypeName: "pcd_host_config",
		Config:   p.dynamic(config),
	})
	if err != nil {
		p.t.Fatalf("ValidateResourceConfig: %v", err)
	}
	var errs []string
	for _, d := range resp.Diagnostics {
		if d.Severity != tfprotov6.DiagnosticSeverityError {
			continue
		}
		at := "<resource>"
		if d.Attribute != nil {
			at = d.Attribute.String()
		}
		errs = append(errs, at+": "+d.Summary)
	}
	return errs
}

// resmgr accepts a host configuration named "" (probed on 2026.4), and name is
// the attribute a replacement is keyed on, so an empty name is a configuration
// mistake the provider refuses before anything reaches resmgr.
func TestHostConfigValidateRejectsEmptyName(t *testing.T) {
	p := newHostConfigPlanner(t)

	errs := p.validate(p.config(map[string]*tftypes.Value{"name": ptr(str(""))}))

	if len(errs) != 1 || !strings.HasPrefix(errs[0], attrPath("name")) {
		t.Fatalf("errors = %v, want exactly one at name", errs)
	}
}

func TestHostConfigValidateAcceptsName(t *testing.T) {
	p := newHostConfigPlanner(t)
	for name, cfg := range map[string]tftypes.Value{
		"known":   p.config(nil),
		"unknown": p.config(map[string]*tftypes.Value{"name": ptr(tftypes.NewValue(tftypes.String, tftypes.UnknownValue))}),
	} {
		if errs := p.validate(cfg); len(errs) != 0 {
			t.Fatalf("%s name: errors = %v, want none", name, errs)
		}
	}
}
