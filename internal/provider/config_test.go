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
