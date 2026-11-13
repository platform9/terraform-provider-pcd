// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package identity

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

// projectStateFor builds the state of a pcd_identity_project with this ID.
func projectStateFor(t *testing.T, r *projectResource, id string, isDomain bool) tfsdk.State {
	t.Helper()
	ctx := context.Background()
	var sch resource.SchemaResponse
	r.Schema(ctx, resource.SchemaRequest{}, &sch)
	s := sch.Schema
	st := tfsdk.State{Schema: s, Raw: tftypes.NewValue(s.Type().TerraformType(ctx), nil)}
	if d := st.Set(ctx, &projectModel{
		ID:          types.StringValue(id),
		Name:        types.StringValue("tf-proj"),
		Description: types.StringValue(""),
		DomainID:    types.StringValue("default"),
		Enabled:     types.BoolValue(true),
		IsDomain:    types.BoolValue(isDomain),
		ParentID:    types.StringValue("default"),
		Tags:        types.SetValueMust(types.StringType, nil),
		Region:      types.StringValue("region-one"),
	}); d.HasError() {
		t.Fatalf("building the state: %v", d)
	}
	return st
}

// Keystone refuses to delete an enabled project that acts as a domain (403
// "Cannot delete a domain that is enabled"), so Delete disables it first; an
// ordinary project is deleted without a PATCH, exactly as before.
func TestProjectDeleteDisablesADomainFirst(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name       string
		id         string
		isDomain   bool
		patch, del int
		wantCalls  []string
		wantError  string
		wantDetail string
	}{
		{name: "ordinary project", id: "p-1", wantCalls: []string{"DELETE /v3/projects/p-1"}},
		{name: "project acting as a domain", id: "p-1", isDomain: true, patch: 200,
			wantCalls: []string{"GET /v3/auth/tokens", "PATCH /v3/projects/p-1", "DELETE /v3/projects/p-1"}},
		{name: "domain already gone", id: "p-1", isDomain: true, patch: 404,
			wantCalls: []string{"GET /v3/auth/tokens", "PATCH /v3/projects/p-1"}},
		{name: "disable refused", id: "p-1", isDomain: true, patch: 403,
			wantCalls: []string{"GET /v3/auth/tokens", "PATCH /v3/projects/p-1"},
			wantError: "identity: disabling project acting as a domain before deleting it"},
		{name: "the provider's own domain", id: "dom-user", isDomain: true,
			wantCalls: []string{"GET /v3/auth/tokens"}, wantError: "Refusing to destroy a protected domain"},
		{name: "the default domain", id: "default", isDomain: true,
			wantError: "Refusing to destroy a protected domain"},
		{name: "delete refused after the disable", id: "p-1", isDomain: true, patch: 200, del: 500,
			wantCalls: []string{"GET /v3/auth/tokens", "PATCH /v3/projects/p-1", "DELETE /v3/projects/p-1"},
			wantError: "identity: deleting project", wantDetail: "was disabled but could not be deleted, so it stays disabled"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newKeystoneFake(t, map[string]func() (int, string){
				"GET /v3/auth/tokens": keystoneReply(200, tokenBody),
				"PATCH /v3/projects/" + tc.id: func() (int, string) {
					if tc.patch == 200 {
						return 200, `{"project": {"id": "` + tc.id + `", "name": "tf-proj", "is_domain": true, "enabled": false}}`
					}
					return tc.patch, `{"error": {"message": "no"}}`
				},
				"DELETE /v3/projects/" + tc.id: func() (int, string) {
					if tc.del != 0 {
						return tc.del, `{"error": {"message": "no"}}`
					}
					return 204, ""
				},
			})
			r := &projectResource{config: fakeConfig(f.srv.URL)}
			st := projectStateFor(t, r, tc.id, tc.isDomain)
			resp := resource.DeleteResponse{State: st}
			r.Delete(ctx, resource.DeleteRequest{State: st}, &resp)
			var summaries []string
			for _, d := range resp.Diagnostics.Errors() {
				summaries = append(summaries, d.Summary())
			}
			if tc.wantError == "" && len(summaries) != 0 {
				t.Fatalf("delete errors = %v, want none", summaries)
			}
			if tc.wantError != "" && (len(summaries) != 1 || summaries[0] != tc.wantError) {
				t.Fatalf("delete errors = %v, want [%s]", summaries, tc.wantError)
			}
			if tc.wantDetail != "" && !strings.Contains(resp.Diagnostics.Errors()[0].Detail(), tc.wantDetail) {
				t.Fatalf("delete error detail = %q, want one containing %q", resp.Diagnostics.Errors()[0].Detail(), tc.wantDetail)
			}
			if fmt.Sprint(f.paths()) != fmt.Sprint(tc.wantCalls) {
				t.Fatalf("calls = %v, want %v", f.paths(), tc.wantCalls)
			}
			if tc.patch == 200 {
				if got := fmt.Sprint(f.bodyOf("PATCH /v3/projects/" + tc.id)); got != "map[project:map[enabled:false]]" {
					t.Fatalf("disable body = %s, want only enabled=false", got)
				}
			}
		})
	}
}

