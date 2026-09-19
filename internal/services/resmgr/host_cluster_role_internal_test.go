// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package resmgr

import (
	"context"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
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
		Settings:    types.MapNull(types.StringType),
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
	// Seed the response state from prior, not a null root, on purpose: the
	// framework itself starts Update with a null root, but a regression that
	// returns without calling State.Set would leave that null root in place and
	// the assertions below would fail loudly instead of silently passing on an
	// all-null response.
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
	if got.HostCluster.ValueString() != "c1" {
		t.Fatalf("host_cluster = %q after apply, want c1; the short-circuit dropped a planned value", got.HostCluster.ValueString())
	}
}

// Import used to set only id/host_id/role, leaving wait_until_converged null;
// the schema default then planned `null -> false` on every imported role, and
// applying it PUT the role to resmgr. Import must write the default itself.
func TestImportStateWritesTheDefault(t *testing.T) {
	ctx := context.Background()
	s := roleSchema(t)
	resp := resource.ImportStateResponse{State: tfsdk.State{Schema: s, Raw: tftypes.NewValue(s.Type().TerraformType(ctx), nil)}}

	(&hostClusterRoleResource{}).ImportState(ctx, resource.ImportStateRequest{ID: "host-a/persistent-storage"}, &resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("import: %v", resp.Diagnostics)
	}
	var got hostClusterRoleModel
	if diags := resp.State.Get(ctx, &got); diags.HasError() {
		t.Fatalf("state: %v", diags)
	}
	if got.ID.ValueString() != "host-a/persistent-storage" || got.HostID.ValueString() != "host-a" || got.Role.ValueString() != "persistent-storage" {
		t.Fatalf("id/host_id/role = %q/%q/%q", got.ID.ValueString(), got.HostID.ValueString(), got.Role.ValueString())
	}
	if got.WaitUntilConverged.IsNull() || got.WaitUntilConverged.ValueBool() {
		t.Fatalf("wait_until_converged = %v after import, want false; null plans a spurious update", got.WaitUntilConverged)
	}
	// The server-side options are not known at import and must stay null so a
	// configuration that sets them plans the update it should.
	if !got.HostCluster.IsNull() || !got.Backends.IsNull() {
		t.Fatalf("host_cluster/backends = %v/%v after import, want null", got.HostCluster, got.Backends)
	}
}

func TestImportStateRejectsABadID(t *testing.T) {
	ctx := context.Background()
	s := roleSchema(t)
	for _, id := range []string{"", "host-a", "/hypervisor", "host-a/"} {
		resp := resource.ImportStateResponse{State: tfsdk.State{Schema: s, Raw: tftypes.NewValue(s.Type().TerraformType(ctx), nil)}}
		(&hostClusterRoleResource{}).ImportState(ctx, resource.ImportStateRequest{ID: id}, &resp)
		if !resp.Diagnostics.HasError() {
			t.Errorf("import id %q: no error, want <host_id>/<role> to be enforced", id)
		}
	}
}

func settingsMap(kv ...string) types.Map {
	elems := map[string]attr.Value{}
	for i := 0; i+1 < len(kv); i += 2 {
		elems[kv[i]] = types.StringValue(kv[i+1])
	}
	return types.MapValueMust(types.StringType, elems)
}

// settings is written through resmgr v1, separately from the v2 assignment,
// so a change to it must trigger that write and nothing else.
func TestSettingsChanged(t *testing.T) {
	base := func(s types.Map) *hostClusterRoleModel {
		m := roleModel(types.StringNull(), types.ListNull(types.StringType))
		m.Role = types.StringValue("dns")
		m.Settings = s
		return m
	}
	for _, tc := range []struct {
		name        string
		plan, state types.Map
		want        bool
	}{
		{name: "both unset", plan: types.MapNull(types.StringType), state: types.MapNull(types.StringType)},
		{name: "same", plan: settingsMap("listen", "[::]:5354"), state: settingsMap("listen", "[::]:5354")},
		{name: "set", plan: settingsMap("listen", "[::]:5354"), state: types.MapNull(types.StringType), want: true},
		{name: "value changed", plan: settingsMap("listen", "[::]:5354"), state: settingsMap("listen", "0.0.0.0:5354"), want: true},
		{name: "key added", plan: settingsMap("listen", "[::]:5354", "debug", "True"), state: settingsMap("listen", "[::]:5354"), want: true},
		{name: "removed entirely", plan: types.MapNull(types.StringType), state: settingsMap("listen", "[::]:5354"), want: true},
		{name: "unknown plan", plan: types.MapUnknown(types.StringType), state: types.MapNull(types.StringType)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := settingsChanged(base(tc.plan), base(tc.state)); got != tc.want {
				t.Fatalf("settingsChanged = %v, want %v", got, tc.want)
			}
		})
	}
	// settings never makes the v2 assignment look changed.
	if roleOptionsChanged(base(settingsMap("listen", "[::]:5354")), base(types.MapNull(types.StringType))) {
		t.Fatalf("roleOptionsChanged = true for a settings-only change; the cluster role would be re-PUT")
	}
}

