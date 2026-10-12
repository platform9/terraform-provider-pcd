// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package compute_test

import (
	"context"
	"testing"

	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-go/tftypes"

	"github.com/platform9/terraform-provider-pcd/internal/acctest"
)

// These tests drive the real provider server through PlanResourceChange for
// pcd_compute_volume_attach, with no API: the provider is never configured and
// planning needs no client. They pin the two promises delete_on_termination
// makes: leaving it unset plans nothing, so upgrading the provider never
// touches an attachment, and changing it never replaces the attachment.

// planVolumeAttach plans pcd_compute_volume_attach from prior to config the way
// Terraform core does and returns the replace paths core acts on, the planned
// state and the prior state.
func planVolumeAttach(t *testing.T, prior, config map[string]tftypes.Value) (replace []*tftypes.AttributePath, planned, priorValue tftypes.Value) {
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
	typ := schemas.ResourceSchemas["pcd_compute_volume_attach"].ValueType().(tftypes.Object)
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
	// ProposedNewState: the configuration, with every attribute it leaves
	// null taken from the prior state (all of them are optional+computed or
	// computed here, or set in both).
	merged := map[string]tftypes.Value{}
	for name := range typ.AttributeTypes {
		if v, ok := config[name]; ok {
			merged[name] = v
		} else {
			merged[name] = prior[name]
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
		TypeName:         "pcd_compute_volume_attach",
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
	plannedState, err := resp.PlannedState.Unmarshal(typ)
	if err != nil {
		t.Fatalf("PlannedState.Unmarshal: %v", err)
	}
	// Terraform core drops a replace path whose planned value equals the
	// prior value, so keep only the paths that would replace the attachment.
	for _, p := range resp.RequiresReplace {
		before, _, errBefore := tftypes.WalkAttributePath(priorObj, p)
		after, _, errAfter := tftypes.WalkAttributePath(plannedState, p)
		b, okBefore := before.(tftypes.Value)
		a, okAfter := after.(tftypes.Value)
		if errBefore != nil || errAfter != nil || !okBefore || !okAfter || !a.Equal(b) {
			replace = append(replace, p)
		}
	}
	return replace, plannedState, priorObj
}

func attachmentState(deleteOnTermination bool) map[string]tftypes.Value {
	s := func(v string) tftypes.Value { return tftypes.NewValue(tftypes.String, v) }
	return map[string]tftypes.Value{
		"id":                    s("vol-1"),
		"instance_id":           s("srv-1"),
		"volume_id":             s("vol-1"),
		"device":                s("/dev/vdb"),
		"region":                s("region-one"),
		"delete_on_termination": tftypes.NewValue(tftypes.Bool, deleteOnTermination),
	}
}

func attachmentConfig(flag *bool) map[string]tftypes.Value {
	c := map[string]tftypes.Value{
		"instance_id": tftypes.NewValue(tftypes.String, "srv-1"),
		"volume_id":   tftypes.NewValue(tftypes.String, "vol-1"),
	}
	if flag != nil {
		c["delete_on_termination"] = tftypes.NewValue(tftypes.Bool, *flag)
	}
	return c
}

// A configuration written before the attribute existed, against an
// attachment whose flag was set in the PCD UI, plans no change at all.
func TestVolumeAttachPlanLeavesAnUnsetFlagAlone(t *testing.T) {
	replace, planned, prior := planVolumeAttach(t, attachmentState(true), attachmentConfig(nil))
	if len(replace) != 0 {
		t.Fatalf("RequiresReplace = %v, want none", replace)
	}
	if !planned.Equal(prior) {
		t.Fatalf("planned state differs from the prior state:\n planned %v\n prior   %v", planned, prior)
	}
}

// Turning the flag off plans an in-place update, not a detach and re-attach.
func TestVolumeAttachPlanChangesTheFlagInPlace(t *testing.T) {
	off := false
	replace, planned, prior := planVolumeAttach(t, attachmentState(true), attachmentConfig(&off))
	if len(replace) != 0 {
		t.Fatalf("RequiresReplace = %v, want none: the flag changes in place", replace)
	}
	if planned.Equal(prior) {
		t.Fatal("planned state equals the prior state; want delete_on_termination = false planned")
	}
}

// Changing the device still means a new attachment.
func TestVolumeAttachPlanDeviceChangeReplaces(t *testing.T) {
	config := attachmentConfig(nil)
	config["device"] = tftypes.NewValue(tftypes.String, "/dev/vdc")
	replace, _, _ := planVolumeAttach(t, attachmentState(false), config)
	want := tftypes.NewAttributePath().WithAttributeName("device")
	if len(replace) != 1 || !replace[0].Equal(want) {
		t.Fatalf("RequiresReplace = %v, want [%v]", replace, want)
	}
}
