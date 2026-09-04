// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package resmgr

import (
	"context"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

func backendsList(names ...string) types.List {
	vals := make([]attr.Value, 0, len(names))
	for _, n := range names {
		vals = append(vals, types.StringValue(n))
	}
	return types.ListValueMust(types.StringType, vals)
}

func roleModel(hostCluster types.String, backends types.List) *hostClusterRoleModel {
	return &hostClusterRoleModel{
		ID:          types.StringValue("host-a/hypervisor"),
		HostID:      types.StringValue("host-a"),
		Role:        types.StringValue("hypervisor"),
		HostCluster: hostCluster,
		Backends:    backends,
	}
}

// Update used to PUT the role on every change, including one that touched only
// wait_until_converged, which resmgr never sees. This helper is what decides
// whether the PUT body would differ from what the server already holds, so a
// wrong "false" skips a real change and a wrong "true" sends a needless write.
func TestRoleOptionsChanged(t *testing.T) {
	noBackends := types.ListNull(types.StringType)
	for _, tc := range []struct {
		name        string
		plan, state *hostClusterRoleModel
		want        bool
	}{
		{name: "identical", plan: roleModel(types.StringValue("c1"), noBackends), state: roleModel(types.StringValue("c1"), noBackends)},
		{name: "both unset", plan: roleModel(types.StringNull(), noBackends), state: roleModel(types.StringNull(), noBackends)},
		// assignBody omits host_cluster for both null and "", so they are the same wire body.
		{name: "null vs empty host_cluster", plan: roleModel(types.StringValue(""), noBackends), state: roleModel(types.StringNull(), noBackends)},
		{name: "host_cluster changed", plan: roleModel(types.StringValue("c2"), noBackends), state: roleModel(types.StringValue("c1"), noBackends), want: true},
		{name: "host_cluster set from null", plan: roleModel(types.StringValue("c1"), noBackends), state: roleModel(types.StringNull(), noBackends), want: true},
		{name: "same backends", plan: roleModel(types.StringNull(), backendsList("syn")), state: roleModel(types.StringNull(), backendsList("syn")), want: false},
		{name: "backends changed", plan: roleModel(types.StringNull(), backendsList("syn", "nfs")), state: roleModel(types.StringNull(), backendsList("syn")), want: true},
		{name: "backends order changed", plan: roleModel(types.StringNull(), backendsList("nfs", "syn")), state: roleModel(types.StringNull(), backendsList("syn", "nfs")), want: true},
		// An empty list is sent as [] and null is not sent at all: different bodies.
		{name: "null vs empty backends", plan: roleModel(types.StringNull(), backendsList()), state: roleModel(types.StringNull(), noBackends), want: true},
		// Unknown is never sent, exactly like null.
		{name: "unknown vs null backends", plan: roleModel(types.StringNull(), types.ListUnknown(types.StringType)), state: roleModel(types.StringNull(), noBackends)},
		{name: "backends cleared", plan: roleModel(types.StringNull(), noBackends), state: roleModel(types.StringNull(), backendsList("syn")), want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := roleOptionsChanged(tc.plan, tc.state); got != tc.want {
				if tc.want {
					t.Fatalf("roleOptionsChanged = false; Update would skip a PUT the server needs")
				}
				t.Fatalf("roleOptionsChanged = true; Update would PUT the role for a change resmgr never sees")
			}
		})
	}
}

// roleSchema is the resource's real schema, so the framework request types used
// below carry exactly what Terraform would send.
func roleSchema(t *testing.T) schema.Schema {
	t.Helper()
	var resp resource.SchemaResponse
	(&hostClusterRoleResource{}).Schema(context.Background(), resource.SchemaRequest{}, &resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("schema: %v", resp.Diagnostics)
	}
	return resp.Schema
}

// roleState builds a tfsdk.State holding m. Starting from a null root is what
// the framework itself does before Create/ImportState write into it.
func roleState(t *testing.T, m *hostClusterRoleModel) tfsdk.State {
	t.Helper()
	ctx := context.Background()
	s := roleSchema(t)
	st := tfsdk.State{Schema: s, Raw: tftypes.NewValue(s.Type().TerraformType(ctx), nil)}
	if diags := st.Set(ctx, m); diags.HasError() {
		t.Fatalf("state: %v", diags)
	}
	return st
}

// A change that touches only wait_until_converged must not reach resmgr. The
// resource is built with no client at all: if Update tries to build one the
// test fails, which is the point.
func TestUpdateSkipsResmgrForClientSideChanges(t *testing.T) {
	ctx := context.Background()
	prior := roleModel(types.StringValue("c1"), types.ListNull(types.StringType))
	prior.WaitUntilConverged = types.BoolValue(false)
	want := roleModel(types.StringValue("c1"), types.ListNull(types.StringType))
	want.WaitUntilConverged = types.BoolValue(true)

	state := roleState(t, prior)
	plan := tfsdk.Plan{Schema: state.Schema, Raw: roleState(t, want).Raw}
	resp := resource.UpdateResponse{State: roleState(t, prior)}

	r := &hostClusterRoleResource{} // config nil: any client build panics or errors
	r.Update(ctx, resource.UpdateRequest{Plan: plan, State: state}, &resp)

	if resp.Diagnostics.HasError() {
		t.Fatalf("Update errored on a client-side-only change: %v", resp.Diagnostics)
	}
	var got hostClusterRoleModel
	if diags := resp.State.Get(ctx, &got); diags.HasError() {
		t.Fatalf("state: %v", diags)
	}
	if !got.WaitUntilConverged.ValueBool() {
		t.Fatal("wait_until_converged = false after apply; the plan's value was not stored")
	}
	if got.ID.ValueString() != "host-a/hypervisor" {
		t.Fatalf("id = %q, want host-a/hypervisor", got.ID.ValueString())
	}
}
