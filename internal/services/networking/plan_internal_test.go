// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package networking

import (
	"context"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

// planUpdate runs the plan modifiers of r's top-level attribute name over an
// update from prior to planned, the way the framework does, and returns the
// attribute's planned value and whether a modifier forces replacement. prior
// and planned point to resource models. planned holds what the framework has
// planned before the modifiers run: the config's values, with every computed
// attribute the config leaves unset marked unknown. The config is planned with
// those unknowns as null.
func planUpdate(t *testing.T, r resource.Resource, name string, prior, planned any) (attr.Value, bool) {
	t.Helper()
	ctx := context.Background()
	s := schemaOf(t, r)
	state := newState(t, s, prior)
	plan := newPlan(t, s, planned)
	configRaw, err := tftypes.Transform(plan.Raw, func(_ *tftypes.AttributePath, v tftypes.Value) (tftypes.Value, error) {
		if v.IsKnown() {
			return v, nil
		}
		return tftypes.NewValue(v.Type(), nil), nil
	})
	if err != nil {
		t.Fatalf("building the config: %v", err)
	}
	config := tfsdk.Config{Schema: s, Raw: configRaw}
	p := path.Root(name)

	switch a := s.Attributes[name].(type) {
	case schema.BoolAttribute:
		var cv, pv, sv types.Bool
		getAttribute(t, config, p, &cv)
		getAttribute(t, plan, p, &pv)
		getAttribute(t, state, p, &sv)
		replace := false
		for _, m := range a.PlanModifiers {
			resp := &planmodifier.BoolResponse{PlanValue: pv}
			m.PlanModifyBool(ctx, planmodifier.BoolRequest{Path: p, Config: config, ConfigValue: cv, Plan: plan, PlanValue: pv, State: state, StateValue: sv}, resp)
			if resp.Diagnostics.HasError() {
				t.Fatalf("planning %s: %v", name, resp.Diagnostics)
			}
			pv, replace = resp.PlanValue, replace || resp.RequiresReplace
		}
		return pv, replace
	case schema.Int64Attribute:
		var cv, pv, sv types.Int64
		getAttribute(t, config, p, &cv)
		getAttribute(t, plan, p, &pv)
		getAttribute(t, state, p, &sv)
		replace := false
		for _, m := range a.PlanModifiers {
			resp := &planmodifier.Int64Response{PlanValue: pv}
			m.PlanModifyInt64(ctx, planmodifier.Int64Request{Path: p, Config: config, ConfigValue: cv, Plan: plan, PlanValue: pv, State: state, StateValue: sv}, resp)
			if resp.Diagnostics.HasError() {
				t.Fatalf("planning %s: %v", name, resp.Diagnostics)
			}
			pv, replace = resp.PlanValue, replace || resp.RequiresReplace
		}
		return pv, replace
	case schema.StringAttribute:
		var cv, pv, sv types.String
		getAttribute(t, config, p, &cv)
		getAttribute(t, plan, p, &pv)
		getAttribute(t, state, p, &sv)
		replace := false
		for _, m := range a.PlanModifiers {
			resp := &planmodifier.StringResponse{PlanValue: pv}
			m.PlanModifyString(ctx, planmodifier.StringRequest{Path: p, Config: config, ConfigValue: cv, Plan: plan, PlanValue: pv, State: state, StateValue: sv}, resp)
			if resp.Diagnostics.HasError() {
				t.Fatalf("planning %s: %v", name, resp.Diagnostics)
			}
			pv, replace = resp.PlanValue, replace || resp.RequiresReplace
		}
		return pv, replace
	default:
		t.Fatalf("planUpdate does not handle attribute %s of type %T", name, a)
		return nil, false
	}
}

// getAttribute reads the attribute at p from a plan, a state or a config.
func getAttribute(t *testing.T, from interface {
	GetAttribute(context.Context, path.Path, any) diag.Diagnostics
}, p path.Path, target any) {
	t.Helper()
	if d := from.GetAttribute(context.Background(), p, target); d.HasError() {
		t.Fatalf("reading %s: %v", p, d)
	}
}
