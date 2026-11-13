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

// TestQuotasetPlanUserMode drives the real provider server through
// PlanResourceChange without any API access (the provider is never
// configured, and the plan needs no client). A user quota set that changes plans the quota fields
// its configuration does not set as unknown, because Nova reports the
// project's values for them and the same apply may change those. An unchanged
// user quota set and a changed project quota set keep every field.
func TestQuotasetPlanUserMode(t *testing.T) {
	ctx := context.Background()
	server, err := acctest.ProtoV6ProviderFactories["pcd"]()
	if err != nil {
		t.Fatalf("provider server: %v", err)
	}
	schemas, err := server.GetProviderSchema(ctx, &tfprotov6.GetProviderSchemaRequest{})
	if err != nil {
		t.Fatalf("GetProviderSchema: %v", err)
	}
	typ, ok := schemas.ResourceSchemas["pcd_compute_quotaset"].ValueType().(tftypes.Object)
	if !ok {
		t.Fatal("pcd_compute_quotaset is not registered as an object")
	}
	// object builds a value from set; every other attribute is a typed null.
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
	str := func(s string) tftypes.Value { return tftypes.NewValue(tftypes.String, s) }
	num := func(n int64) tftypes.Value { return tftypes.NewValue(tftypes.Number, n) }
	dynamic := func(v tftypes.Value) *tfprotov6.DynamicValue {
		dv, err := tfprotov6.NewDynamicValue(typ, v)
		if err != nil {
			t.Fatalf("NewDynamicValue: %v", err)
		}
		return &dv
	}

	for _, tc := range []struct {
		name        string
		userID      string
		cores       int64
		wantUnknown bool
	}{
		{name: "user quota set changes", userID: "u-1", cores: 12, wantUnknown: true},
		{name: "user quota set unchanged", userID: "u-1", cores: 8},
		{name: "project quota set changes", cores: 12},
	} {
		t.Run(tc.name, func(t *testing.T) {
			id := "p-1/region-one"
			userID := tftypes.NewValue(tftypes.String, nil)
			if tc.userID != "" {
				id += "/" + tc.userID
				userID = str(tc.userID)
			}
			// The prior state as Read stores it: every field known.
			prior := map[string]tftypes.Value{
				"id": str(id), "project_id": str("p-1"), "user_id": userID, "region": str("region-one"),
				"fixed_ips": num(-1), "floating_ips": num(-1), "injected_file_content_bytes": num(10240),
				"injected_file_path_bytes": num(255), "injected_files": num(5), "key_pairs": num(100),
				"metadata_items": num(128), "ram": num(16384), "security_group_rules": num(-1),
				"security_groups": num(-1), "cores": num(8), "instances": num(4), "server_groups": num(10),
				"server_group_members": num(10),
			}
			config := map[string]tftypes.Value{
				"project_id": str("p-1"), "user_id": userID, "cores": num(tc.cores), "instances": num(4), "ram": num(16384),
			}
			// Terraform core proposes the configuration, with every attribute it
			// leaves null taken from the prior state.
			proposed := map[string]tftypes.Value{}
			for name, v := range prior {
				proposed[name] = v
			}
			for name, v := range config {
				proposed[name] = v
			}
			resp, err := server.PlanResourceChange(ctx, &tfprotov6.PlanResourceChangeRequest{
				TypeName:         "pcd_compute_quotaset",
				PriorState:       dynamic(object(prior)),
				ProposedNewState: dynamic(object(proposed)),
				Config:           dynamic(object(config)),
			})
			if err != nil {
				t.Fatalf("PlanResourceChange: %v", err)
			}
			for _, d := range resp.Diagnostics {
				t.Errorf("diagnostic: %s: %s", d.Summary, d.Detail)
			}
			planned, err := resp.PlannedState.Unmarshal(typ)
			if err != nil {
				t.Fatalf("PlannedState.Unmarshal: %v", err)
			}
			var attrs map[string]tftypes.Value
			if err := planned.As(&attrs); err != nil {
				t.Fatalf("planned.As: %v", err)
			}
			for _, name := range []string{"key_pairs", "metadata_items", "server_groups"} {
				if attrs[name].IsKnown() == tc.wantUnknown {
					t.Errorf("%s planned %v; want unknown: %v", name, attrs[name], tc.wantUnknown)
				}
			}
			for name, want := range map[string]tftypes.Value{"cores": num(tc.cores), "ram": num(16384), "id": str(id)} {
				if !attrs[name].Equal(want) {
					t.Errorf("%s planned %v, want %v", name, attrs[name], want)
				}
			}
		})
	}
}
