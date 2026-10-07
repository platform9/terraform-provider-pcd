// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package compute

import (
	"net/http"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/platform9/terraform-provider-pcd/internal/clients"
)

// noPanic runs f and fails the test if f panics. A panic in a resource method
// crashes the provider plugin, which fails the whole apply.
func noPanic(t *testing.T, f func()) {
	t.Helper()
	defer func() {
		if p := recover(); p != nil {
			t.Fatalf("panicked: %v", p)
		}
	}()
	f()
}

// gophercloud decodes a create answer whose body lacks the object's key to a
// nil object and no error. These creates used to dereference it and crash the
// provider. Each must report an error and leave nothing in state.
func TestCreateRefusesAnAnswerWithoutTheObject(t *testing.T) {
	t.Parallel()
	creates := []struct {
		name, route string
		newResource func(*clients.Config) resource.Resource
		plan        func(*testing.T, resource.Resource) tfsdk.Plan
	}{
		{"flavor", "POST " + flavorsPath,
			func(c *clients.Config) resource.Resource { return &flavorResource{config: c} },
			func(t *testing.T, r resource.Resource) tfsdk.Plan { return flavorPlan(t, r.(*flavorResource), nil) }},
		{"volume_attach", "POST /servers/srv-1/os-volume_attachments",
			func(c *clients.Config) resource.Resource { return &volumeAttachResource{config: c} },
			func(t *testing.T, r resource.Resource) tfsdk.Plan {
				return newPlan(t, schemaOf(t, r), &volumeAttachModel{ID: types.StringUnknown(),
					InstanceID: types.StringValue("srv-1"), VolumeID: types.StringValue("vol-1"),
					Device: types.StringUnknown(), Region: types.StringUnknown()})
			}},
		{"interface_attach", "POST /servers/srv-1/os-interface",
			func(c *clients.Config) resource.Resource { return &interfaceAttachResource{config: c} },
			func(t *testing.T, r resource.Resource) tfsdk.Plan {
				return newPlan(t, schemaOf(t, r), &interfaceAttachModel{ID: types.StringUnknown(),
					InstanceID: types.StringValue("srv-1"), PortID: types.StringValue("port-1"),
					NetworkID: types.StringUnknown(), FixedIP: types.StringUnknown(), MAC: types.StringUnknown(),
					PortState: types.StringUnknown(), Region: types.StringUnknown()})
			}},
		{"servergroup", "POST /os-server-groups",
			func(c *clients.Config) resource.Resource { return &servergroupResource{config: c} },
			func(t *testing.T, r resource.Resource) tfsdk.Plan {
				return newPlan(t, schemaOf(t, r), &servergroupModel{ID: types.StringUnknown(),
					Name: types.StringValue("web"), Policies: types.ListValueMust(types.StringType, []attr.Value{types.StringValue("anti-affinity")}),
					Members: types.ListUnknown(types.StringType), Region: types.StringUnknown()})
			}},
		{"keypair", "POST /os-keypairs",
			func(c *clients.Config) resource.Resource { return &keypairResource{config: c} },
			func(t *testing.T, r resource.Resource) tfsdk.Plan {
				return newPlan(t, schemaOf(t, r), &keypairModel{ID: types.StringUnknown(),
					Name: types.StringValue("deploy"), PublicKey: types.StringValue("ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIEXAMPLE deploy"),
					PrivateKey: types.StringUnknown(), Fingerprint: types.StringUnknown(), UserID: types.StringUnknown(),
					Region: types.StringUnknown()})
			}},
	}
	for _, c := range creates {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			r := c.newResource(newFakeNova(t, novaRoutes{c.route: reply(http.StatusOK, `{}`)}).config)
			var resp resource.CreateResponse
			noPanic(t, func() { resp = runCreate(r, c.plan(t, r)) })
			if !resp.Diagnostics.HasError() {
				t.Fatal("create succeeded; want an error for an answer without the object")
			}
			if !resp.State.Raw.IsNull() {
				t.Fatalf("create left a row: %v", resp.State.Raw)
			}
		})
	}
}
