// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package compute

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

// instanceValue builds a pcd_compute_instance plan, state or config value
// from just the attributes a test cares about (Go strings or framework
// values); every other attribute is null. SetAttribute builds the object
// around them, so the helper keeps working as the instance schema grows.
func instanceValue(t *testing.T, attrs map[string]any) tftypes.Value {
	t.Helper()
	ctx := context.Background()
	var sch resource.SchemaResponse
	(&instanceResource{}).Schema(ctx, resource.SchemaRequest{}, &sch)
	plan := tfsdk.Plan{Schema: sch.Schema, Raw: tftypes.NewValue(sch.Schema.Type().TerraformType(ctx), nil)}
	for k, v := range attrs {
		if d := plan.SetAttribute(ctx, path.Root(k), v); d.HasError() {
			t.Fatalf("setting %s: %v", k, d)
		}
	}
	return plan.Raw
}

func fdMetadata(kv map[string]string) types.Map {
	vals := map[string]attr.Value{}
	for k, v := range kv {
		vals[k] = types.StringValue(v)
	}
	return types.MapValueMust(types.StringType, vals)
}

// The failure-domain affinity is plain instance metadata: the provider must
// not reserve the key the PCD scheduler reads.
func TestInstanceAcceptsFailureDomainMetadata(t *testing.T) {
	ctx := context.Background()
	r := &instanceResource{}
	var sch resource.SchemaResponse
	r.Schema(ctx, resource.SchemaRequest{}, &sch)
	cfg := tfsdk.Config{Schema: sch.Schema, Raw: instanceValue(t, map[string]any{
		"name": "vm-1", "metadata": fdMetadata(map[string]string{"pf9_failure_domain": "7:8"})})}
	resp := resource.ValidateConfigResponse{}
	r.ValidateConfig(ctx, resource.ValidateConfigRequest{Config: cfg}, &resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("pf9_failure_domain refused: %v", resp.Diagnostics)
	}
}

// A failure-domain value names aggregates created in the same apply, so it
// is unknown when Terraform validates the configuration. Validation must
// accept unknown metadata values instead of failing to convert them.
func TestInstanceValidateConfigAcceptsUnknownMetadataValues(t *testing.T) {
	ctx := context.Background()
	r := &instanceResource{}
	var sch resource.SchemaResponse
	r.Schema(ctx, resource.SchemaRequest{}, &sch)
	meta := types.MapValueMust(types.StringType, map[string]attr.Value{"pf9_failure_domain": types.StringUnknown()})
	cfg := tfsdk.Config{Schema: sch.Schema, Raw: instanceValue(t, map[string]any{"name": "vm-1", "metadata": meta})}
	resp := resource.ValidateConfigResponse{}
	r.ValidateConfig(ctx, resource.ValidateConfigRequest{Config: cfg}, &resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("an unknown metadata value failed validation: %v", resp.Diagnostics)
	}
	// The reserved key is still refused, known value or not.
	meta = types.MapValueMust(types.StringType, map[string]attr.Value{"migration-priority": types.StringUnknown()})
	cfg = tfsdk.Config{Schema: sch.Schema, Raw: instanceValue(t, map[string]any{"name": "vm-1", "metadata": meta})}
	resp = resource.ValidateConfigResponse{}
	r.ValidateConfig(ctx, resource.ValidateConfigRequest{Config: cfg}, &resp)
	if !resp.Diagnostics.HasError() || resp.Diagnostics[0].Summary() != "Reserved metadata key" {
		t.Fatalf("diags = %v; want the reserved key refused", resp.Diagnostics)
	}
}

// A null metadata value is known at validation, but Create and Update cannot
// send it, so validation refuses it and names the key.
func TestInstanceValidateConfigRefusesNullMetadataValue(t *testing.T) {
	ctx := context.Background()
	r := &instanceResource{}
	var sch resource.SchemaResponse
	r.Schema(ctx, resource.SchemaRequest{}, &sch)
	meta := types.MapValueMust(types.StringType, map[string]attr.Value{"x": types.StringNull()})
	cfg := tfsdk.Config{Schema: sch.Schema, Raw: instanceValue(t, map[string]any{"name": "vm-1", "metadata": meta})}
	resp := resource.ValidateConfigResponse{}
	r.ValidateConfig(ctx, resource.ValidateConfigRequest{Config: cfg}, &resp)
	errs := resp.Diagnostics.Errors()
	if len(errs) != 1 || errs[0].Summary() != "Null metadata value" {
		t.Fatalf("diags = %v; want one Null metadata value error", resp.Diagnostics)
	}
	if d, ok := errs[0].(diag.DiagnosticWithPath); !ok || !d.Path().Equal(path.Root("metadata").AtMapKey("x")) {
		t.Fatalf("diag = %v; want it at metadata[\"x\"]", errs[0])
	}
}

