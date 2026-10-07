// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package dns

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

// planComputedInt64 returns what the framework plans for name, a Computed
// Int64 attribute the config cannot set, on an update from prior to planned:
// unknown, unless one of the attribute's plan modifiers replaces it.
func planComputedInt64(t *testing.T, s schema.Schema, name string, prior, planned *zoneModel) types.Int64 {
	t.Helper()
	ctx := context.Background()
	state, plan := newState(t, s, prior), newPlan(t, s, planned)
	config := tfsdk.Config{Schema: s, Raw: plan.Raw}
	p := path.Root(name)
	var sv types.Int64
	if d := state.GetAttribute(ctx, p, &sv); d.HasError() {
		t.Fatalf("reading %s: %v", name, d)
	}
	pv := types.Int64Unknown()
	for _, m := range s.Attributes[name].(schema.Int64Attribute).PlanModifiers {
		resp := &planmodifier.Int64Response{PlanValue: pv}
		m.PlanModifyInt64(ctx, planmodifier.Int64Request{Path: p, Config: config, ConfigValue: types.Int64Null(),
			Plan: plan, PlanValue: pv, State: state, StateValue: sv}, resp)
		if resp.Diagnostics.HasError() {
			t.Fatalf("planning %s: %v", name, resp.Diagnostics)
		}
		pv = resp.PlanValue
	}
	return pv
}

// Designate increments a zone's SOA serial on every update. serial used to
// plan its prior value, so the read-back after an update returned a serial the
// plan did not hold, and Terraform failed the apply with "Provider produced
// inconsistent result after apply". The plan must leave serial unknown.
func TestZoneUpdateAcceptsTheNewSerial(t *testing.T) {
	t.Parallel()
	updatedZoneJSON := strings.NewReplacer(`"ttl": 3600`, `"ttl": 7200`, `"serial": 1`, `"serial": 2`).Replace(activeZoneJSON)
	routes := zoneRoutes()
	routes["GET "+zonePath] = reply(http.StatusOK, updatedZoneJSON)
	r := &zoneResource{config: newFakeDesignate(t, routes).config}
	s := schemaOf(t, r)

	prior := activeZone(t)
	planned := prior
	planned.TTL = types.Int64Value(7200)
	planned.Serial = planComputedInt64(t, s, "serial", &prior, &planned)

	resp := runUpdate(r, newPlan(t, s, &planned), newState(t, s, &prior))
	if resp.Diagnostics.HasError() {
		t.Fatalf("update: %v", resp.Diagnostics)
	}
	var got zoneModel
	if d := resp.State.Get(context.Background(), &got); d.HasError() {
		t.Fatalf("reading the state: %v", d)
	}
	if got.Serial.ValueInt64() != 2 {
		t.Fatalf("update state serial = %s, want Designate's new serial 2", got.Serial)
	}
	if !planned.Serial.IsUnknown() && !planned.Serial.Equal(got.Serial) {
		t.Fatalf("serial was planned as %s and applied as %s: Terraform reports an inconsistent result",
			planned.Serial, got.Serial)
	}
}
