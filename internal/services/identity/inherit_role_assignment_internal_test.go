// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package identity

import (
	"context"
	"fmt"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

// assignmentModel builds a role assignment model; "" means the attribute is unset.
func assignmentModel(domainID, projectID, groupID, userID, roleID string) roleAssignmentModel {
	return roleAssignmentModel{
		ID:        types.StringUnknown(),
		RoleID:    types.StringValue(roleID),
		UserID:    optionalString(userID),
		GroupID:   optionalString(groupID),
		ProjectID: optionalString(projectID),
		DomainID:  optionalString(domainID),
		Region:    types.StringUnknown(),
	}
}

// Each grant goes to Keystone's OS-INHERIT path for its target and actor:
// create PUTs it, refresh HEADs it, destroy DELETEs it, and the ID has the
// same five parts as pcd_identity_role_assignment.
func TestInheritRoleAssignmentLifecycle(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name   string
		model  roleAssignmentModel
		path   string
		wantID string
	}{
		{name: "user on a domain", model: assignmentModel("d-1", "", "", "u-1", "r-1"),
			path: "/v3/OS-INHERIT/domains/d-1/users/u-1/roles/r-1/inherited_to_projects", wantID: "d-1///u-1/r-1"},
		{name: "group on a project", model: assignmentModel("", "p-1", "g-1", "", "r-1"),
			path: "/v3/OS-INHERIT/projects/p-1/groups/g-1/roles/r-1/inherited_to_projects", wantID: "/p-1/g-1//r-1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newKeystoneFake(t, map[string]func() (int, string){
				"PUT " + tc.path:    keystoneReply(204, ""),
				"HEAD " + tc.path:   keystoneReply(204, ""),
				"DELETE " + tc.path: keystoneReply(204, ""),
			})
			r := &inheritRoleAssignmentResource{config: fakeConfig(f.srv.URL)}
			var sch resource.SchemaResponse
			r.Schema(ctx, resource.SchemaRequest{}, &sch)
			s := sch.Schema

			plan := tfsdk.Plan{Schema: s, Raw: tftypes.NewValue(s.Type().TerraformType(ctx), nil)}
			if d := plan.Set(ctx, &tc.model); d.HasError() {
				t.Fatalf("building the plan: %v", d)
			}
			createResp := resource.CreateResponse{State: tfsdk.State{Schema: s, Raw: tftypes.NewValue(s.Type().TerraformType(ctx), nil)}}
			r.Create(ctx, resource.CreateRequest{Plan: plan}, &createResp)
			if createResp.Diagnostics.HasError() {
				t.Fatalf("create: %v", createResp.Diagnostics)
			}
			var got roleAssignmentModel
			createResp.State.Get(ctx, &got)
			if got.ID.ValueString() != tc.wantID || got.Region.ValueString() != "region-one" {
				t.Fatalf("create state id=%s region=%s; want %s, region-one", got.ID, got.Region, tc.wantID)
			}

			readResp := resource.ReadResponse{State: createResp.State}
			r.Read(ctx, resource.ReadRequest{State: createResp.State}, &readResp)
			if readResp.Diagnostics.HasError() || readResp.State.Raw.IsNull() {
				t.Fatalf("read: %v (removed: %v)", readResp.Diagnostics, readResp.State.Raw.IsNull())
			}

			deleteResp := resource.DeleteResponse{State: readResp.State}
			r.Delete(ctx, resource.DeleteRequest{State: readResp.State}, &deleteResp)
			if deleteResp.Diagnostics.HasError() {
				t.Fatalf("delete: %v", deleteResp.Diagnostics)
			}
			want := []string{"PUT " + tc.path, "HEAD " + tc.path, "DELETE " + tc.path}
			if fmt.Sprint(f.paths()) != fmt.Sprint(want) {
				t.Fatalf("calls = %v, want %v", f.paths(), want)
			}
		})
	}
}