// The reserved key is refused when its value is known, too.
func TestInstanceValidateConfigRefusesReservedKeyWithKnownValue(t *testing.T) {
	ctx := context.Background()
	r := &instanceResource{}
	var sch resource.SchemaResponse
	r.Schema(ctx, resource.SchemaRequest{}, &sch)
	meta := types.MapValueMust(types.StringType, map[string]attr.Value{"migration-priority": types.StringValue("high")})
	cfg := tfsdk.Config{Schema: sch.Schema, Raw: instanceValue(t, map[string]any{"name": "vm-1", "metadata": meta})}
	resp := resource.ValidateConfigResponse{}
	r.ValidateConfig(ctx, resource.ValidateConfigRequest{Config: cfg}, &resp)
	if !resp.Diagnostics.HasError() || resp.Diagnostics[0].Summary() != "Reserved metadata key" {
		t.Fatalf("diags = %v; want the reserved key refused with a known value", resp.Diagnostics)
	}
}

// Removing pf9_failure_domain from metadata deletes the key with its own
// DELETE, which is what makes PCD's Nova drop the scheduler's record of it;
// rewriting the whole metadata (what the PCD UI does) would leave it behind.
// When it was the only key the configuration says metadata = {}: deleting the
// metadata argument instead leaves the map unmanaged (a null configuration
// plans the prior value), so nothing is sent and the key stays.
func TestInstanceUpdateDeletesFailureDomainKey(t *testing.T) {
	for _, tc := range []struct {
		name        string
		prior, plan map[string]string
	}{
		{"other keys stay", map[string]string{"app": "web", "pf9_failure_domain": "7:8"}, map[string]string{"app": "web"}},
		{"it was the only key", map[string]string{"pf9_failure_domain": "7:8"}, map[string]string{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			left, err := json.Marshal(tc.plan)
			if err != nil {
				t.Fatal(err)
			}
			var mu sync.Mutex
			var calls []string
			nova := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				w.Header().Set("Content-Type", "application/json")
				calls = append(calls, r.Method+" "+r.URL.Path)
				switch r.Method + " " + r.URL.Path {
				case "GET /servers/srv-1":
					fmt.Fprintf(w, `{"server": {"id": "srv-1", "name": "vm-1", "status": "ACTIVE", "metadata": %s, "addresses": {}}}`, left)
				case "POST /servers/srv-1/metadata":
					fmt.Fprintf(w, `{"metadata": %s}`, left)
				case "DELETE /servers/srv-1/metadata/pf9_failure_domain":
					w.WriteHeader(http.StatusNoContent)
				default:
					t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
					w.WriteHeader(http.StatusNotImplemented)
				}
			}))
			defer nova.Close()
			r := &instanceResource{config: fakeConfig(nova.URL)}
			var sch resource.SchemaResponse
			r.Schema(ctx, resource.SchemaRequest{}, &sch)
			with := func(meta map[string]string) map[string]any {
				return map[string]any{"id": "srv-1", "name": "vm-1", "flavor_id": "flv-1", "metadata": fdMetadata(meta)}
			}
			state := tfsdk.State{Schema: sch.Schema, Raw: instanceValue(t, with(tc.prior))}
			plan := tfsdk.Plan{Schema: sch.Schema, Raw: instanceValue(t, with(tc.plan))}
			resp := resource.UpdateResponse{State: state}
			r.Update(ctx, resource.UpdateRequest{Plan: plan, State: state}, &resp)
			if resp.Diagnostics.HasError() {
				t.Fatalf("update: %v", resp.Diagnostics)
			}
			mu.Lock()
			defer mu.Unlock()
			if !slices.Contains(calls, "DELETE /servers/srv-1/metadata/pf9_failure_domain") {
				t.Fatalf("calls = %v; want the key deleted on its own", calls)
			}
		})
	}
}