// The v1 PUT replaces the whole settings object, and the uber-role expansion
// computed most of it; the body must carry everything resmgr holds with only
// the managed keys overwritten.
func TestMergeSettings(t *testing.T) {
	current := map[string]any{"listen": "0.0.0.0:5354", "debug": "False", "db_host": "10.0.0.1", "workers": float64(2)}
	got := mergeSettings(current, map[string]string{"listen": "[::]:5354"})
	if got["listen"] != "[::]:5354" {
		t.Fatalf("listen not overwritten: %v", got)
	}
	if got["debug"] != "False" || got["db_host"] != "10.0.0.1" || got["workers"] != float64(2) {
		t.Fatalf("unmanaged settings not preserved; designate would lose its configuration: %v", got)
	}
	if current["listen"] != "0.0.0.0:5354" {
		t.Fatalf("mergeSettings mutated its input")
	}
}

// State holds exactly the managed keys, as strings, so plan compares like
// with like; a managed key resmgr dropped is absent, which plans a re-apply.
func TestReadSettings(t *testing.T) {
	current := map[string]any{"listen": "[::]:5354", "debug": true, "workers": float64(2), "db_host": "10.0.0.1"}
	got := readSettings(current, map[string]string{"listen": "[::]:5354", "debug": "true", "workers": "2", "gone": "x"})
	want := map[string]string{"listen": "[::]:5354", "debug": "true", "workers": "2"}
	if len(got) != len(want) {
		t.Fatalf("readSettings = %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("readSettings[%s] = %q, want %q", k, got[k], v)
		}
	}
	if _, ok := got["db_host"]; ok {
		t.Fatalf("an unmanaged key leaked into state; every plan would show it as a change")
	}
}

func TestSettingString(t *testing.T) {
	for _, tc := range []struct {
		in   any
		want string
	}{
		{"[::]:5354", "[::]:5354"}, {true, "true"}, {float64(5354), "5354"}, {float64(1.5), "1.5"}, {nil, ""},
		{[]any{"a"}, `["a"]`},
	} {
		if got := settingString(tc.in); got != tc.want {
			t.Errorf("settingString(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// dnsModel is a dns role with the given settings, as a plan or state holds it.
func dnsModel(settings types.Map) *hostClusterRoleModel {
	m := roleModel(types.StringNull(), types.ListNull(types.StringType))
	m.ID = types.StringValue("host-a/dns")
	m.Role = types.StringValue("dns")
	m.WaitUntilConverged = types.BoolValue(false)
	m.Settings = settings
	return m
}

// nullListen is `settings = { listen = null }`, which terraform validate
// accepts and which cannot be decoded into the map the v1 PUT sends.
func nullListen() types.Map {
	return types.MapValueMust(types.StringType, map[string]attr.Value{"listen": types.StringNull()})
}

// failOnClientBuild turns the panic of a resmgr client built from a nil config
// into a failure that says what happened. Deferred by tests whose resource has
// no config, so any server call is caught.
func failOnClientBuild(t *testing.T) {
	t.Helper()
	if r := recover(); r != nil {
		t.Fatalf("a resmgr client was built before settings were decoded: %v", r)
	}
}

func TestManagedSettingsRejectsANullElement(t *testing.T) {
	var diags diag.Diagnostics
	managedSettings(context.Background(), dnsModel(nullListen()), &diags)
	if !diags.HasError() {
		t.Fatal("managedSettings accepted a null element; Create and Update would have nothing to stop on")
	}
}

// Create used to decode settings after the v2 PUT, so a decode failure left
// the role assigned in resmgr and absent from state. It must fail before any
// server call and record nothing.
func TestCreateDecodesSettingsBeforeAssigning(t *testing.T) {
	ctx := context.Background()
	s := roleSchema(t)
	plan := tfsdk.Plan{Schema: s, Raw: roleState(t, dnsModel(nullListen())).Raw}
	resp := resource.CreateResponse{State: tfsdk.State{Schema: s, Raw: tftypes.NewValue(s.Type().TerraformType(ctx), nil)}}

	defer failOnClientBuild(t)
	(&hostClusterRoleResource{}).Create(ctx, resource.CreateRequest{Plan: plan}, &resp)

	if !resp.Diagnostics.HasError() {
		t.Fatal("Create accepted settings with a null element")
	}
	if !resp.State.Raw.IsNull() {
		t.Fatal("Create recorded state although nothing was assigned")
	}
}

// Update must also decode settings before any write. The second case pairs
// the null element with a host_cluster change: ValidateConfig keeps that pair
// out of real configurations (settings is dns-only, host_cluster
// hypervisor-only), but it is the one way to put the v2 PUT ahead of the
// settings write, so it is the case that shows the decode comes first.
func TestUpdateDecodesSettingsBeforeWriting(t *testing.T) {
	for _, tc := range []struct {
		name                      string
		priorCluster, planCluster types.String
	}{
		{name: "settings only", priorCluster: types.StringNull(), planCluster: types.StringNull()},
		{name: "with a v2 option change", priorCluster: types.StringValue("c1"), planCluster: types.StringValue("c2")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			prior := dnsModel(types.MapNull(types.StringType))
			prior.HostCluster = tc.priorCluster
			want := dnsModel(nullListen())
			want.HostCluster = tc.planCluster

			state := roleState(t, prior)
			plan := tfsdk.Plan{Schema: state.Schema, Raw: roleState(t, want).Raw}
			resp := resource.UpdateResponse{State: tfsdk.State{Schema: state.Schema, Raw: tftypes.NewValue(state.Schema.Type().TerraformType(ctx), nil)}}

			defer failOnClientBuild(t)
			(&hostClusterRoleResource{}).Update(ctx, resource.UpdateRequest{Plan: plan, State: state}, &resp)

			if !resp.Diagnostics.HasError() {
				t.Fatal("Update accepted settings with a null element")
			}
		})
	}
}
