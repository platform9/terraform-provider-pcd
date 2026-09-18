// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package resmgr_test

import (
	"context"
	"testing"

	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-go/tftypes"

	"github.com/platform9/terraform-provider-pcd/internal/acctest"
)

// These tests drive the real provider server through PlanResourceChange for
// pcd_host_config, with no lab: the provider is never configured (its Configure
// authenticates eagerly) and plan modifiers need no client. They pin what
// resmgr refuses to change on an existing host configuration (PCD-9803, probed
// on 2026.4): the interface an existing network label maps to, the name, and
// the cluster name all answer 400, so each must plan a replacement; adding or
// removing a label and changing an interface are accepted, so they must not.

// hostConfigPlanner holds the server and the resource's type for one test.
type hostConfigPlanner struct {
	t      *testing.T
	ctx    context.Context
	server tfprotov6.ProviderServer
	typ    tftypes.Object
}

func newHostConfigPlanner(t *testing.T) *hostConfigPlanner {
	t.Helper()
	ctx := context.Background()
	server, err := acctest.ProtoV6ProviderFactories["pcd"]()
	if err != nil {
		t.Fatalf("provider server: %v", err)
	}
	schemas, err := server.GetProviderSchema(ctx, &tfprotov6.GetProviderSchemaRequest{})
	if err != nil {
		t.Fatalf("GetProviderSchema: %v", err)
	}
	rs, ok := schemas.ResourceSchemas["pcd_host_config"]
	if !ok {
		t.Fatal("pcd_host_config is not registered")
	}
	typ, ok := rs.ValueType().(tftypes.Object)
	if !ok {
		t.Fatalf("pcd_host_config schema type is %T, want tftypes.Object", rs.ValueType())
	}
	return &hostConfigPlanner{t: t, ctx: ctx, server: server, typ: typ}
}

func str(s string) tftypes.Value { return tftypes.NewValue(tftypes.String, s) }

func labelsValue(m map[string]string) tftypes.Value {
	vals := map[string]tftypes.Value{}
	for k, v := range m {
		vals[k] = str(v)
	}
	return tftypes.NewValue(tftypes.Map{ElementType: tftypes.String}, vals)
}

// object builds a pcd_host_config value from the attributes in set; every other
// attribute is a typed null, which is what an attribute absent from a
// configuration looks like on the wire.
func (p *hostConfigPlanner) object(set map[string]tftypes.Value) tftypes.Value {
	vals := map[string]tftypes.Value{}
	for name, at := range p.typ.AttributeTypes {
		if v, ok := set[name]; ok {
			vals[name] = v
		} else {
			vals[name] = tftypes.NewValue(at, nil)
		}
	}
	return tftypes.NewValue(p.typ, vals)
}

// priorState is a host configuration as Read stores it: every attribute known,
// every interface on enp1s0, cluster_name bp-1, and the given labels.
func (p *hostConfigPlanner) priorState(labels map[string]string) tftypes.Value {
	return p.object(map[string]tftypes.Value{
		"id":                       str("hc-1"),
		"name":                     str("hc"),
		"mgmt_interface":           str("enp1s0"),
		"vm_console_interface":     str("enp1s0"),
		"host_liveness_interface":  str("enp1s0"),
		"tunneling_interface":      str("enp1s0"),
		"imagelib_interface":       str("enp1s0"),
		"live_migration_interface": str("enp1s0"),
		"network_labels":           labelsValue(labels),
		"cluster_name":             str("bp-1"),
		"gpu_pci":                  tftypes.NewValue(tftypes.List{ElementType: tftypes.String}, []tftypes.Value{}),
	})
}

// config is the minimal configuration matching priorState, with overrides.
// A nil override value removes the attribute from the configuration.
func (p *hostConfigPlanner) config(overrides map[string]*tftypes.Value) tftypes.Value {
	set := map[string]tftypes.Value{
		"name":           str("hc"),
		"mgmt_interface": str("enp1s0"),
		"network_labels": labelsValue(map[string]string{"physnet1": "enp1s0"}),
		"cluster_name":   str("bp-1"),
	}
	for k, v := range overrides {
		if v == nil {
			delete(set, k)
		} else {
			set[k] = *v
		}
	}
	return p.object(set)
}

