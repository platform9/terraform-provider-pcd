// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package compute

import (
	"context"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

// The DRR key lives in the same Nova metadata map as user metadata; the
// provider must keep them apart on read so neither attribute drifts.
func TestSplitMigrationPriority(t *testing.T) {
	meta, prio := splitMigrationPriority(map[string]string{"env": "dev", "migration-priority": "high", "team": "x"})
	if prio != "high" {
		t.Fatalf("prio = %q, want high", prio)
	}
	if _, leaked := meta["migration-priority"]; leaked {
		t.Fatalf("reserved key leaked into user metadata: %v", meta)
	}
	if len(meta) != 2 || meta["env"] != "dev" || meta["team"] != "x" {
		t.Fatalf("user metadata mangled: %v", meta)
	}

	meta, prio = splitMigrationPriority(map[string]string{"env": "dev"})
	if prio != "" || len(meta) != 1 {
		t.Fatalf("unset priority: prio=%q meta=%v", prio, meta)
	}
	meta, prio = splitMigrationPriority(nil)
	if prio != "" || len(meta) != 0 {
		t.Fatalf("nil metadata: prio=%q meta=%v", prio, meta)
	}
}

func TestMigrationPriorityValues(t *testing.T) {
	for _, ok := range []string{"normal", "low", "high", "never"} {
		if !migrationPriorities[ok] {
			t.Errorf("%q should be valid", ok)
		}
	}
	for _, bad := range []string{"Normal", "excluded", "medium", "0", " high"} {
		if migrationPriorities[bad] {
			t.Errorf("%q should be invalid", bad)
		}
	}
}

// block_device conversion: defaults, validation, and root detection.
func TestBlockDevicesFromList(t *testing.T) {
	ctx := context.Background()
	bdType := map[string]attr.Type{
		"source_type": types.StringType, "uuid": types.StringType, "volume_size": types.Int64Type,
		"destination_type": types.StringType, "boot_index": types.Int64Type, "delete_on_termination": types.BoolType,
		"volume_type": types.StringType, "guest_format": types.StringType, "device_type": types.StringType, "disk_bus": types.StringType,
	}
	obj := func(vals map[string]attr.Value) attr.Value {
		full := map[string]attr.Value{
			"source_type": types.StringNull(), "uuid": types.StringNull(), "volume_size": types.Int64Null(),
			"destination_type": types.StringNull(), "boot_index": types.Int64Null(), "delete_on_termination": types.BoolNull(),
			"volume_type": types.StringNull(), "guest_format": types.StringNull(), "device_type": types.StringNull(), "disk_bus": types.StringNull(),
		}
		for k, v := range vals {
			full[k] = v
		}
		o, d := types.ObjectValue(bdType, full)
		if d.HasError() {
			t.Fatal(d)
		}
		return o
	}
	list := func(items ...attr.Value) types.List {
		l, d := types.ListValue(types.ObjectType{AttrTypes: bdType}, items)
		if d.HasError() {
			t.Fatal(d)
		}
		return l
	}

	t.Run("new volume from image is root with sane defaults", func(t *testing.T) {
		var diags diag.Diagnostics
		bds, root := blockDevicesFromList(ctx, list(obj(map[string]attr.Value{
			"source_type": types.StringValue("image"), "uuid": types.StringValue("img"), "volume_size": types.Int64Value(20), "boot_index": types.Int64Value(0),
		})), &diags)
		if diags.HasError() {
			t.Fatal(diags)
		}
		if !root || len(bds) != 1 || bds[0].DestinationType != "volume" || bds[0].VolumeSize != 20 || bds[0].BootIndex != 0 {
			t.Fatalf("got root=%v bds=%+v", root, bds)
		}
	})
	t.Run("data disk defaults to boot_index -1 and is not root", func(t *testing.T) {
		var diags diag.Diagnostics
		bds, root := blockDevicesFromList(ctx, list(obj(map[string]attr.Value{
			"source_type": types.StringValue("blank"), "volume_size": types.Int64Value(5),
		})), &diags)
		if diags.HasError() || root || bds[0].BootIndex != -1 {
			t.Fatalf("root=%v bootIndex=%d diags=%v", root, bds[0].BootIndex, diags)
		}
	})
	t.Run("ISO install: blank root + cdrom at boot_index 1", func(t *testing.T) {
		var diags diag.Diagnostics
		bds, root := blockDevicesFromList(ctx, list(
			obj(map[string]attr.Value{"source_type": types.StringValue("blank"), "volume_size": types.Int64Value(40), "boot_index": types.Int64Value(0)}),
			obj(map[string]attr.Value{"source_type": types.StringValue("image"), "uuid": types.StringValue("iso"), "volume_size": types.Int64Value(1), "boot_index": types.Int64Value(1), "device_type": types.StringValue("cdrom")}),
		), &diags)
		if diags.HasError() || !root || len(bds) != 2 || bds[1].DeviceType != "cdrom" || bds[1].BootIndex != 1 {
			t.Fatalf("root=%v bds=%+v diags=%v", root, bds, diags)
		}
	})
	t.Run("uuid required unless blank", func(t *testing.T) {
		var diags diag.Diagnostics
		blockDevicesFromList(ctx, list(obj(map[string]attr.Value{"source_type": types.StringValue("volume")})), &diags)
		if !diags.HasError() {
			t.Fatal("expected uuid-required error")
		}
	})
	t.Run("blank requires volume_size", func(t *testing.T) {
		var diags diag.Diagnostics
		blockDevicesFromList(ctx, list(obj(map[string]attr.Value{"source_type": types.StringValue("blank")})), &diags)
		if !diags.HasError() {
			t.Fatal("expected volume_size-required error")
		}
	})
	t.Run("invalid source_type rejected", func(t *testing.T) {
		var diags diag.Diagnostics
		blockDevicesFromList(ctx, list(obj(map[string]attr.Value{"source_type": types.StringValue("disk"), "uuid": types.StringValue("x")})), &diags)
		if !diags.HasError() {
			t.Fatal("expected invalid source_type error")
		}
	})
	t.Run("empty list yields nil and no root", func(t *testing.T) {
		var diags diag.Diagnostics
		bds, root := blockDevicesFromList(ctx, types.ListNull(types.ObjectType{AttrTypes: bdType}), &diags)
		if bds != nil || root || diags.HasError() {
			t.Fatalf("bds=%v root=%v", bds, root)
		}
	})
}

// scheduler_hints must reach Nova as the os:scheduler_hints body gophercloud
// builds; absent block => nil (no hints key at all).
func TestSchedulerHintsFromList(t *testing.T) {
	ctx := context.Background()
	var diags diag.Diagnostics

	if got := schedulerHintsFromList(ctx, types.ListNull(schedulerHintsObjectType()), &diags); got != nil || diags.HasError() {
		t.Fatalf("null list: got %v diags %v, want nil", got, diags)
	}

	obj, d := types.ObjectValue(schedulerHintsObjectType().AttrTypes, map[string]attr.Value{
		"group":                 types.StringValue("2b6f4a3e-1c2d-4e5f-8a9b-0c1d2e3f4a5b"),
		"different_host":        types.ListValueMust(types.StringType, []attr.Value{types.StringValue("11111111-2222-4333-8444-555555555555")}),
		"same_host":             types.ListNull(types.StringType),
		"additional_properties": types.MapValueMust(types.StringType, map[string]attr.Value{"query": types.StringValue("[\"=\",\"$hypervisor_type\",\"QEMU\"]")}),
	})
	if d.HasError() {
		t.Fatal(d)
	}
	l := types.ListValueMust(schedulerHintsObjectType(), []attr.Value{obj})
	got := schedulerHintsFromList(ctx, l, &diags)
	if diags.HasError() {
		t.Fatal(diags)
	}
	body, err := got.ToSchedulerHintsMap()
	if err != nil {
		t.Fatal(err)
	}
	// gophercloud envelopes the hints under the key Nova expects on the create body.
	m, ok := body["os:scheduler_hints"].(map[string]any)
	if !ok {
		t.Fatalf("hints not enveloped as os:scheduler_hints: %v", body)
	}
	if m["group"] != "2b6f4a3e-1c2d-4e5f-8a9b-0c1d2e3f4a5b" {
		t.Fatalf("group not mapped: %v", m)
	}
	if dh, ok := m["different_host"].([]string); !ok || len(dh) != 1 {
		t.Fatalf("different_host not mapped: %v", m)
	}
	if _, has := m["same_host"]; has {
		t.Fatalf("null same_host must be omitted: %v", m)
	}
	if m["query"] != "[\"=\",\"$hypervisor_type\",\"QEMU\"]" {
		t.Fatalf("additional_properties not merged: %v", m)
	}
}
