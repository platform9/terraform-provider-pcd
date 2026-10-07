// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package networking

import (
	"context"
	"net/http"
	"strings"
	"testing"

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

// gophercloud decodes an answer whose body lacks the security_group_rule key
// to a nil rule and no error. Read used to dereference it and crash the
// provider. It must report an error and keep the row: the rule may well
// exist, so the answer is no reason to drop it from state.
func TestSecgroupRuleReadRefusesAnAnswerWithoutTheRule(t *testing.T) {
	t.Parallel()
	r := &secgroupRuleResource{config: newFakeNeutron(t, neutronRoutes{
		"GET /v2.0/security-group-rules/rule-ssh": reply(http.StatusOK, `{}`),
	}).config}
	prior := newState(t, schemaOf(t, r), &secgroupRuleModel{
		ID: types.StringValue("rule-ssh"), Direction: types.StringValue("ingress"), EtherType: types.StringValue("IPv4"),
		SecurityGroupID: types.StringValue(secgroupID), Protocol: types.StringValue("tcp"),
		PortRangeMin: types.Int64Value(22), PortRangeMax: types.Int64Value(22),
		RemoteGroupID: types.StringValue(""), RemoteIPPrefix: types.StringValue("0.0.0.0/0"),
		Description: types.StringNull(), TenantID: types.StringValue("proj-1"), Region: types.StringValue("region-one"),
	})
	var resp resource.ReadResponse
	noPanic(t, func() { resp = runRead(r, prior) })
	if !reportsNoObject(resp.Diagnostics) {
		t.Fatalf("read diagnostics = %v; want an error for the answer without the rule", resp.Diagnostics)
	}
	if !resp.State.Raw.Equal(prior.Raw) {
		t.Fatalf("read changed the row to %v; want it kept as %v", resp.State.Raw, prior.Raw)
	}
}

// The data sources that look an object up by ID used to dereference the nil
// object as well. Each must report an error and set nothing.
func TestDataSourceReadRefusesAnAnswerWithoutTheObject(t *testing.T) {
	t.Parallel()
	reads := []struct {
		name, route   string
		newDataSource func(*clients.Config) datasource.DataSource
		set           map[string]string
	}{
		{"router", "GET " + routerPath,
			func(c *clients.Config) datasource.DataSource { return &routerDataSource{config: c} },
			map[string]string{"router_id": routerID}},
		{"subnet", "GET " + subnetPath,
			func(c *clients.Config) datasource.DataSource { return &subnetDataSource{config: c} },
			map[string]string{"subnet_id": subnetID}},
		{"secgroup", "GET " + secgroupPath,
			func(c *clients.Config) datasource.DataSource { return &secgroupDataSource{config: c} },
			map[string]string{"secgroup_id": secgroupID}},
		{"qos_policy", "GET " + qosPolicyPath,
			func(c *clients.Config) datasource.DataSource { return &qosPolicyDataSource{config: c} },
			map[string]string{"policy_id": qosPolicyID}},
	}
	for _, c := range reads {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			d := c.newDataSource(newFakeNeutron(t, neutronRoutes{c.route: reply(http.StatusOK, `{}`)}).config)
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
