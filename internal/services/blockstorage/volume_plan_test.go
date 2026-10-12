// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package blockstorage_test

import (
	"context"
	"testing"

	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-go/tftypes"

	"github.com/platform9/terraform-provider-pcd/internal/acctest"
)

// These tests drive the real provider server through PlanResourceChange for
// pcd_blockstorage_volume, with no API: the provider is never configured and
// planning needs no client. They pin that the changes Update makes in place
// (a retype, an extend, a rename) plan as updates, not as replacements that
// delete the volume and its data, whether or not the configuration sets
// availability_zone and volume_type.

// planVolume plans pcd_blockstorage_volume from prior to config the way
// Terraform core does and returns the replace paths core acts on and the
// planned state.
func planVolume(t *testing.T, prior, config map[string]tftypes.Value) ([]*tftypes.AttributePath, map[string]tftypes.Value) {
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
	typ := schemas.ResourceSchemas["pcd_blockstorage_volume"].ValueType().(tftypes.Object)
	object := func(set map[string]tftypes.Value) tftypes.Value {
		vals := map[string]tftypes.Value{}
		for name, at := range typ.AttributeTypes {
			if v, ok := set[name]; ok {
				vals[name] = v
			} else {
				vals[name] = tftypes.NewValue(at, nil)
			}
		}
		return tftypes.NewValue(typ, vals)
	}
	// ProposedNewState: the configuration, with every attribute it leaves null
	// taken from the prior state.
	merged := map[string]tftypes.Value{}
	for name := range typ.AttributeTypes {
		if v, ok := config[name]; ok {
			merged[name] = v
		} else if v, ok := prior[name]; ok {
			merged[name] = v
		}
	}
	dyn := func(v tftypes.Value) *tfprotov6.DynamicValue {
		dv, err := tfprotov6.NewDynamicValue(typ, v)
		if err != nil {
			t.Fatalf("NewDynamicValue: %v", err)
		}
		return &dv
	}
	priorObj := object(prior)
	resp, err := server.PlanResourceChange(ctx, &tfprotov6.PlanResourceChangeRequest{
		TypeName:         "pcd_blockstorage_volume",
		PriorState:       dyn(priorObj),
		ProposedNewState: dyn(object(merged)),
		Config:           dyn(object(config)),
	})
	if err != nil {
		t.Fatalf("PlanResourceChange: %v", err)
	}
	for _, d := range resp.Diagnostics {
		if d.Severity == tfprotov6.DiagnosticSeverityError {
			t.Fatalf("PlanResourceChange: %s: %s", d.Summary, d.Detail)
		}
	}
	planned, err := resp.PlannedState.Unmarshal(typ)
	if err != nil {
		t.Fatalf("PlannedState.Unmarshal: %v", err)
	}
	var attrs map[string]tftypes.Value
	if err := planned.As(&attrs); err != nil {
		t.Fatalf("planned.As: %v", err)
	}
	// Terraform core drops a replace path whose planned value equals the
	// prior value, so keep only the paths that would replace the volume.
	var replace []*tftypes.AttributePath
	for _, p := range resp.RequiresReplace {
		before, _, errBefore := tftypes.WalkAttributePath(priorObj, p)
		after, _, errAfter := tftypes.WalkAttributePath(planned, p)
		b, okBefore := before.(tftypes.Value)
		a, okAfter := after.(tftypes.Value)
		if errBefore != nil || errAfter != nil || !okBefore || !okAfter || !a.Equal(b) {
			replace = append(replace, p)
		}
	}
	return replace, attrs
}

func volumePlanString(s string) tftypes.Value { return tftypes.NewValue(tftypes.String, s) }

// volumePriorState is vol-1 as Read records it: 1 GiB of type-a in zone nova.
func volumePriorState() map[string]tftypes.Value {
	return map[string]tftypes.Value{
		"id":                   volumePlanString("vol-1"),
		"name":                 volumePlanString("data-1"),
		"size":                 tftypes.NewValue(tftypes.Number, 1),
		"description":          volumePlanString(""),
		"volume_type":          volumePlanString("type-a"),
		"availability_zone":    volumePlanString("nova"),
		"metadata":             tftypes.NewValue(tftypes.Map{ElementType: tftypes.String}, map[string]tftypes.Value{}),
		"bootable":             tftypes.NewValue(tftypes.Bool, false),
		"encrypted":            tftypes.NewValue(tftypes.Bool, false),
		"status":               volumePlanString("available"),
		"region":               volumePlanString("region-one"),
		"volume_retype_policy": volumePlanString("never"),
	}
}

func TestVolumePlanUpdatesInPlace(t *testing.T) {
	for _, tc := range []struct {
		name   string
		config map[string]tftypes.Value
	}{
		{name: "retype", config: map[string]tftypes.Value{
			"name": volumePlanString("data-1"), "size": tftypes.NewValue(tftypes.Number, 1), "volume_type": volumePlanString("type-b"),
		}},
		{name: "extend without zone or type", config: map[string]tftypes.Value{
			"name": volumePlanString("data-1"), "size": tftypes.NewValue(tftypes.Number, 2),
		}},
		{name: "rename without zone or type", config: map[string]tftypes.Value{
			"name": volumePlanString("data-2"), "size": tftypes.NewValue(tftypes.Number, 1),
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			replace, planned := planVolume(t, volumePriorState(), tc.config)
			if len(replace) != 0 {
				t.Fatalf("RequiresReplace = %v, want none: a replacement deletes the volume and its data", replace)
			}
			if !planned["availability_zone"].Equal(volumePlanString("nova")) {
				t.Fatalf("planned availability_zone = %v, want the prior nova", planned["availability_zone"])
			}
		})
	}
}

// Changing the availability zone still replaces the volume: Cinder cannot
// move a volume between zones in place.
func TestVolumePlanZoneChangeReplaces(t *testing.T) {
	replace, _ := planVolume(t, volumePriorState(), map[string]tftypes.Value{
		"name": volumePlanString("data-1"), "size": tftypes.NewValue(tftypes.Number, 1), "availability_zone": volumePlanString("zone-2"),
	})
	want := tftypes.NewAttributePath().WithAttributeName("availability_zone")
	if len(replace) != 1 || !replace[0].Equal(want) {
		t.Fatalf("RequiresReplace = %v, want [%v]", replace, want)
	}
}

// A state written before volume_retype_policy existed has no value for it.
// The plan fills in the default without replacing anything; the update it
// plans retypes nothing, and a refresh normally fills the value in first.
func TestVolumePlanFillsTheRetypePolicyDefault(t *testing.T) {
	prior := volumePriorState()
	prior["volume_retype_policy"] = tftypes.NewValue(tftypes.String, nil)
	replace, planned := planVolume(t, prior, map[string]tftypes.Value{
		"name": volumePlanString("data-1"), "size": tftypes.NewValue(tftypes.Number, 1),
	})
	if len(replace) != 0 {
		t.Fatalf("RequiresReplace = %v, want none", replace)
	}
	if !planned["volume_retype_policy"].Equal(volumePlanString("never")) {
		t.Fatalf("planned volume_retype_policy = %v, want never", planned["volume_retype_policy"])
	}
}
