// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package resmgr

import (
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/types"
)

// A virtual_networking block that leaves vnid_range unset plans that leaf
// unknown. After a successful read-back, Create used to put the configured
// block back over the server's whole, unknown leaf included, and Terraform
// refused the unknown in state. The leaf must take the server's value.
func TestBlueprintCreateFillsAnUnsetVirtualNetworkingLeaf(t *testing.T) {
	t.Parallel()
	r := &blueprintResource{config: newRoutedResmgr(t, blueprintRoutes()).config}
	s := schemaOf(t, r)

	var planned blueprintResourceModel
	if d := blueprintPlan(t, r).Get(t.Context(), &planned); d.HasError() {
		t.Fatalf("reading the plan: %v", d)
	}
	planned.VirtualNetworking = geneveNetworking(types.StringUnknown())

	resp := runCreate(r, newPlan(t, s, &planned))
	if resp.Diagnostics.HasError() {
		t.Fatalf("create: %v", resp.Diagnostics)
	}
	if !resp.State.Raw.IsFullyKnown() {
		t.Fatalf("create state holds unknown values, which Terraform refuses: %v", resp.State.Raw)
	}
	if got := blueprintRow(t, resp.State); !got.VirtualNetworking.Equal(geneveNetworking(types.StringValue("1000:2000"))) {
		t.Fatalf("create state virtual_networking = %s, want geneve with resmgr's vnid_range 1000:2000", got.VirtualNetworking)
	}
}

// The same block on an update: the read-back succeeds, and Update used to save
// the plan's unknown vnid_range.
func TestBlueprintUpdateFillsAnUnsetVirtualNetworkingLeaf(t *testing.T) {
	t.Parallel()
	r := &blueprintResource{config: newRoutedResmgr(t, blueprintRoutes()).config}
	s := schemaOf(t, r)

	prior := blueprintResourceModel{
		Name:                      types.StringValue(blueprintName),
		NetworkingType:            types.StringValue("ovn"),
		EnableDistributedRouting:  types.BoolValue(true),
		DNSDomainName:             types.StringValue("pcd.local"),
		VirtualNetworking:         geneveNetworking(types.StringValue("1000:2000")),
		ImageLibraryStorage:       types.StringValue(""),
		ImageLibrarySharedStorage: types.BoolValue(false),
		InstanceSharedStorage:     types.BoolValue(false),
		VMStorage:                 types.StringValue("/opt/data/instances"),
		VNCFloatingIP:             types.StringValue(""),
		StorageBackendsJSON:       types.StringValue(blueprintBackends),
	}
	// A new vnc_floating_ip. The config sets virtual_networking without
	// vnid_range, so the plan leaves that leaf unknown.
	planned := prior
	planned.VNCFloatingIP = types.StringValue(blueprintVNCIP)
	planned.VirtualNetworking = geneveNetworking(types.StringUnknown())

	resp := runUpdate(r, newPlan(t, s, &planned), newState(t, s, &prior))
	if resp.Diagnostics.HasError() {
		t.Fatalf("update: %v", resp.Diagnostics)
	}
	if !resp.State.Raw.IsFullyKnown() {
		t.Fatalf("update state holds unknown values, which Terraform refuses: %v", resp.State.Raw)
	}
	if got := blueprintRow(t, resp.State); !got.VirtualNetworking.Equal(geneveNetworking(types.StringValue("1000:2000"))) {
		t.Fatalf("update state virtual_networking = %s, want geneve with resmgr's vnid_range 1000:2000", got.VirtualNetworking)
	}
}