// Import reads the ID back: a grant Keystone confirms fills every attribute
// from the ID; one it does not know (HEAD 404) drops out of state, so the
// import fails with "Cannot import non-existent remote object". A destroy of a
// grant someone already removed succeeds.
func TestInheritRoleAssignmentImportAndGone(t *testing.T) {
	ctx := context.Background()
	const path = "/v3/OS-INHERIT/domains/default/users/u-1/roles/r-1/inherited_to_projects"
	exists := true
	f := newKeystoneFake(t, map[string]func() (int, string){
		"HEAD " + path: func() (int, string) {
			if exists {
				return 204, ""
			}
			return 404, ""
		},
		"DELETE " + path: keystoneReply(404, `{"error": {"message": "not found"}}`),
	})
	r := &inheritRoleAssignmentResource{config: fakeConfig(f.srv.URL)}
	var sch resource.SchemaResponse
	r.Schema(ctx, resource.SchemaRequest{}, &sch)
	s := sch.Schema

	importResp := resource.ImportStateResponse{State: tfsdk.State{Schema: s, Raw: tftypes.NewValue(s.Type().TerraformType(ctx), nil)}}
	r.ImportState(ctx, resource.ImportStateRequest{ID: "default///u-1/r-1"}, &importResp)
	readResp := resource.ReadResponse{State: importResp.State}
	r.Read(ctx, resource.ReadRequest{State: importResp.State}, &readResp)
	if readResp.Diagnostics.HasError() {
		t.Fatalf("read after import: %v", readResp.Diagnostics)
	}
	var got roleAssignmentModel
	readResp.State.Get(ctx, &got)
	if got.DomainID.ValueString() != "default" || got.UserID.ValueString() != "u-1" || got.RoleID.ValueString() != "r-1" ||
		!got.GroupID.IsNull() || !got.ProjectID.IsNull() {
		t.Fatalf("imported state = %+v; want domain default, user u-1, role r-1, group and project null", got)
	}

	exists = false
	goneResp := resource.ReadResponse{State: readResp.State}
	r.Read(ctx, resource.ReadRequest{State: readResp.State}, &goneResp)
	if goneResp.Diagnostics.HasError() || !goneResp.State.Raw.IsNull() {
		t.Fatalf("read of a removed grant: diags %v, removed %v; want it dropped from state", goneResp.Diagnostics, goneResp.State.Raw.IsNull())
	}

	deleteResp := resource.DeleteResponse{State: readResp.State}
	r.Delete(ctx, resource.DeleteRequest{State: readResp.State}, &deleteResp)
	if deleteResp.Diagnostics.HasError() {
		t.Fatalf("delete of a removed grant: %v", deleteResp.Diagnostics)
	}
}

// Only a 404 means an inherited grant is gone: a refresh that Keystone refuses
// (403) or fails (500) is an error, and the grant stays in state.
func TestInheritRoleAssignmentReadKeepsTheGrantOnAnError(t *testing.T) {
	ctx := context.Background()
	const path = "/v3/OS-INHERIT/domains/d-1/users/u-1/roles/r-1/inherited_to_projects"
	for _, status := range []int{403, 500} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			f := newKeystoneFake(t, map[string]func() (int, string){"HEAD " + path: keystoneReply(status, "")})
			r := &inheritRoleAssignmentResource{config: fakeConfig(f.srv.URL)}
			var sch resource.SchemaResponse
			r.Schema(ctx, resource.SchemaRequest{}, &sch)
			s := sch.Schema
			state := tfsdk.State{Schema: s, Raw: tftypes.NewValue(s.Type().TerraformType(ctx), nil)}
			m := assignmentModel("d-1", "", "", "u-1", "r-1")
			m.ID = types.StringValue("d-1///u-1/r-1")
			m.Region = types.StringValue("region-one")
			if d := state.Set(ctx, &m); d.HasError() {
				t.Fatalf("building the state: %v", d)
			}
			readResp := resource.ReadResponse{State: state}
			r.Read(ctx, resource.ReadRequest{State: state}, &readResp)
			var got []string
			for _, d := range readResp.Diagnostics.Errors() {
				got = append(got, d.Summary())
			}
			if fmt.Sprint(got) != "[identity: reading inherited role assignment]" || readResp.State.Raw.IsNull() {
				t.Fatalf("read errors = %v, removed %v; want [identity: reading inherited role assignment] and the grant kept",
					got, readResp.State.Raw.IsNull())
			}
		})
	}
}

