// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package compute

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"

	"github.com/platform9/terraform-provider-pcd/internal/clients"
)

// runDataSourceRead calls d's Read the way the framework does, with a config
// that sets the string attributes in set and leaves every other one null, and
// a response state that starts as a null object.
func runDataSourceRead(t *testing.T, d datasource.DataSource, set map[string]string) datasource.ReadResponse {
	t.Helper()
	ctx := context.Background()
	var schemaResp datasource.SchemaResponse
	d.Schema(ctx, datasource.SchemaRequest{}, &schemaResp)
	s := schemaResp.Schema
	typ := s.Type().TerraformType(ctx).(tftypes.Object)
	vals := make(map[string]tftypes.Value, len(typ.AttributeTypes))
	for name, at := range typ.AttributeTypes {
		vals[name] = tftypes.NewValue(at, nil)
	}
	for name, v := range set {
		vals[name] = tftypes.NewValue(tftypes.String, v)
	}
	config := tfsdk.Config{Schema: s, Raw: tftypes.NewValue(typ, vals)}
	resp := datasource.ReadResponse{State: tfsdk.State{Schema: s, Raw: tftypes.NewValue(typ, nil)}}
	d.Read(ctx, datasource.ReadRequest{Config: config}, &resp)
	return resp
}

// reportsNoObject reports whether diags holds an error that names the answer
// without the object.
func reportsNoObject(diags diag.Diagnostics) bool {
	for _, d := range diags.Errors() {
		if strings.Contains(d.Detail(), clients.ErrNoObject.Error()) {
			return true
		}
	}
	return false
}

// gophercloud decodes an answer whose body lacks the object's key to a nil
// object and no error. These reads used to dereference it and crash the
// provider. Each must report an error and keep the row: the object may well
// exist, so the answer is no reason to drop it from state.
func TestReadRefusesAnAnswerWithoutTheObject(t *testing.T) {
	t.Parallel()
	reads := []struct {
		name, route string
		newResource func(*clients.Config) resource.Resource
		row         any
	}{
		{"servergroup", "GET /os-server-groups/sg-1",
			func(c *clients.Config) resource.Resource { return &servergroupResource{config: c} },
			&servergroupModel{ID: types.StringValue("sg-1"), Name: types.StringValue("web"),
				Policies: types.ListValueMust(types.StringType, []attr.Value{types.StringValue("anti-affinity")}),
				Members:  types.ListValueMust(types.StringType, []attr.Value{}), Region: types.StringValue("region-one")}},
		{"keypair", "GET /os-keypairs/deploy",
			func(c *clients.Config) resource.Resource { return &keypairResource{config: c} },
			&keypairModel{ID: types.StringValue("deploy"), Name: types.StringValue("deploy"),
				PublicKey: types.StringValue("ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIEXAMPLE deploy"), PrivateKey: types.StringNull(),
				Fingerprint: types.StringValue("aa:bb"), UserID: types.StringValue("user-1"), Region: types.StringValue("region-one")}},
		{"volume_attach", "GET /servers/srv-1/os-volume_attachments/vol-1",
			func(c *clients.Config) resource.Resource { return &volumeAttachResource{config: c} },
			&volumeAttachModel{ID: types.StringValue("vol-1"), InstanceID: types.StringValue("srv-1"),
				VolumeID: types.StringValue("vol-1"), Device: types.StringValue("/dev/vdb"), Region: types.StringValue("region-one")}},
		{"interface_attach", "GET /servers/srv-1/os-interface/port-1",
			func(c *clients.Config) resource.Resource { return &interfaceAttachResource{config: c} },
			&interfaceAttachModel{ID: types.StringValue("srv-1/port-1"), InstanceID: types.StringValue("srv-1"),
				PortID: types.StringValue("port-1"), NetworkID: types.StringValue("net-1"), FixedIP: types.StringValue("10.0.0.5"),
				MAC: types.StringValue("fa:16:3e:00:00:01"), PortState: types.StringValue("ACTIVE"), Region: types.StringValue("region-one")}},
	}
	for _, c := range reads {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			r := c.newResource(newFakeNova(t, novaRoutes{c.route: reply(http.StatusOK, `{}`)}).config)
			prior := newState(t, schemaOf(t, r), c.row)
			var resp resource.ReadResponse
			noPanic(t, func() { resp = runRead(r, prior) })
			if !reportsNoObject(resp.Diagnostics) {
				t.Fatalf("read diagnostics = %v; want an error for the answer without the object", resp.Diagnostics)
			}
			if !resp.State.Raw.Equal(prior.Raw) {
				t.Fatalf("read changed the row to %v; want it kept as %v", resp.State.Raw, prior.Raw)
			}
		})
	}
}

// The data sources that look an object up by ID or name used to dereference
// the nil object as well. Each must report an error and set nothing.
func TestDataSourceReadRefusesAnAnswerWithoutTheObject(t *testing.T) {
	t.Parallel()
	reads := []struct {
		name, route   string
		newDataSource func(*clients.Config) datasource.DataSource
		set           map[string]string
	}{
		{"flavor", "GET /flavors/flv-1",
			func(c *clients.Config) datasource.DataSource { return &flavorDataSource{config: c} },
			map[string]string{"flavor_id": "flv-1"}},
		{"keypair", "GET /os-keypairs/deploy",
			func(c *clients.Config) datasource.DataSource { return &keypairDataSource{config: c} },
			map[string]string{"name": "deploy"}},
	}
	for _, c := range reads {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			d := c.newDataSource(newFakeNova(t, novaRoutes{c.route: reply(http.StatusOK, `{}`)}).config)
			var resp datasource.ReadResponse
			noPanic(t, func() { resp = runDataSourceRead(t, d, c.set) })
			if !reportsNoObject(resp.Diagnostics) {
				t.Fatalf("read diagnostics = %v; want an error for the answer without the object", resp.Diagnostics)
			}
			if !resp.State.Raw.IsNull() {
				t.Fatalf("read set %v; want nothing set", resp.State.Raw)
			}
		})
	}
}
