// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package compute

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"

	"github.com/platform9/terraform-provider-pcd/internal/clients"
)

// A server whose build ends in ERROR (PortBindingFailed on the CE lab) stays in
// Nova. Create used to return without state, so Terraform forgot the server,
// the next apply booted another, and the first had to be deleted by hand.
// Create must return the error with the server in state (Terraform then taints
// it), and the refresh and delete a destroy runs must remove the server even
// though Nova reports ERROR until it is gone.
func TestCreateKeepsAServerThatFailedToBoot(t *testing.T) {
	ctx := context.Background()

	var mu sync.Mutex
	deleteCalled, getsAfterDelete := false, 0
	nova := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch r.Method + " " + r.URL.Path {
		case "POST /servers":
			w.WriteHeader(http.StatusAccepted)
			fmt.Fprint(w, `{"server": {"id": "srv-1"}}`)
		case "GET /servers/srv-1":
			if deleteCalled {
				getsAfterDelete++
				if getsAfterDelete > 1 {
					w.WriteHeader(http.StatusNotFound)
					return
				}
			}
			fmt.Fprint(w, `{"server": {"id": "srv-1", "name": "vm-1", "status": "ERROR", "metadata": {}, "addresses": {},
				"fault": {"code": 500, "message": "Binding failed for port p-1, please check neutron logs for more information."}}}`)
		case "DELETE /servers/srv-1":
			deleteCalled = true
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotImplemented)
		}
	}))
	defer nova.Close()

	r := &instanceResource{config: &clients.Config{
		Region: "region-one",
		Provider: &gophercloud.ProviderClient{EndpointLocator: func(gophercloud.EndpointOpts) (string, error) {
			return nova.URL + "/", nil
		}},
	}}
	var sch resource.SchemaResponse
	r.Schema(ctx, resource.SchemaRequest{}, &sch)
	s := sch.Schema
	nested := func(block string) types.ObjectType {
		return s.Blocks[block].(schema.ListNestedBlock).NestedObject.Type().(types.ObjectType)
	}
	network, diags := types.ListValueFrom(ctx, nested("network"), []instanceNetworkModel{
		{UUID: types.StringValue("net-1"), Name: types.StringNull(), Port: types.StringUnknown()},
	})
	planned := instanceModel{
		ID:                types.StringUnknown(),
		Name:              types.StringValue("vm-1"),
		ImageID:           types.StringValue("img-1"),
		ImageName:         types.StringNull(),
		FlavorID:          types.StringValue("flv-1"),
		FlavorName:        types.StringNull(),
		KeyPair:           types.StringNull(),
		SecurityGroups:    types.SetUnknown(types.StringType),
		Network:           network,
		BlockDevice:       types.ListNull(nested("block_device")),
		SchedulerHints:    types.ListNull(nested("scheduler_hints")),
		Metadata:          types.MapUnknown(types.StringType),
		MigrationPriority: types.StringUnknown(),
		UserData:          types.StringNull(),
		AvailabilityZone:  types.StringUnknown(),
		ConfigDrive:       types.BoolNull(),
		AccessIPv4:        types.StringUnknown(),
		Status:            types.StringUnknown(),
		Region:            types.StringUnknown(),
	}
	plan := tfsdk.Plan{Schema: s, Raw: tftypes.NewValue(s.Type().TerraformType(ctx), nil)}
	diags.Append(plan.Set(ctx, &planned)...)
	if diags.HasError() {
		t.Fatalf("building the plan: %v", diags)
	}

	createResp := resource.CreateResponse{State: tfsdk.State{Schema: s, Raw: tftypes.NewValue(s.Type().TerraformType(ctx), nil)}}
	r.Create(ctx, resource.CreateRequest{Plan: plan}, &createResp)
	if !createResp.Diagnostics.HasError() {
		t.Fatal("create succeeded; want the ERROR build reported")
	}
	if createResp.State.Raw.IsNull() {
		t.Fatal("create returned no state: Terraform forgets srv-1 and the next apply boots a second server")
	}
	if !createResp.State.Raw.IsFullyKnown() {
		t.Fatalf("create state holds unknown values, which Terraform refuses: %v", createResp.State.Raw)
	}
	var got instanceModel
	if d := createResp.State.Get(ctx, &got); d.HasError() {
		t.Fatalf("reading the create state: %v", d)
	}
	if got.ID.ValueString() != "srv-1" || got.Name.ValueString() != "vm-1" ||
		got.FlavorID.ValueString() != "flv-1" || got.ImageID.ValueString() != "img-1" {
		t.Fatalf("create state id=%s name=%s flavor_id=%s image_id=%s; want srv-1, vm-1, flv-1, img-1",
			got.ID, got.Name, got.FlavorID, got.ImageID)
	}

	// terraform destroy (or the replacing apply) refreshes the tainted instance first.
	readResp := resource.ReadResponse{State: createResp.State}
	r.Read(ctx, resource.ReadRequest{State: createResp.State}, &readResp)
	if readResp.Diagnostics.HasError() {
		t.Fatalf("refresh of the failed server: %v", readResp.Diagnostics)
	}
	if d := readResp.State.Get(ctx, &got); d.HasError() {
		t.Fatalf("reading the refreshed state: %v", d)
	}
	if got.Status.ValueString() != "ERROR" {
		t.Fatalf("refreshed status = %s, want ERROR", got.Status)
	}

	deleteResp := resource.DeleteResponse{State: readResp.State}
	r.Delete(ctx, resource.DeleteRequest{State: readResp.State}, &deleteResp)
	if deleteResp.Diagnostics.HasError() {
		t.Fatalf("delete of a server in ERROR: %v", deleteResp.Diagnostics)
	}
	mu.Lock()
	defer mu.Unlock()
	if !deleteCalled {
		t.Fatal("delete never called DELETE /servers/srv-1")
	}
	if getsAfterDelete < 2 {
		t.Fatalf("delete returned after %d polls; want it to wait out ERROR until Nova answers 404", getsAfterDelete)
	}
}
