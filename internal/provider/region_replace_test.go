// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"context"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

// objectWithRegion returns an object of schema s whose attributes are all null
// except region.
func objectWithRegion(ctx context.Context, s schema.Schema, region string) tftypes.Value {
	typ := s.Type().TerraformType(ctx).(tftypes.Object)
	vals := map[string]tftypes.Value{}
	for name, at := range typ.AttributeTypes {
		vals[name] = tftypes.NewValue(at, nil)
	}
	vals["region"] = tftypes.NewValue(tftypes.String, region)
	return tftypes.NewValue(typ, vals)
}

// An object lives in one region, and every client a resource builds now
// targets its region, so a change of region must replace the object. It used
// to plan an in-place update that moved nothing.
func TestRegionChangeForcesReplacement(t *testing.T) {
	ctx := context.Background()
	p := New("test")()
	regional := 0
	for _, newResource := range p.Resources(ctx) {
		r := newResource()
		var meta resource.MetadataResponse
		r.Metadata(ctx, resource.MetadataRequest{ProviderTypeName: "pcd"}, &meta)
		var sch resource.SchemaResponse
		r.Schema(ctx, resource.SchemaRequest{}, &sch)
		attr, ok := sch.Schema.Attributes["region"].(schema.StringAttribute)
		if !ok {
			continue
		}
		regional++
		state := tfsdk.State{Schema: sch.Schema, Raw: objectWithRegion(ctx, sch.Schema, "region-one")}
		plan := tfsdk.Plan{Schema: sch.Schema, Raw: objectWithRegion(ctx, sch.Schema, "region-two")}
		req := planmodifier.StringRequest{
			Path: path.Root("region"), Config: tfsdk.Config{Schema: sch.Schema, Raw: plan.Raw},
			ConfigValue: types.StringValue("region-two"), Plan: plan, PlanValue: types.StringValue("region-two"),
			State: state, StateValue: types.StringValue("region-one"),
		}
		replace := false
		for _, m := range attr.PlanModifiers {
			resp := &planmodifier.StringResponse{PlanValue: req.PlanValue}
			m.PlanModifyString(ctx, req, resp)
			replace = replace || resp.RequiresReplace
		}
		if !replace {
			t.Errorf("%s: changing region plans an in-place update; want a replacement", meta.TypeName)
		}
	}
	if regional < 40 {
		t.Fatalf("found %d resources with a region attribute; the walk is missing them", regional)
	}
}
