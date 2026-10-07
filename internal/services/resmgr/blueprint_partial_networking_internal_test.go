// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package resmgr

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
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

// planVirtualNetworking returns the virtual_networking the framework plans on
// an update from prior to planned for a block that sets only the known leaves
// of configured: it runs each unset leaf's plan modifiers over that leaf, which
// the framework has planned unknown.
func planVirtualNetworking(t *testing.T, r *blueprintResource, prior, planned blueprintResourceModel, configured types.Object) types.Object {
	t.Helper()
	ctx := context.Background()
	s := schemaOf(t, r)
	nested := s.Attributes["virtual_networking"].(schema.SingleNestedAttribute)
	state := newState(t, s, &prior)
	planned.VirtualNetworking = configured
	plan := newPlan(t, s, &planned)
	config := tfsdk.Config{Schema: s, Raw: plan.Raw}
	priorLeaves := prior.VirtualNetworking.Attributes()
	leaves := map[string]attr.Value{}
	for name, v := range configured.Attributes() {
		leaves[name] = v
		if !v.IsUnknown() {
			continue
		}
		p := path.Root("virtual_networking").AtName(name)
		switch a := nested.Attributes[name].(type) {
		case schema.StringAttribute:
			req := planmodifier.StringRequest{Path: p, Config: config, ConfigValue: types.StringNull(), Plan: plan,
				PlanValue: types.StringUnknown(), State: state, StateValue: priorLeaves[name].(types.String)}
			for _, m := range a.PlanModifiers {
				resp := &planmodifier.StringResponse{PlanValue: req.PlanValue}
				m.PlanModifyString(ctx, req, resp)
				req.PlanValue = resp.PlanValue
			}
			leaves[name] = req.PlanValue
		case schema.BoolAttribute:
			req := planmodifier.BoolRequest{Path: p, Config: config, ConfigValue: types.BoolNull(), Plan: plan,
				PlanValue: types.BoolUnknown(), State: state, StateValue: priorLeaves[name].(types.Bool)}
			for _, m := range a.PlanModifiers {
				resp := &planmodifier.BoolResponse{PlanValue: req.PlanValue}
				m.PlanModifyBool(ctx, req, resp)
				req.PlanValue = resp.PlanValue
			}
			leaves[name] = req.PlanValue
		default:
			t.Fatalf("virtual_networking.%s has unexpected type %T", name, a)
		}
	}
	return types.ObjectValueMust(virtualNetworkingAttrTypes, leaves)
}

// The write API takes the whole blueprint, so a virtual_networking leaf the
// config leaves unset must go out with the blueprint's current value. Update
// used to send an unset vnid_range as "" and an unset enabled as false, which
// would clear the region's VNI range and turn its virtual networking off.
func TestBlueprintUpdateSendsTheCurrentValueOfUnsetVirtualNetworkingLeaves(t *testing.T) {
	t.Parallel()
	var sent struct {
		VirtualNetworking *virtualNetworkingAPI `json:"virtualNetworking"`
	}
	routes := blueprintRoutes()
	routes["PUT "+blueprintPath] = func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&sent); err != nil {
			t.Errorf("decoding the PUT body: %v", err)
		}
		w.WriteHeader(http.StatusOK)
	}
	r := &blueprintResource{config: newRoutedResmgr(t, routes).config}
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
	// A new vnc_floating_ip. The config's virtual_networking block sets only
	// underlay_type.
	planned := prior
	planned.VNCFloatingIP = types.StringValue(blueprintVNCIP)
	planned.VirtualNetworking = planVirtualNetworking(t, r, prior, planned,
		types.ObjectValueMust(virtualNetworkingAttrTypes, map[string]attr.Value{
			"enabled":       types.BoolUnknown(),
			"underlay_type": types.StringValue("geneve"),
			"vnid_range":    types.StringUnknown(),
		}))

	resp := runUpdate(r, newPlan(t, s, &planned), newState(t, s, &prior))
	if resp.Diagnostics.HasError() {
		t.Fatalf("update: %v", resp.Diagnostics)
	}
	if want := (virtualNetworkingAPI{Enabled: true, UnderlayType: "geneve", VnidRange: "1000:2000"}); sent.VirtualNetworking == nil || *sent.VirtualNetworking != want {
		t.Fatalf("PUT sent virtualNetworking %+v; want %+v, the blueprint's current values", sent.VirtualNetworking, want)
	}
}