func ptr(v tftypes.Value) *tftypes.Value { return &v }

// proposed is what Terraform core hands the provider as ProposedNewState: the
// configuration, with every attribute the configuration leaves null taken from
// the prior state.
func (p *hostConfigPlanner) proposed(prior, config tftypes.Value) tftypes.Value {
	var priorAttrs, configAttrs map[string]tftypes.Value
	if err := prior.As(&priorAttrs); err != nil {
		p.t.Fatalf("prior.As: %v", err)
	}
	if err := config.As(&configAttrs); err != nil {
		p.t.Fatalf("config.As: %v", err)
	}
	merged := map[string]tftypes.Value{}
	for name, cv := range configAttrs {
		if cv.IsNull() {
			merged[name] = priorAttrs[name]
		} else {
			merged[name] = cv
		}
	}
	return tftypes.NewValue(p.typ, merged)
}

func (p *hostConfigPlanner) dynamic(v tftypes.Value) *tfprotov6.DynamicValue {
	p.t.Helper()
	dv, err := tfprotov6.NewDynamicValue(p.typ, v)
	if err != nil {
		p.t.Fatalf("NewDynamicValue: %v", err)
	}
	return &dv
}

// plan runs PlanResourceChange from prior to config and returns the attribute
// paths that require replacement plus the planned network_labels value.
func (p *hostConfigPlanner) plan(prior, config tftypes.Value) (replace []string, plannedLabels tftypes.Value) {
	p.t.Helper()
	proposed := config
	if !prior.IsNull() {
		proposed = p.proposed(prior, config)
	}
	resp, err := p.server.PlanResourceChange(p.ctx, &tfprotov6.PlanResourceChangeRequest{
		TypeName:         "pcd_host_config",
		PriorState:       p.dynamic(prior),
		ProposedNewState: p.dynamic(proposed),
		Config:           p.dynamic(config),
	})
	if err != nil {
		p.t.Fatalf("PlanResourceChange: %v", err)
	}
	for _, d := range resp.Diagnostics {
		if d.Severity == tfprotov6.DiagnosticSeverityError {
			p.t.Fatalf("PlanResourceChange diagnostic: %s: %s", d.Summary, d.Detail)
		}
	}
	for _, path := range resp.RequiresReplace {
		replace = append(replace, path.String())
	}
	planned, err := resp.PlannedState.Unmarshal(p.typ)
	if err != nil {
		p.t.Fatalf("PlannedState.Unmarshal: %v", err)
	}
	var attrs map[string]tftypes.Value
	if err := planned.As(&attrs); err != nil {
		p.t.Fatalf("planned.As: %v", err)
	}
	return replace, attrs["network_labels"]
}

func attrPath(name string) string {
	return tftypes.NewAttributePath().WithAttributeName(name).String()
}

func (p *hostConfigPlanner) wantReplace(replace []string, attr string) {
	p.t.Helper()
	if len(replace) != 1 || replace[0] != attrPath(attr) {
		p.t.Fatalf("RequiresReplace = %v, want [%s]; resmgr answers 400 to this change, so an in-place update fails every apply", replace, attrPath(attr))
	}
}

func (p *hostConfigPlanner) wantInPlace(replace []string, labels, wantLabels tftypes.Value) {
	p.t.Helper()
	if len(replace) != 0 {
		p.t.Fatalf("RequiresReplace = %v, want none: resmgr accepts this change in place", replace)
	}
	if !labels.Equal(wantLabels) {
		p.t.Fatalf("planned network_labels = %s, want %s", labels, wantLabels)
	}
}

var oneLabel = map[string]string{"physnet1": "enp1s0"}

// A changed label value is the PCD-9803 report: a bond renamed under a physnet
// answers "Changing Network Label physnet1 is not allowed".
func TestHostConfigPlanReplacesOnNetworkLabelValueChange(t *testing.T) {
	p := newHostConfigPlanner(t)
	prior := p.priorState(oneLabel)
	config := p.config(map[string]*tftypes.Value{"network_labels": ptr(labelsValue(map[string]string{"physnet1": "enp2s0"}))})

	replace, _ := p.plan(prior, config)

	p.wantReplace(replace, "network_labels")
}

