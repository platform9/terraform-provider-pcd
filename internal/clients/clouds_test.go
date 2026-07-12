// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package clients

import (
	"os"
	"path/filepath"
	"testing"
)

const sampleCloudsYAML = `
clouds:
  pcd:
    region_name: Infra
    verify: false
    cacert: /etc/pki/ca.pem
    auth:
      auth_url: https://pcd.example.com/keystone/v3
      username: admin
      password: s3cret
      project_name: service
      user_domain_name: Default
      domain_name: Default
      application_credential_id: ""
  secure-cloud:
    auth:
      auth_url: https://other.example.com/v3
      username: bob
      password: pw
      project_id: abc123
      user_domain_id: d1
      project_domain_id: d2
`

// writeCloudsYAML writes the sample file and points OS_CLIENT_CONFIG_FILE at it.
func writeCloudsYAML(t *testing.T, body string) {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "clouds.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatalf("writing fixture: %v", err)
	}
	t.Setenv("OS_CLIENT_CONFIG_FILE", p)
}

func TestLoadCloud_basic(t *testing.T) {
	writeCloudsYAML(t, sampleCloudsYAML)

	cc, err := LoadCloud("pcd")
	if err != nil {
		t.Fatalf("LoadCloud: %v", err)
	}

	checks := map[string]struct{ got, want string }{
		"auth_url":            {cc.AuthURL, "https://pcd.example.com/keystone/v3"},
		"region":              {cc.Region, "Infra"},
		"username":            {cc.Username, "admin"},
		"password":            {cc.Password, "s3cret"},
		"project_name":        {cc.TenantName, "service"},
		"user_domain_name":    {cc.UserDomainName, "Default"},
		"project_domain_name": {cc.ProjectDomainName, "Default"}, // falls back to domain_name
		"cacert":              {cc.CACertFile, "/etc/pki/ca.pem"},
	}
	for name, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %q, want %q", name, c.got, c.want)
		}
	}
	if !cc.HasInsecure || !cc.Insecure {
		t.Errorf("verify:false should set Insecure=true (HasInsecure=%v Insecure=%v)", cc.HasInsecure, cc.Insecure)
	}
}

func TestLoadCloud_projectIDAndDistinctDomains(t *testing.T) {
	writeCloudsYAML(t, sampleCloudsYAML)

	cc, err := LoadCloud("secure-cloud")
	if err != nil {
		t.Fatalf("LoadCloud: %v", err)
	}
	if cc.TenantID != "abc123" {
		t.Errorf("project_id = %q, want abc123", cc.TenantID)
	}
	if cc.UserDomainID != "d1" || cc.ProjectDomainID != "d2" {
		t.Errorf("distinct domains not honored: user=%q project=%q", cc.UserDomainID, cc.ProjectDomainID)
	}
	// verify absent => HasInsecure false, Insecure false (do not clobber other tiers).
	if cc.HasInsecure {
		t.Errorf("verify absent should leave HasInsecure=false, got true")
	}
}

func TestLoadCloud_missingCloud(t *testing.T) {
	writeCloudsYAML(t, sampleCloudsYAML)
	if _, err := LoadCloud("does-not-exist"); err == nil {
		t.Fatal("expected error for missing cloud name")
	}
}

func TestLoadCloud_malformed(t *testing.T) {
	writeCloudsYAML(t, "clouds: [this is not a map")
	if _, err := LoadCloud("pcd"); err == nil {
		t.Fatal("expected parse error for malformed YAML")
	}
}

func TestLoadCloud_noFile(t *testing.T) {
	// Point at a non-existent file and rely on the other search paths being absent
	// in the test environment is fragile; instead point the env var at a missing
	// path and ensure that specific path fails to load. Because ./clouds.yaml or
	// system paths could theoretically exist, only assert when none resolve.
	t.Setenv("OS_CLIENT_CONFIG_FILE", filepath.Join(t.TempDir(), "absent.yaml"))
	if _, err := LoadCloud("pcd"); err == nil {
		if _, statErr := os.Stat("clouds.yaml"); statErr != nil {
			t.Fatal("expected error when no clouds.yaml is resolvable")
		}
	}
}
