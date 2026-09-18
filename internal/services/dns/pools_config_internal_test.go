// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package dns

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// bind9Pool is the CE lab pool from the 2026-07-13 DNS validation runbook.
func bind9Pool() poolConfig {
	return poolConfig{
		Name:        "default",
		Description: "BIND9 on hyp1",
		NSRecords:   []nsRecord{{Hostname: "ns1.pcd.local.", Priority: 1}},
		Nameservers: []hostPort{{Host: "172.16.122.251", Port: 53}},
		Targets: []poolTarget{{
			Type:    "bind9",
			Masters: []hostPort{{Host: "172.16.122.251", Port: 5354}},
			Options: targetOptions{Host: "172.16.122.251", Port: 53, RNDCHost: "172.16.122.251", RNDCPort: 953, RNDCKeyFile: "/etc/designate/rndc.key"},
		}},
	}
}

// pdns4Pool is the reporter's configuration from PCD-9943.
func pdns4Pool() poolConfig {
	return poolConfig{
		Name:        "default",
		Description: "Cloudflare via pdns4-shim and external-dns",
		Attributes:  map[string]string{},
		NSRecords:   []nsRecord{{Hostname: "ns1.pcd-ce-lab.usmnblm01.rye.ninja.", Priority: 1}},
		Nameservers: []hostPort{{Host: "fd97:45c2:b3a1:f00::9280", Port: 53}},
		Targets: []poolTarget{{
			Type:        "pdns4",
			Description: "pdns4-shim",
			Masters:     []hostPort{{Host: "10.45.0.1", Port: 53}},
			Options:     targetOptions{Host: "10.45.60.1", Port: 53, APIEndpoint: "https://pdns4-shim.rye.ninja:443", APIToken: "example_token"},
		}},
	}
}

func issuePaths(issues []poolIssue) []string {
	out := make([]string, 0, len(issues))
	for _, i := range issues {
		out = append(out, i.Path)
	}
	return out
}

func TestValidatePoolsAcceptsBothBackends(t *testing.T) {
	for _, p := range []poolConfig{bind9Pool(), pdns4Pool()} {
		if errs, warns := validatePools([]poolConfig{p}); len(errs) != 0 || len(warns) != 0 {
			t.Fatalf("%s: unexpected issues: errors %v warnings %v", p.Targets[0].Type, errs, warns)
		}
	}
	// Designate's bind9 backend defaults rndc_host to 127.0.0.1 and rndc_port
	// to 953, and takes either an rndc key file or an rndc config file, so a
	// target that names only the config file is complete.
	p := bind9Pool()
	p.Targets[0].Options.RNDCHost, p.Targets[0].Options.RNDCPort, p.Targets[0].Options.RNDCKeyFile = "", 0, ""
	p.Targets[0].Options.RNDCConfigFile = "/etc/designate/rndc.conf"
	if errs, _ := validatePools([]poolConfig{p}); len(errs) != 0 {
		t.Fatalf("a bind9 target with rndc_config_file alone was rejected; designate-manage accepts it: %v", errs)
	}
}

// Every rule the reporter had to write as a Terraform variable validation
// (PCD-9943) has one case here; the path tells the user where to look.
func TestValidatePoolsRejects(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(p *poolConfig)
		path   string
	}{
		{"empty name", func(p *poolConfig) { p.Name = "" }, "pools[0].name"},
		{"ns_record without trailing dot", func(p *poolConfig) { p.NSRecords[0].Hostname = "ns1.pcd.local" }, "pools[0].ns_records[0].hostname"},
		{"ns_record priority 0", func(p *poolConfig) { p.NSRecords[0].Priority = 0 }, "pools[0].ns_records[0].priority"},
		{"no ns_records", func(p *poolConfig) { p.NSRecords = nil }, "pools[0].ns_records"},
		{"nameserver host not an IP", func(p *poolConfig) { p.Nameservers[0].Host = "ns1.pcd.local" }, "pools[0].nameservers[0].host"},
		{"nameserver port out of range", func(p *poolConfig) { p.Nameservers[0].Port = 70000 }, "pools[0].nameservers[0].port"},
		{"no nameservers", func(p *poolConfig) { p.Nameservers = nil }, "pools[0].nameservers"},
		{"no targets", func(p *poolConfig) { p.Targets = nil }, "pools[0].targets"},
		{"unknown target type", func(p *poolConfig) { p.Targets[0].Type = "powerdns" }, "pools[0].targets[0].type"},
		{"master host not an IP", func(p *poolConfig) { p.Targets[0].Masters[0].Host = "mdns.local" }, "pools[0].targets[0].masters[0].host"},
		{"master port 0", func(p *poolConfig) { p.Targets[0].Masters[0].Port = 0 }, "pools[0].targets[0].masters[0].port"},
		{"no masters", func(p *poolConfig) { p.Targets[0].Masters = nil }, "pools[0].targets[0].masters"},
		{"options host not an IP", func(p *poolConfig) { p.Targets[0].Options.Host = "bind.local" }, "pools[0].targets[0].options.host"},
		{"options port out of range", func(p *poolConfig) { p.Targets[0].Options.Port = 0 }, "pools[0].targets[0].options.port"},
		{"bind9 without rndc key or config file", func(p *poolConfig) { p.Targets[0].Options.RNDCKeyFile = "" }, "pools[0].targets[0].options"},
		{"bind9 rndc_port out of range", func(p *poolConfig) { p.Targets[0].Options.RNDCPort = 65536 }, "pools[0].targets[0].options.rndc_port"},
		{"bind9 with api options", func(p *poolConfig) { p.Targets[0].Options.APIToken = "x" }, "pools[0].targets[0].options"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := bind9Pool()
			tc.mutate(&p)
			errs, _ := validatePools([]poolConfig{p})
			for _, e := range errs {
				if e.Path == tc.path {
					return
				}
			}
			t.Fatalf("no error at %s; designate-manage would have been the first to complain. got %v", tc.path, issuePaths(errs))
		})
	}
	for _, tc := range []struct {
		name   string
		mutate func(p *poolConfig)
		path   string
	}{
		{"pdns4 without api_token", func(p *poolConfig) { p.Targets[0].Options.APIToken = "" }, "pools[0].targets[0].options"},
		{"pdns4 with rndc options", func(p *poolConfig) { p.Targets[0].Options.RNDCHost = "10.45.60.1" }, "pools[0].targets[0].options"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := pdns4Pool()
			tc.mutate(&p)
			errs, _ := validatePools([]poolConfig{p})
			for _, e := range errs {
				if e.Path == tc.path {
					return
				}
			}
			t.Fatalf("no error at %s; got %v", tc.path, issuePaths(errs))
		})
	}
	errs, _ := validatePools([]poolConfig{bind9Pool(), bind9Pool()})
	if len(errs) == 0 || errs[0].Path != "pools[1].name" {
		t.Fatalf("duplicate pool names not rejected: %v", issuePaths(errs))
	}
	if errs, _ := validatePools(nil); len(errs) == 0 {
		t.Fatalf("an empty pool list produced no error")
	}
}

