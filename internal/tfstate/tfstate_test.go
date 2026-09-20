// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package tfstate_test

import (
	"context"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"

	"github.com/platform9/terraform-provider-pcd/internal/tfstate"
)

type sampleModel struct {
	ID    types.String `tfsdk:"id"`
	Name  types.String `tfsdk:"name"`
	Tags  types.Set    `tfsdk:"tags"`
	Count types.Int64  `tfsdk:"count"`
}

// A Create that records its object before the API has reported the object's
// computed attributes leaves them unknown, and Terraform refuses unknown values
// in state. NullUnknowns must replace exactly those, leaving known and null
// values alone.
func TestNullUnknownsReplacesOnlyTheUnknowns(t *testing.T) {
	ctx := context.Background()
	s := schema.Schema{Attributes: map[string]schema.Attribute{
		"id":    schema.StringAttribute{Computed: true},
		"name":  schema.StringAttribute{Optional: true},
		"tags":  schema.SetAttribute{Computed: true, ElementType: types.StringType},
		"count": schema.Int64Attribute{Computed: true},
	}}

	state := tfsdk.State{Schema: s, Raw: tftypes.NewValue(s.Type().TerraformType(ctx), nil)}
	if d := state.Set(ctx, &sampleModel{
		ID:    types.StringValue("vol-1"),
		Name:  types.StringNull(),
		Tags:  types.SetUnknown(types.StringType),
		Count: types.Int64Unknown(),
	}); d.HasError() {
		t.Fatalf("building the state: %v", d)
	}
	if state.Raw.IsFullyKnown() {
		t.Fatal("test setup is wrong: the state holds no unknowns to replace")
	}

	if d := tfstate.NullUnknowns(&state); d.HasError() {
		t.Fatalf("NullUnknowns: %v", d)
	}

	if !state.Raw.IsFullyKnown() {
		t.Fatalf("state still holds unknown values, which Terraform refuses: %v", state.Raw)
	}
	var got sampleModel
	if d := state.Get(ctx, &got); d.HasError() {
		t.Fatalf("reading the state back: %v", d)
	}
	if got.ID.ValueString() != "vol-1" {
		t.Fatalf("id = %s, want vol-1: a known value must survive", got.ID)
	}
	if !got.Name.IsNull() {
		t.Fatalf("name = %s, want null: a null value must stay null", got.Name)
	}
	if !got.Tags.IsNull() {
		t.Fatalf("tags = %s, want null", got.Tags)
	}
	if !got.Count.IsNull() {
		t.Fatalf("count = %s, want null", got.Count)
	}
}

// A state with nothing unknown must come through untouched.
func TestNullUnknownsLeavesAFullyKnownStateAlone(t *testing.T) {
	ctx := context.Background()
	s := schema.Schema{Attributes: map[string]schema.Attribute{
		"id":    schema.StringAttribute{Computed: true},
		"name":  schema.StringAttribute{Optional: true},
		"tags":  schema.SetAttribute{Computed: true, ElementType: types.StringType},
		"count": schema.Int64Attribute{Computed: true},
	}}
	tags, d := types.SetValueFrom(ctx, types.StringType, []string{"a"})
	if d.HasError() {
		t.Fatalf("building tags: %v", d)
	}

	state := tfsdk.State{Schema: s, Raw: tftypes.NewValue(s.Type().TerraformType(ctx), nil)}
	if d := state.Set(ctx, &sampleModel{
		ID:    types.StringValue("vol-1"),
		Name:  types.StringValue("data-1"),
		Tags:  tags,
		Count: types.Int64Value(3),
	}); d.HasError() {
		t.Fatalf("building the state: %v", d)
	}
	before := state.Raw.String()

	if d := tfstate.NullUnknowns(&state); d.HasError() {
		t.Fatalf("NullUnknowns: %v", d)
	}
	if state.Raw.String() != before {
		t.Fatalf("state changed:\n before %s\n after  %s", before, state.Raw.String())
	}
}