// resmgr's blueprint update replaces the whole row, so a field a PUT leaves
// out is stored as null. The provider does not model the image library stores,
// and an update used to drop them; with a host holding the image library role,
// resmgr refuses that removal, so every update failed instead. An update must
// send back the stored fields the provider does not manage.
func TestBlueprintUpdateKeepsFieldsTheProviderDoesNotModel(t *testing.T) {
	t.Parallel()
	stored := strings.Replace(blueprintJSON, `"vncFloatingIp": "10.0.0.5",`,
		`"vncFloatingIp": "10.0.0.5", "imageLibraryStores": ["nfs"], "defaultImageLibraryStore": "nfs",`, 1)
	var sent map[string]json.RawMessage
	routes := blueprintRoutes()
	routes["GET "+blueprintPath] = reply(http.StatusOK, stored)
	routes["PUT "+blueprintPath] = func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&sent); err != nil {
			t.Errorf("decoding the PUT body: %v", err)
		}
		w.WriteHeader(http.StatusOK)
	}
	r := &blueprintResource{config: newRoutedResmgr(t, routes).config}
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
		VNCFloatingIP:             types.StringValue(blueprintVNCIP),
		StorageBackendsJSON:       types.StringValue(blueprintBackends),
	}
	planned := prior
	planned.DNSDomainName = types.StringValue("pcd.example")

	resp := runUpdate(r, newPlan(t, s, &planned), newState(t, s, &prior))
	if resp.Diagnostics.HasError() {
		t.Fatalf("update: %v", resp.Diagnostics)
	}
	if got := string(sent["imageLibraryStores"]); got != `["nfs"]` {
		t.Errorf("PUT sent imageLibraryStores %s, want the stored [\"nfs\"]", got)
	}
	if got := string(sent["defaultImageLibraryStore"]); got != `"nfs"` {
		t.Errorf("PUT sent defaultImageLibraryStore %s, want the stored \"nfs\"", got)
	}
	if got := string(sent["dnsDomainName"]); got != `"pcd.example"` {
		t.Errorf("PUT sent dnsDomainName %s, want the planned \"pcd.example\"", got)
	}
}

// Without the stored blueprint, a PUT would null the fields the provider does
// not manage, so an update whose read of it fails must send nothing and report
// the error; the next apply retries it.
func TestBlueprintUpdateSendsNothingWhenTheStoredBlueprintCannotBeRead(t *testing.T) {
	t.Parallel()
	routes := blueprintRoutes()
	routes["GET "+blueprintPath] = badGateway
	fake := newRoutedResmgr(t, routes)
	r := &blueprintResource{config: fake.config}
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
		VNCFloatingIP:             types.StringValue(blueprintVNCIP),
		StorageBackendsJSON:       types.StringValue(blueprintBackends),
	}
	planned := prior
	planned.DNSDomainName = types.StringValue("pcd.example")
	priorState := newState(t, s, &prior)

	resp := runUpdate(r, newPlan(t, s, &planned), priorState)
	if !resp.Diagnostics.HasError() {
		t.Fatal("update succeeded without reading the stored blueprint")
	}
	if sent := fake.received(); slices.Contains(sent, "PUT "+blueprintPath) {
		t.Fatalf("update sent %v; it must not write without the stored blueprint", sent)
	}
	if !resp.State.Raw.Equal(priorState.Raw) {
		t.Fatalf("update state = %v, want the prior state", resp.State.Raw)
	}
}