// A full-length IPv6 master is valid YAML and valid for the pool tables, but
// designate-manage copies it into zone_masters.host, VARCHAR(32) (PCD-9946).
// That is a warning, not an error: a Designate with the column widened is fine.
func TestValidatePoolsWarnsOnLongMaster(t *testing.T) {
	p := pdns4Pool()
	p.Targets[0].Masters[0].Host = "fd97:45c2:b3a1:100:e481:3fff:fec5:249f" // 38 characters
	errs, warns := validatePools([]poolConfig{p})
	if len(errs) != 0 {
		t.Fatalf("a long master must not be an error: %v", errs)
	}
	if len(warns) != 1 || warns[0].Path != "pools[0].targets[0].masters[0].host" || !strings.Contains(warns[0].Msg, "PCD-9946") {
		t.Fatalf("expected one warning naming PCD-9946 at the master host, got %v", warns)
	}
	p.Targets[0].Masters[0].Host = "fd97:45c2:b3a1:100::5354" // 24 characters, the reporter's workaround
	if _, warns := validatePools([]poolConfig{p}); len(warns) != 0 {
		t.Fatalf("a 24-character master must not warn: %v", warns)
	}
}

// The rendered file must be what designate-manage pool update reads: one YAML
// document holding a list of pools, with the keys the upstream sample uses and
// nothing that was not configured.
func TestRenderPoolsYAML(t *testing.T) {
	out, err := renderPoolsYAML([]poolConfig{bind9Pool(), pdns4Pool()})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out, "---\n- name: default\n") {
		t.Fatalf("unexpected document start:\n%s", out)
	}
	var back []map[string]any
	if err := yaml.Unmarshal([]byte(out), &back); err != nil {
		t.Fatalf("rendered YAML does not parse back: %v\n%s", err, out)
	}
	if len(back) != 2 {
		t.Fatalf("rendered %d pools, want 2", len(back))
	}
	bind := back[0]
	if _, ok := bind["id"]; ok {
		t.Fatalf("id rendered although unset; designate-manage would look up a pool by an empty id")
	}
	if attrs, ok := bind["attributes"].(map[string]any); !ok || len(attrs) != 0 {
		t.Fatalf("attributes = %v, want an empty mapping (the upstream sample always writes attributes:)", bind["attributes"])
	}
	if _, ok := bind["also_notifies"]; ok {
		t.Fatalf("also_notifies rendered although unset")
	}
	target := bind["targets"].([]any)[0].(map[string]any)
	opts := target["options"].(map[string]any)
	for _, k := range []string{"host", "port", "rndc_host", "rndc_port", "rndc_key_file"} {
		if _, ok := opts[k]; !ok {
			t.Fatalf("bind9 options lack %s: %v", k, opts)
		}
	}
	for _, k := range []string{"api_endpoint", "api_token", "rndc_config_file"} {
		if _, ok := opts[k]; ok {
			t.Fatalf("bind9 options carry %s although unset", k)
		}
	}
	if got := opts["rndc_port"]; got != 953 {
		t.Fatalf("rndc_port = %v (%T), want the integer 953", got, got)
	}
	pdns := back[1]["targets"].([]any)[0].(map[string]any)["options"].(map[string]any)
	if pdns["api_endpoint"] != "https://pdns4-shim.rye.ninja:443" || pdns["api_token"] != "example_token" {
		t.Fatalf("pdns4 options = %v", pdns)
	}
	if _, ok := pdns["rndc_host"]; ok {
		t.Fatalf("pdns4 options carry rndc_host")
	}
	withID := bind9Pool()
	withID.ID = "794ccc2c-d751-44fe-b57f-8894c9f5c842"
	out, _ = renderPoolsYAML([]poolConfig{withID})
	if !strings.Contains(out, "\n  id: 794ccc2c-d751-44fe-b57f-8894c9f5c842\n") {
		t.Fatalf("pool id not rendered:\n%s", out)
	}
}
