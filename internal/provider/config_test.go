// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/types"
)

func TestStrval_emptyConfigFallsToEnv(t *testing.T) {
	t.Setenv("PCD_TEST_STRVAL", "fromenv")

	cases := map[string]struct {
		v    types.String
		want string
	}{
		"set config wins":       {types.StringValue("cfg"), "cfg"},
		"empty config -> env":   {types.StringValue(""), "fromenv"},
		"null config -> env":    {types.StringNull(), "fromenv"},
		"unknown config -> env": {types.StringUnknown(), "fromenv"},
	}
	for name, c := range cases {
		if got := strval(c.v, "PCD_TEST_STRVAL"); got != c.want {
			t.Errorf("%s: strval = %q, want %q", name, got, c.want)
		}
	}
}

func TestPick_precedence(t *testing.T) {
	t.Setenv("PCD_TEST_PICK", "envval")

	// config > env > clouds.yaml fallback.
	if got := pick(types.StringValue("cfg"), "cloudval", "PCD_TEST_PICK"); got != "cfg" {
		t.Errorf("explicit config should win: got %q", got)
	}
	if got := pick(types.StringNull(), "cloudval", "PCD_TEST_PICK"); got != "envval" {
		t.Errorf("env should beat clouds.yaml: got %q", got)
	}
	if got := pick(types.StringNull(), "cloudval", "PCD_TEST_PICK_UNSET"); got != "cloudval" {
		t.Errorf("clouds.yaml fallback when env unset: got %q", got)
	}
	// The bug that adversarial review caught: an explicitly-empty config value must
	// fall through to env, not skip straight to the clouds.yaml fallback.
	if got := pick(types.StringValue(""), "cloudval", "PCD_TEST_PICK"); got != "envval" {
		t.Errorf("empty config should fall to env, not clouds.yaml: got %q", got)
	}
}

// The scope prefers a project ID over a name, so an ID taken from a lower
// source than the name would quietly scope the provider to another project.
func TestPickProject_resolvesIDAndNameTogether(t *testing.T) {
	for _, tc := range []struct {
		name               string
		env                map[string]string
		cfgID, cfgName     types.String
		cloudID, cloudName string
		wantID, wantName   string
	}{
		{
			name:     "tenant_name in config ignores the exported project ID",
			env:      map[string]string{"OS_PROJECT_ID": "env-id", "OS_PROJECT_NAME": "env-name"},
			cfgID:    types.StringNull(),
			cfgName:  types.StringValue("cfg-name"),
			wantName: "cfg-name",
		},
		{
			name:    "tenant_id in config ignores the exported project name",
			env:     map[string]string{"OS_PROJECT_ID": "env-id", "OS_PROJECT_NAME": "env-name"},
			cfgID:   types.StringValue("cfg-id"),
			cfgName: types.StringNull(),
			wantID:  "cfg-id",
		},
		{
			name:     "tenant_name in config ignores the legacy OS_TENANT_ID",
			env:      map[string]string{"OS_TENANT_ID": "env-id"},
			cfgID:    types.StringNull(),
			cfgName:  types.StringValue("cfg-name"),
			wantName: "cfg-name",
		},
		{
			name:     "both exported and neither in config",
			env:      map[string]string{"OS_PROJECT_ID": "env-id", "OS_PROJECT_NAME": "env-name"},
			cfgID:    types.StringNull(),
			cfgName:  types.StringNull(),
			cloudID:  "cloud-id",
			wantID:   "env-id",
			wantName: "env-name",
		},
		{
			name:     "an exported project name ignores the clouds.yaml project ID",
			env:      map[string]string{"OS_PROJECT_NAME": "env-name"},
			cfgID:    types.StringNull(),
			cfgName:  types.StringNull(),
			cloudID:  "cloud-id",
			wantName: "env-name",
		},
		{
			name:     "the legacy OS_TENANT_NAME ignores the clouds.yaml project ID",
			env:      map[string]string{"OS_TENANT_NAME": "env-name"},
			cfgID:    types.StringNull(),
			cfgName:  types.StringNull(),
			cloudID:  "cloud-id",
			wantName: "env-name",
		},
		{
			name:     "tenant_name in config ignores the clouds.yaml project ID",
			cfgID:    types.StringNull(),
			cfgName:  types.StringValue("cfg-name"),
			cloudID:  "cloud-id",
			wantName: "cfg-name",
		},
		{
			name:      "clouds.yaml when neither config nor env sets a project",
			cfgID:     types.StringNull(),
			cfgName:   types.StringNull(),
			cloudID:   "cloud-id",
			cloudName: "cloud-name",
			wantID:    "cloud-id",
			wantName:  "cloud-name",
		},
		{
			name:     "an empty tenant_id in config counts as unset",
			env:      map[string]string{"OS_PROJECT_NAME": "env-name"},
			cfgID:    types.StringValue(""),
			cfgName:  types.StringNull(),
			cloudID:  "cloud-id",
			wantName: "env-name",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Clear every project variable so the caller's shell cannot leak in.
			for _, e := range []string{"OS_PROJECT_ID", "OS_TENANT_ID", "OS_PROJECT_NAME", "OS_TENANT_NAME"} {
				t.Setenv(e, tc.env[e])
			}
			gotID, gotName := pickProject(tc.cfgID, tc.cfgName, tc.cloudID, tc.cloudName)
			if gotID != tc.wantID || gotName != tc.wantName {
				t.Errorf("pickProject = (%q, %q), want (%q, %q)", gotID, gotName, tc.wantID, tc.wantName)
			}
		})
	}
}