// An empty import ID, or one with an empty role, actor or target, names no
// grant, so import refuses it before it sets any attribute; an ID without five
// parts is refused by the refresh that follows, before any request.
func TestInheritRoleAssignmentImportRefusesBadIDs(t *testing.T) {
	ctx := context.Background()
	f := newKeystoneFake(t, map[string]func() (int, string){})
	r := &inheritRoleAssignmentResource{config: fakeConfig(f.srv.URL)}
	var sch resource.SchemaResponse
	r.Schema(ctx, resource.SchemaRequest{}, &sch)
	s := sch.Schema
	for _, tc := range []struct {
		name, id, wantImportError, wantReadError string
	}{
		{name: "empty", id: "", wantImportError: "Import needs an inherited role assignment ID"},
		{name: "empty role", id: "d-1///u-1/", wantImportError: "Import needs an inherited role assignment ID"},
		{name: "no actor", id: "d-1////r-1", wantImportError: "Import needs an inherited role assignment ID"},
		{name: "no target", id: "///u-1/r-1", wantImportError: "Import needs an inherited role assignment ID"},
		{name: "two actors", id: "d-1//g-1/u-1/r-1", wantImportError: "Import needs an inherited role assignment ID"},
		{name: "two targets", id: "d-1/p-1//u-1/r-1", wantImportError: "Import needs an inherited role assignment ID"},
		{name: "three parts", id: "d-1/u-1/r-1", wantReadError: "identity: parsing inherited role assignment id"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			importResp := resource.ImportStateResponse{State: tfsdk.State{Schema: s, Raw: tftypes.NewValue(s.Type().TerraformType(ctx), nil)}}
			r.ImportState(ctx, resource.ImportStateRequest{ID: tc.id}, &importResp)
			var got []string
			for _, d := range importResp.Diagnostics.Errors() {
				got = append(got, d.Summary())
			}
			if tc.wantImportError != "" {
				if fmt.Sprint(got) != fmt.Sprint([]string{tc.wantImportError}) || !importResp.State.Raw.IsNull() {
					t.Fatalf("import errors = %v, state null %v; want [%s] and no state", got, importResp.State.Raw.IsNull(), tc.wantImportError)
				}
				return
			}
			if len(got) != 0 {
				t.Fatalf("import errors = %v, want none", got)
			}
			readResp := resource.ReadResponse{State: importResp.State}
			r.Read(ctx, resource.ReadRequest{State: importResp.State}, &readResp)
			got = nil
			for _, d := range readResp.Diagnostics.Errors() {
				got = append(got, d.Summary())
			}
			if fmt.Sprint(got) != fmt.Sprint([]string{tc.wantReadError}) {
				t.Fatalf("read errors = %v, want [%s]", got, tc.wantReadError)
			}
			if calls := f.paths(); len(calls) != 0 {
				t.Fatalf("requests = %v, want none", calls)
			}
		})
	}
}