// Adding a label alongside a changed one is still a changed one.
func TestHostConfigPlanReplacesOnLabelAddedAndValueChanged(t *testing.T) {
	p := newHostConfigPlanner(t)
	prior := p.priorState(oneLabel)
	config := p.config(map[string]*tftypes.Value{"network_labels": ptr(labelsValue(map[string]string{"physnet1": "enp2s0", "physnet2": "enp3s0"}))})

	replace, _ := p.plan(prior, config)

	p.wantReplace(replace, "network_labels")
}

// resmgr accepts a new label on an existing configuration, so adding one must
// not recreate the configuration (and re-onboard every host assigned to it).
func TestHostConfigPlanUpdatesInPlaceOnLabelAdded(t *testing.T) {
	p := newHostConfigPlanner(t)
	prior := p.priorState(oneLabel)
	want := labelsValue(map[string]string{"physnet1": "enp1s0", "physnet2": "enp3s0"})
	config := p.config(map[string]*tftypes.Value{"network_labels": ptr(want)})

	replace, labels := p.plan(prior, config)

	p.wantInPlace(replace, labels, want)
}

// Removing a label is accepted in place as well.
func TestHostConfigPlanUpdatesInPlaceOnLabelRemoved(t *testing.T) {
	p := newHostConfigPlanner(t)
	prior := p.priorState(map[string]string{"physnet1": "enp1s0", "physnet2": "enp3s0"})
	want := labelsValue(oneLabel)
	config := p.config(map[string]*tftypes.Value{"network_labels": ptr(want)})

	replace, labels := p.plan(prior, config)

	p.wantInPlace(replace, labels, want)
}

// resmgr answers "Changing Name is not allowed".
func TestHostConfigPlanReplacesOnNameChange(t *testing.T) {
	p := newHostConfigPlanner(t)
	prior := p.priorState(oneLabel)
	config := p.config(map[string]*tftypes.Value{"name": ptr(str("hc-renamed"))})

	replace, _ := p.plan(prior, config)

	p.wantReplace(replace, "name")
}

// resmgr answers "Changing Cluster Name is not allowed".
func TestHostConfigPlanReplacesOnClusterNameChange(t *testing.T) {
	p := newHostConfigPlanner(t)
	prior := p.priorState(oneLabel)
	config := p.config(map[string]*tftypes.Value{"cluster_name": ptr(str("bp-2"))})

	replace, _ := p.plan(prior, config)

	p.wantReplace(replace, "cluster_name")
}

// Interfaces are mutable, so moving management traffic is an ordinary update.
func TestHostConfigPlanUpdatesInPlaceOnInterfaceChange(t *testing.T) {
	p := newHostConfigPlanner(t)
	prior := p.priorState(oneLabel)
	config := p.config(map[string]*tftypes.Value{"mgmt_interface": ptr(str("enp2s0"))})

	replace, labels := p.plan(prior, config)

	p.wantInPlace(replace, labels, labelsValue(oneLabel))
}

// network_labels and cluster_name are Optional+Computed. A configuration that
// never sets them plans them unknown whenever anything else changes, and
// RequiresReplace treats unknown as a change — so without UseStateForUnknown
// ahead of it, changing an interface would destroy and recreate the
// configuration (and drop the labels from the PUT, which resmgr answers with
// 500). The prior values must carry through and nothing may be replaced.
func TestHostConfigPlanKeepsUnconfiguredAttributesOnUnrelatedChange(t *testing.T) {
	p := newHostConfigPlanner(t)
	prior := p.priorState(oneLabel)
	config := p.config(map[string]*tftypes.Value{
		"mgmt_interface": ptr(str("enp2s0")),
		"network_labels": nil,
		"cluster_name":   nil,
	})

	replace, labels := p.plan(prior, config)

	p.wantInPlace(replace, labels, labelsValue(oneLabel))
}

// Creation has no prior value to differ from, so nothing can require replacement.
func TestHostConfigPlanCreateDoesNotRequireReplace(t *testing.T) {
	p := newHostConfigPlanner(t)
	nullPrior := tftypes.NewValue(p.typ, nil)

	replace, _ := p.plan(nullPrior, p.config(nil))

	if len(replace) != 0 {
		t.Fatalf("RequiresReplace = %v on create, want none", replace)
	}
}
