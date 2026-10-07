// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package identity

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

// newState returns a state of r's schema that holds model, a pointer to the
// resource's model struct.
func newState(t *testing.T, r resource.Resource, model any) tfsdk.State {
	t.Helper()
	ctx := context.Background()
	var schemaResp resource.SchemaResponse
	r.Schema(ctx, resource.SchemaRequest{}, &schemaResp)
	s := schemaResp.Schema
	state := tfsdk.State{Schema: s, Raw: tftypes.NewValue(s.Type().TerraformType(ctx), nil)}
	if d := state.Set(ctx, model); d.HasError() {
		t.Fatalf("building the state: %v", d)
	}
	return state
}

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

// The rows the read and update tests start from, each as Terraform recorded it.
func projectRow() *projectModel {
	return &projectModel{ID: types.StringValue("proj-1"), Name: types.StringValue("workload"),
		Description: types.StringValue(""), DomainID: types.StringValue("default"), Enabled: types.BoolValue(true),
		IsDomain: types.BoolValue(false), ParentID: types.StringValue("default"),
		Tags: types.SetValueMust(types.StringType, []attr.Value{}), Region: types.StringValue("region-one")}
}

func groupRow() *groupModel {
	return &groupModel{ID: types.StringValue("grp-1"), Name: types.StringValue("operators"),
		Description: types.StringValue(""), DomainID: types.StringValue("default"), Region: types.StringValue("region-one")}
}

func roleRow() *roleModel {
	return &roleModel{ID: types.StringValue("role-1"), Name: types.StringValue("auditor"),
		DomainID: types.StringValue(""), Region: types.StringValue("region-one")}
}

func userRow() *userModel {
	return &userModel{ID: types.StringValue("user-2"), Name: types.StringValue("deployer"),
		Description: types.StringValue(""), DomainID: types.StringValue("default"),
		DefaultProjectID: types.StringValue(""), Enabled: types.BoolValue(true), Password: types.StringNull(),
		Region: types.StringValue("region-one")}
}

func appCredRow() *appCredModel {
	return &appCredModel{ID: types.StringValue("ac-1"), Name: types.StringValue("ci"), Description: types.StringValue(""),
		Secret: types.StringValue("secret-1"), ProjectID: types.StringValue("proj-1"),
		Roles:     types.SetValueMust(types.StringType, []attr.Value{types.StringValue("member")}),
		ExpiresAt: types.StringNull(), Unrestricted: types.BoolValue(false), Region: types.StringValue("region-one")}
}

// gophercloud decodes an answer whose body lacks the object's key to a nil
// object and no error. These reads used to dereference it and crash the
// provider. Each must report an error and keep the row: the object may well
// exist, so the answer is no reason to drop it from state.
func TestReadRefusesAnAnswerWithoutTheObject(t *testing.T) {
	t.Parallel()
	reads := []struct {
		name        string
		routes      map[string]http.HandlerFunc
		newResource func(*clients.Config) resource.Resource
		row         any
	}{
		{"project", map[string]http.HandlerFunc{"GET /v3/projects/proj-1": reply(http.StatusOK, `{}`)},
			func(c *clients.Config) resource.Resource { return &projectResource{config: c} }, projectRow()},
		{"group", map[string]http.HandlerFunc{"GET /v3/groups/grp-1": reply(http.StatusOK, `{}`)},
			func(c *clients.Config) resource.Resource { return &groupResource{config: c} }, groupRow()},
		{"role", map[string]http.HandlerFunc{"GET /v3/roles/role-1": reply(http.StatusOK, `{}`)},
			func(c *clients.Config) resource.Resource { return &roleResource{config: c} }, roleRow()},
		{"user", map[string]http.HandlerFunc{"GET /v3/users/user-2": reply(http.StatusOK, `{}`)},
			func(c *clients.Config) resource.Resource { return &userResource{config: c} }, userRow()},
		{"application_credential", map[string]http.HandlerFunc{
			"GET /v3/auth/tokens":                               reply(http.StatusOK, tokenWithUser),
			"GET /v3/users/user-1/application_credentials/ac-1": reply(http.StatusOK, `{}`),
		},
			func(c *clients.Config) resource.Resource { return &appCredResource{config: c} }, appCredRow()},
	}
	for _, c := range reads {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			r := c.newResource(newFakeKeystone(t, c.routes))
			prior := newState(t, r, c.row)
			resp := resource.ReadResponse{State: prior}
			noPanic(t, func() { r.Read(context.Background(), resource.ReadRequest{State: prior}, &resp) })
			if !reportsNoObject(resp.Diagnostics) {
				t.Fatalf("read diagnostics = %v; want an error for the answer without the object", resp.Diagnostics)
			}
			if !resp.State.Raw.Equal(prior.Raw) {
				t.Fatalf("read changed the row to %v; want it kept as %v", resp.State.Raw, prior.Raw)
			}
		})
	}
}