// Exactly one actor and exactly one target must be configured; a reference
// that is still unknown at plan time is not an error yet.
func TestInheritRoleAssignmentConfigValidators(t *testing.T) {
	ctx := context.Background()
	r := &inheritRoleAssignmentResource{}
	var sch resource.SchemaResponse
	r.Schema(ctx, resource.SchemaRequest{}, &sch)
	s := sch.Schema
	unknownUser := assignmentModel("d-1", "", "", "", "r-1")
	unknownUser.UserID = types.StringUnknown()
	for _, tc := range []struct {
		name       string
		model      roleAssignmentModel
		wantErrors []string
	}{
		{name: "user on a domain", model: assignmentModel("d-1", "", "", "u-1", "r-1")},
		{name: "unknown user", model: unknownUser},
		{name: "no actor", model: assignmentModel("d-1", "", "", "", "r-1"), wantErrors: []string{"Missing Attribute Configuration"}},
		{name: "user and group", model: assignmentModel("d-1", "", "g-1", "u-1", "r-1"), wantErrors: []string{"Invalid Attribute Combination"}},
		{name: "domain and project", model: assignmentModel("d-1", "p-1", "", "u-1", "r-1"), wantErrors: []string{"Invalid Attribute Combination"}},
		{name: "no target", model: assignmentModel("", "", "", "u-1", "r-1"), wantErrors: []string{"Missing Attribute Configuration"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.model.ID = types.StringNull()
			tc.model.Region = types.StringNull()
			st := tfsdk.State{Schema: s, Raw: tftypes.NewValue(s.Type().TerraformType(ctx), nil)}
			if d := st.Set(ctx, &tc.model); d.HasError() {
				t.Fatalf("building the config: %v", d)
			}
			cfg := tfsdk.Config{Schema: s, Raw: st.Raw}
			var got []string
			for _, v := range r.ConfigValidators(ctx) {
				resp := resource.ValidateConfigResponse{}
				v.ValidateResource(ctx, resource.ValidateConfigRequest{Config: cfg}, &resp)
				for _, d := range resp.Diagnostics.Errors() {
					got = append(got, d.Summary())
				}
			}
			if fmt.Sprint(got) != fmt.Sprint(tc.wantErrors) {
				t.Fatalf("errors = %v, want %v", got, tc.wantErrors)
			}
		})
	}
}

// pcd_identity_role_assignment finds only direct grants: it lists the roles on
// the domain or project itself, which Keystone answers without inherited
// grants. Before, it asked /v3/role_assignments, which also returns an
// inherited grant on the same domain or project, so it imported grants it
// could not remove.
// Only a 404 means the grant is gone; any other error is returned, so a refresh
// that Keystone refuses or fails keeps the grant in state.
func TestRoleAssignmentExistsSeesDirectGrantsOnly(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name                               string
		domainID, projectID, groupID, user string
		path, body                         string
		status                             int
		want, wantErr                      bool
	}{
		{name: "direct grant present", domainID: "d-1", user: "u-1", path: "/domains/d-1/users/u-1/roles",
			body: `{"roles": [{"id": "r-other", "name": "x"}, {"id": "r-1", "name": "member"}], "links": {"next": null}}`, want: true},
		{name: "only an inherited grant", domainID: "d-1", user: "u-1", path: "/domains/d-1/users/u-1/roles",
			body: `{"roles": [], "links": {"next": null}}`},
		{name: "group on a project", projectID: "p-1", groupID: "g-1", path: "/projects/p-1/groups/g-1/roles",
			body: `{"roles": [{"id": "r-1", "name": "member"}], "links": {"next": null}}`, want: true},
		{name: "user deleted", domainID: "d-1", user: "u-gone", path: "/domains/d-1/users/u-gone/roles", status: 404,
			body: `{"error": {"message": "Could not find user: u-gone."}}`},
		{name: "forbidden", domainID: "d-1", user: "u-1", path: "/domains/d-1/users/u-1/roles", status: 403,
			body: `{"error": {"message": "x"}}`, wantErr: true},
		{name: "server error", domainID: "d-1", user: "u-1", path: "/domains/d-1/users/u-1/roles", status: 500,
			body: `{"error": {"message": "x"}}`, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status := tc.status
			if status == 0 {
				status = 200
			}
			f := newKeystoneFake(t, map[string]func() (int, string){"GET " + tc.path: keystoneReply(status, tc.body)})
			got, err := roleAssignmentExists(ctx, f.bareClient(), tc.domainID, tc.projectID, tc.groupID, tc.user, "r-1")
			if tc.wantErr {
				if err == nil {
					t.Fatalf("roleAssignmentExists = %v with no error; want the %d returned", got, status)
				}
				return
			}
			if err != nil {
				t.Fatalf("roleAssignmentExists: %v", err)
			}
			if got != tc.want {
				t.Fatalf("roleAssignmentExists = %v, want %v", got, tc.want)
			}
		})
	}
}