// Disabling a project that acts as a domain disables the domain, so Update
// refuses it for a protected domain before any PATCH, as Delete refuses a
// destroy; disabling any other domain or project, and any other change to a
// protected domain, including one already disabled, is sent as before. When
// the token read fails, Update cannot tell whether the domain is protected, so
// it returns the error and sends nothing.
func TestProjectUpdateRefusesToDisableAProtectedDomain(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name            string
		id              string
		isDomain        bool
		rename          bool
		disabledInState bool
		tokenStatus     int
		wantCalls       []string
		wantError       string
	}{
		{name: "disable the provider's own domain", id: "dom-user", isDomain: true,
			wantCalls: []string{"GET /v3/auth/tokens"}, wantError: "Refusing to disable a protected domain"},
		{name: "disable the default domain", id: "default", isDomain: true,
			wantError: "Refusing to disable a protected domain"},
		{name: "disable another domain", id: "dom-other", isDomain: true,
			wantCalls: []string{"GET /v3/auth/tokens", "PATCH /v3/projects/dom-other"}},
		{name: "disable an ordinary project", id: "p-1", wantCalls: []string{"PATCH /v3/projects/p-1"}},
		{name: "rename the provider's own domain", id: "dom-user", isDomain: true, rename: true,
			wantCalls: []string{"PATCH /v3/projects/dom-user"}},
		{name: "rename the default domain, already disabled", id: "default", isDomain: true, rename: true,
			disabledInState: true, wantCalls: []string{"PATCH /v3/projects/default"}},
		{name: "disable a domain when the token read fails", id: "dom-user", isDomain: true, tokenStatus: 500,
			wantCalls: []string{"GET /v3/auth/tokens"}, wantError: "identity: checking which domain the provider authenticates into"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			token := keystoneReply(200, tokenBody)
			if tc.tokenStatus != 0 {
				token = keystoneReply(tc.tokenStatus, `{"error": {"message": "boom"}}`)
			}
			f := newKeystoneFake(t, map[string]func() (int, string){
				"GET /v3/auth/tokens": token,
				"PATCH /v3/projects/" + tc.id: keystoneReply(200, fmt.Sprintf(`{"project": {"id": %q, "name": "tf-proj", `+
					`"domain_id": "default", "parent_id": "default", "is_domain": %v, "enabled": %v, "tags": []}}`,
					tc.id, tc.isDomain, tc.rename && !tc.disabledInState)),
			})
			r := &projectResource{config: fakeConfig(f.srv.URL)}
			st := projectStateFor(t, r, tc.id, tc.isDomain)
			var m projectModel
			if d := st.Get(ctx, &m); d.HasError() {
				t.Fatalf("reading the state: %v", d)
			}
			m.Enabled = types.BoolValue(tc.rename)
			if tc.disabledInState {
				m.Enabled = types.BoolValue(false)
				if d := st.Set(ctx, &m); d.HasError() {
					t.Fatalf("building the disabled state: %v", d)
				}
			}
			if tc.rename {
				m.Name = types.StringValue("tf-renamed")
			}
			pl := tfsdk.Plan{Schema: st.Schema, Raw: tftypes.NewValue(st.Schema.Type().TerraformType(ctx), nil)}
			if d := pl.Set(ctx, &m); d.HasError() {
				t.Fatalf("building the plan: %v", d)
			}
			resp := resource.UpdateResponse{State: st}
			r.Update(ctx, resource.UpdateRequest{State: st, Plan: pl}, &resp)
			var summaries []string
			for _, d := range resp.Diagnostics.Errors() {
				summaries = append(summaries, d.Summary())
			}
			if tc.wantError == "" && len(summaries) != 0 {
				t.Fatalf("update errors = %v, want none", summaries)
			}
			if tc.wantError != "" && (len(summaries) != 1 || summaries[0] != tc.wantError) {
				t.Fatalf("update errors = %v, want [%s]", summaries, tc.wantError)
			}
			if fmt.Sprint(f.paths()) != fmt.Sprint(tc.wantCalls) {
				t.Fatalf("calls = %v, want %v", f.paths(), tc.wantCalls)
			}
		})
	}
}