// An update answer without the object used to crash the provider the same
// way. Each update must report an error and keep the prior row.
func TestUpdateRefusesAnAnswerWithoutTheObject(t *testing.T) {
	t.Parallel()
	renamedProject := projectRow()
	renamedProject.Name = types.StringValue("workload-2")
	renamedGroup := groupRow()
	renamedGroup.Name = types.StringValue("operators-2")
	renamedRole := roleRow()
	renamedRole.Name = types.StringValue("auditor-2")
	renamedUser := userRow()
	renamedUser.Name = types.StringValue("deployer-2")
	updates := []struct {
		name, route  string
		newResource  func(*clients.Config) resource.Resource
		row, planned any
	}{
		{"project", "PATCH /v3/projects/proj-1",
			func(c *clients.Config) resource.Resource { return &projectResource{config: c} }, projectRow(), renamedProject},
		{"group", "PATCH /v3/groups/grp-1",
			func(c *clients.Config) resource.Resource { return &groupResource{config: c} }, groupRow(), renamedGroup},
		{"role", "PATCH /v3/roles/role-1",
			func(c *clients.Config) resource.Resource { return &roleResource{config: c} }, roleRow(), renamedRole},
		{"user", "PATCH /v3/users/user-2",
			func(c *clients.Config) resource.Resource { return &userResource{config: c} }, userRow(), renamedUser},
	}
	for _, c := range updates {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			r := c.newResource(newFakeKeystone(t, map[string]http.HandlerFunc{c.route: reply(http.StatusOK, `{}`)}))
			prior := newState(t, r, c.row)
			plan := tfsdk.Plan(newState(t, r, c.planned))
			resp := resource.UpdateResponse{State: prior}
			noPanic(t, func() {
				r.Update(context.Background(), resource.UpdateRequest{Plan: plan, State: prior}, &resp)
			})
			if !reportsNoObject(resp.Diagnostics) {
				t.Fatalf("update diagnostics = %v; want an error for the answer without the object", resp.Diagnostics)
			}
			if !resp.State.Raw.Equal(prior.Raw) {
				t.Fatalf("update changed the row to %v; want the prior row %v", resp.State.Raw, prior.Raw)
			}
		})
	}
}

// The data sources that look an object up by ID used to dereference the nil
// object as well, and so did pcd_identity_auth_scope for a token answer
// without its user. Each must report an error and set nothing.
func TestDataSourceReadRefusesAnAnswerWithoutTheObject(t *testing.T) {
	t.Parallel()
	reads := []struct {
		name, route, body string
		newDataSource     func(*clients.Config) datasource.DataSource
		set               map[string]string
	}{
		{"project", "GET /v3/projects/proj-1", `{}`,
			func(c *clients.Config) datasource.DataSource { return &projectDataSource{config: c} },
			map[string]string{"project_id": "proj-1"}},
		{"group", "GET /v3/groups/grp-1", `{}`,
			func(c *clients.Config) datasource.DataSource { return &groupDataSource{config: c} },
			map[string]string{"group_id": "grp-1"}},
		{"role", "GET /v3/roles/role-1", `{}`,
			func(c *clients.Config) datasource.DataSource { return &roleDataSource{config: c} },
			map[string]string{"role_id": "role-1"}},
		{"user", "GET /v3/users/user-2", `{}`,
			func(c *clients.Config) datasource.DataSource { return &userDataSource{config: c} },
			map[string]string{"user_id": "user-2"}},
		{"auth_scope", "GET /v3/auth/tokens", `{"token": {}}`,
			func(c *clients.Config) datasource.DataSource { return &authScopeDataSource{config: c} }, nil},
	}
	for _, c := range reads {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			d := c.newDataSource(newFakeKeystone(t, map[string]http.HandlerFunc{c.route: reply(http.StatusOK, c.body)}))
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
