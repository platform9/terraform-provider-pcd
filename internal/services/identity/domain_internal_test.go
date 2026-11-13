// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package identity

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/gophercloud/gophercloud/v2/openstack/identity/v3/domains"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

const domainBody = `{"domain": {"id": "dom-1", "name": "tf-dom", "description": "d", "enabled": true, "links": {}}}`

func domainModelFor(name, description string, enabled bool) *domainModel {
	return &domainModel{
		ID:          types.StringValue("dom-1"),
		Name:        types.StringValue(name),
		Description: types.StringValue(description),
		Enabled:     types.BoolValue(enabled),
		Region:      types.StringValue("region-one"),
	}
}

// An update sends name, description and enabled together whenever one of them
// changed, so the domain ends up exactly as planned; with none of them changed
// it sends nothing, because Keystone rejects an empty PATCH (a changed region
// replaces the domain instead).
func TestDomainUpdateOpts(t *testing.T) {
	state := domainModelFor("a", "first", true)
	for _, tc := range []struct {
		name     string
		plan     *domainModel
		wantSend bool
		wantBody map[string]any
	}{
		{name: "unchanged", plan: domainModelFor("a", "first", true)},
		{name: "region only", plan: func() *domainModel {
			m := domainModelFor("a", "first", true)
			m.Region = types.StringValue("region-two")
			return m
		}()},
		{name: "renamed", plan: domainModelFor("b", "first", true), wantSend: true,
			wantBody: map[string]any{"name": "b", "description": "first", "enabled": true}},
		{name: "description cleared", plan: domainModelFor("a", "", true), wantSend: true,
			wantBody: map[string]any{"name": "a", "description": "", "enabled": true}},
		{name: "disabled", plan: domainModelFor("a", "first", false), wantSend: true,
			wantBody: map[string]any{"name": "a", "description": "first", "enabled": false}},
		{name: "unknown description is not sent", plan: func() *domainModel {
			m := domainModelFor("b", "first", true)
			m.Description = types.StringUnknown()
			return m
		}(), wantSend: true, wantBody: map[string]any{"name": "b", "enabled": true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts, send := domainUpdateOpts(tc.plan, state)
			if send != tc.wantSend {
				t.Fatalf("send = %v, want %v", send, tc.wantSend)
			}
			if !send {
				return
			}
			b, err := opts.ToDomainUpdateMap()
			if err != nil {
				t.Fatal(err)
			}
			got := b["domain"].(map[string]any)
			if fmt.Sprint(got) != fmt.Sprint(tc.wantBody) {
				t.Fatalf("PATCH body = %v, want %v", got, tc.wantBody)
			}
		})
	}
}

// Keystone refuses to delete an enabled domain, so delete always disables
// first; a 404 at either step means the domain is already gone, and a failed
// delete after a successful disable must not be reported as success.
func TestDeleteDomainDisablesThenDeletes(t *testing.T) {
	for _, tc := range []struct {
		name              string
		patch, del        int
		wantCalls         []string
		wantErr, wantLeft string
	}{
		{name: "deleted", patch: 200, del: 204, wantCalls: []string{"PATCH /domains/dom-1", "DELETE /domains/dom-1"}},
		{name: "already gone at disable", patch: 404, wantCalls: []string{"PATCH /domains/dom-1"}},
		{name: "gone between disable and delete", patch: 200, del: 404, wantCalls: []string{"PATCH /domains/dom-1", "DELETE /domains/dom-1"}},
		{name: "disable refused", patch: 403, wantCalls: []string{"PATCH /domains/dom-1"}, wantErr: "disabling domain dom-1"},
		{name: "delete refused", patch: 200, del: 403, wantCalls: []string{"PATCH /domains/dom-1", "DELETE /domains/dom-1"},
			wantErr: "stays disabled"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newKeystoneFake(t, map[string]func() (int, string){
				"PATCH /domains/dom-1": func() (int, string) {
					if tc.patch == 200 {
						return 200, `{"domain": {"id": "dom-1", "name": "tf-dom", "enabled": false}}`
					}
					return tc.patch, `{"error": {"message": "no"}}`
				},
				"DELETE /domains/dom-1": keystoneReply(tc.del, ""),
			})
			err := deleteDomain(context.Background(), f.bareClient(), "dom-1")
			if tc.wantErr == "" && err != nil {
				t.Fatalf("deleteDomain: %v", err)
			}
			if tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)) {
				t.Fatalf("deleteDomain error = %v, want one containing %q", err, tc.wantErr)
			}
			if got := f.paths(); fmt.Sprint(got) != fmt.Sprint(tc.wantCalls) {
				t.Fatalf("calls = %v, want %v", got, tc.wantCalls)
			}
			if tc.patch == 200 {
				if got := fmt.Sprint(f.bodyOf("PATCH /domains/dom-1")); got != "map[domain:map[enabled:false]]" {
					t.Fatalf("disable body = %s, want only enabled=false", got)
				}
			}
		})
	}
}

// The default domain and every domain the provider's token depends on are
// protected; any other domain is not.
func TestProtectedDomainReason(t *testing.T) {
	s := tokenScope{UserID: "u", UserDomainID: "dom-user", ProjectDomainID: "dom-project", DomainID: "dom-scope"}
	for id, want := range map[string]bool{
		"default": true, "dom-user": true, "dom-project": true, "dom-scope": true,
		"dom-other": false, "": false,
	} {
		if got := s.protectedDomainReason(id) != ""; got != want {
			t.Errorf("protectedDomainReason(%q) protected = %v, want %v", id, got, want)
		}
	}
}

// domainProtection reads the provider's own token for any domain other than
// default, and answers default without an API call.
func TestDomainProtectionReadsTheToken(t *testing.T) {
	f := newKeystoneFake(t, map[string]func() (int, string){"GET /auth/tokens": keystoneReply(200, tokenBody)})
	ctx := context.Background()
	for id, want := range map[string]bool{"dom-user": true, "dom-project": true, "dom-other": false} {
		reason, err := domainProtection(ctx, f.bareClient(), id)
		if err != nil {
			t.Fatalf("domainProtection(%s): %v", id, err)
		}
		if (reason != "") != want {
			t.Errorf("domainProtection(%s) = %q, protected want %v", id, reason, want)
		}
	}
	before := len(f.paths())
	if reason, err := domainProtection(ctx, f.bareClient(), "default"); err != nil || reason == "" {
		t.Fatalf("domainProtection(default) = %q, %v; want a reason", reason, err)
	}
	if len(f.paths()) != before {
		t.Fatalf("domainProtection(default) called the API: %v", f.paths())
	}
}

// A token whose project or domain section does not decode is an error, not a
// token without that scope: otherwise the domain it names would silently lose
// its protection.
func TestCurrentTokenScopeRefusesAMalformedToken(t *testing.T) {
	const user = `"user": {"id": "u-provider", "name": "admin", "domain": {"id": "dom-user", "name": "users"}}`
	for _, tc := range []struct{ name, id, body string }{
		{name: "project", id: "dom-project",
			body: `{"token": {"methods": ["password"], ` + user + `, "project": {"id": "p-1", "domain": "dom-project"}}}`},
		{name: "domain", id: "dom-scope",
			body: `{"token": {"methods": ["password"], ` + user + `, "domain": "dom-scope"}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newKeystoneFake(t, map[string]func() (int, string){"GET /auth/tokens": keystoneReply(200, tc.body)})
			ctx := context.Background()
			if s, err := currentTokenScope(ctx, f.bareClient()); err == nil {
				t.Fatalf("currentTokenScope = %+v; want an error", s)
			}
			if reason, err := domainProtection(ctx, f.bareClient(), tc.id); err == nil {
				t.Fatalf("domainProtection(%s) = %q with no error; want an error", tc.id, reason)
			}
		})
	}
}

// domainPlanFixture builds a state (and optionally a plan) for the domain
// resource's schema.
func domainPlanFixture(t *testing.T, r *domainResource, state *domainModel, plan *domainModel) (tfsdk.State, tfsdk.Plan) {
	t.Helper()
	ctx := context.Background()
	var sch resource.SchemaResponse
	r.Schema(ctx, resource.SchemaRequest{}, &sch)
	s := sch.Schema
	st := tfsdk.State{Schema: s, Raw: tftypes.NewValue(s.Type().TerraformType(ctx), nil)}
	if d := st.Set(ctx, state); d.HasError() {
		t.Fatalf("building the state: %v", d)
	}
	pl := tfsdk.Plan{Schema: s, Raw: tftypes.NewValue(s.Type().TerraformType(ctx), nil)}
	if plan != nil {
		if d := pl.Set(ctx, plan); d.HasError() {
			t.Fatalf("building the plan: %v", d)
		}
	}
	return st, pl
}

// providerUserInDefault is tokenBody with the provider's user in Keystone's
// default domain.
var providerUserInDefault = strings.Replace(tokenBody,
	`"domain": {"id": "dom-user", "name": "users"}`, `"domain": {"id": "default", "name": "Default"}`, 1)

// A plan that destroys, replaces or disables the domain the provider
// authenticates into must fail at plan time, before anything is sent; other
// plans pass. A changed region replaces the domain, so it is refused as a destroy.
func TestDomainModifyPlanRefusesProtectedDomains(t *testing.T) {
	ctx := context.Background()
	inRegionTwo := func() *domainModel {
		m := domainModelFor("a", "first", true)
		m.Region = types.StringValue("region-two")
		return m
	}
	unknownRegion := func() *domainModel {
		m := domainModelFor("a", "first", true)
		m.Region = types.StringUnknown()
		return m
	}
	for _, tc := range []struct {
		name       string
		id         string
		plan       *domainModel // nil means destroy
		token      string       // the provider's token; tokenBody when empty
		wantError  string
		wantDetail string
	}{
		{name: "destroy the user's domain", id: "dom-user", wantError: "Refusing to destroy a protected domain",
			wantDetail: "so the change would cut off the provider's own access"},
		{name: "destroy default", id: "default", wantError: "Refusing to destroy a protected domain",
			wantDetail: "which holds the PCD administrator, so the change would lock out the PCD administrator"},
		{name: "destroy default, the provider user's domain", id: "default", token: providerUserInDefault,
			wantError:  "Refusing to destroy a protected domain",
			wantDetail: "domain default: the user the provider authenticates as belongs to it, so the change would cut off the provider's own access"},
		{name: "disable the project's domain", id: "dom-project",
			plan: domainModelFor("a", "first", false), wantError: "Refusing to disable a protected domain"},
		{name: "destroy another domain", id: "dom-other"},
		{name: "rename the user's domain", id: "dom-user", plan: domainModelFor("b", "first", true)},
		{name: "move the user's domain to another region", id: "dom-user", plan: inRegionTwo(),
			wantError:  "Refusing to destroy a protected domain",
			wantDetail: "Changing `region` replaces the domain; set `region` back to \"region-one\" to keep managing it."},
		{name: "protected domain with a region unknown at plan time", id: "dom-user", plan: unknownRegion()},
		{name: "move another domain to another region", id: "dom-other", plan: inRegionTwo()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			token := tokenBody
			if tc.token != "" {
				token = tc.token
			}
			f := newKeystoneFake(t, map[string]func() (int, string){"GET /v3/auth/tokens": keystoneReply(200, token)})
			r := &domainResource{config: fakeConfig(f.srv.URL)}
			state := domainModelFor("a", "first", true)
			state.ID = types.StringValue(tc.id)
			if tc.plan != nil {
				tc.plan.ID = types.StringValue(tc.id)
			}
			st, pl := domainPlanFixture(t, r, state, tc.plan)
			resp := resource.ModifyPlanResponse{Plan: pl}
			r.ModifyPlan(ctx, resource.ModifyPlanRequest{State: st, Plan: pl}, &resp)
			var summaries []string
			for _, d := range resp.Diagnostics.Errors() {
				summaries = append(summaries, d.Summary())
			}
			if tc.wantError == "" && len(resp.Diagnostics) != 0 {
				t.Fatalf("diagnostics = %v, want none", resp.Diagnostics)
			}
			if tc.wantError != "" && (len(summaries) != 1 || summaries[0] != tc.wantError) {
				t.Fatalf("errors = %v, want [%s]", summaries, tc.wantError)
			}
			if tc.wantDetail != "" && !strings.Contains(resp.Diagnostics.Errors()[0].Detail(), tc.wantDetail) {
				t.Fatalf("detail = %q, want it to contain %q", resp.Diagnostics.Errors()[0].Detail(), tc.wantDetail)
			}
		})
	}
}

// Update and Delete check the guard again at apply time, for plans made while
// the provider was not configured and for the destroy half of a replacement (a
// changed region): a protected domain is refused before any PATCH or DELETE.
// Reading the token only words the refusal of default, so a failed read still
// refuses it, with the administrator's reason.
func TestDomainApplyRefusesProtectedDomains(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name, id, wantError string
		plan                *domainModel // nil means Delete
		tokenStatus         int          // the token GET's status; 200 when zero
		wantDetail          string
	}{
		{name: "destroy the user's domain", id: "dom-user", wantError: "Refusing to destroy a protected domain"},
		{name: "destroy default", id: "default", wantError: "Refusing to destroy a protected domain"},
		{name: "destroy default when the token read fails", id: "default", tokenStatus: 500,
			wantError:  "Refusing to destroy a protected domain",
			wantDetail: "which holds the PCD administrator, so the change would lock out the PCD administrator"},
		{name: "disable the project's domain", id: "dom-project", plan: domainModelFor("b", "first", false),
			wantError: "Refusing to disable a protected domain"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status, body := 200, tokenBody
			if tc.tokenStatus != 0 {
				status, body = tc.tokenStatus, `{"error": {"code": 500, "message": "boom"}}`
			}
			f := newKeystoneFake(t, map[string]func() (int, string){"GET /v3/auth/tokens": keystoneReply(status, body)})
			r := &domainResource{config: fakeConfig(f.srv.URL)}
			state := domainModelFor("a", "first", true)
			state.ID = types.StringValue(tc.id)
			if tc.plan != nil {
				tc.plan.ID = types.StringValue(tc.id)
			}
			st, pl := domainPlanFixture(t, r, state, tc.plan)
			var diags diag.Diagnostics
			if tc.plan == nil {
				resp := resource.DeleteResponse{State: st}
				r.Delete(ctx, resource.DeleteRequest{State: st}, &resp)
				diags = resp.Diagnostics
			} else {
				resp := resource.UpdateResponse{State: tfsdk.State{Schema: st.Schema, Raw: tftypes.NewValue(st.Schema.Type().TerraformType(ctx), nil)}}
				r.Update(ctx, resource.UpdateRequest{State: st, Plan: pl}, &resp)
				diags = resp.Diagnostics
			}
			var summaries []string
			for _, d := range diags.Errors() {
				summaries = append(summaries, d.Summary())
			}
			if len(summaries) != 1 || summaries[0] != tc.wantError {
				t.Fatalf("errors = %v, want [%s]", summaries, tc.wantError)
			}
			if tc.wantDetail != "" && !strings.Contains(diags.Errors()[0].Detail(), tc.wantDetail) {
				t.Fatalf("detail = %q, want it to contain %q", diags.Errors()[0].Detail(), tc.wantDetail)
			}
			for _, p := range f.paths() {
				if p != "GET /v3/auth/tokens" {
					t.Fatalf("calls = %v; want no PATCH or DELETE", f.paths())
				}
			}
		})
	}
}

// Create, refresh and destroy against a fake Keystone: destroy checks the
// token, disables the domain and then deletes it.
func TestDomainResourceLifecycle(t *testing.T) {
	ctx := context.Background()
	deleted := false
	f := newKeystoneFake(t, map[string]func() (int, string){
		"POST /v3/domains": keystoneReply(201, domainBody),
		"GET /v3/domains/dom-1": func() (int, string) {
			if deleted {
				return 404, `{"error": {"message": "not found"}}`
			}
			return 200, domainBody
		},
		"GET /v3/auth/tokens":     keystoneReply(200, tokenBody),
		"PATCH /v3/domains/dom-1": keystoneReply(200, `{"domain": {"id": "dom-1", "name": "tf-dom", "enabled": false}}`),
		"DELETE /v3/domains/dom-1": func() (int, string) {
			deleted = true
			return 204, ""
		},
	})
	r := &domainResource{config: fakeConfig(f.srv.URL)}
	var sch resource.SchemaResponse
	r.Schema(ctx, resource.SchemaRequest{}, &sch)
	s := sch.Schema

	plan := tfsdk.Plan{Schema: s, Raw: tftypes.NewValue(s.Type().TerraformType(ctx), nil)}
	if d := plan.Set(ctx, &domainModel{
		ID:          types.StringUnknown(),
		Name:        types.StringValue("tf-dom"),
		Description: types.StringValue("d"),
		Enabled:     types.BoolValue(true),
		Region:      types.StringUnknown(),
	}); d.HasError() {
		t.Fatalf("building the plan: %v", d)
	}
	createResp := resource.CreateResponse{State: tfsdk.State{Schema: s, Raw: tftypes.NewValue(s.Type().TerraformType(ctx), nil)}}
	r.Create(ctx, resource.CreateRequest{Plan: plan}, &createResp)
	if createResp.Diagnostics.HasError() {
		t.Fatalf("create: %v", createResp.Diagnostics)
	}
	if !createResp.State.Raw.IsFullyKnown() {
		t.Fatalf("create state holds unknown values: %v", createResp.State.Raw)
	}
	if got := fmt.Sprint(f.bodyOf("POST /v3/domains")); got != "map[domain:map[description:d enabled:true name:tf-dom]]" {
		t.Fatalf("create body = %s", got)
	}
	var got domainModel
	createResp.State.Get(ctx, &got)
	if got.ID.ValueString() != "dom-1" || got.Region.ValueString() != "region-one" {
		t.Fatalf("create state id=%s region=%s; want dom-1, region-one", got.ID, got.Region)
	}

	readResp := resource.ReadResponse{State: createResp.State}
	r.Read(ctx, resource.ReadRequest{State: createResp.State}, &readResp)
	if readResp.Diagnostics.HasError() {
		t.Fatalf("read: %v", readResp.Diagnostics)
	}

	deleteResp := resource.DeleteResponse{State: readResp.State}
	r.Delete(ctx, resource.DeleteRequest{State: readResp.State}, &deleteResp)
	if deleteResp.Diagnostics.HasError() {
		t.Fatalf("delete: %v", deleteResp.Diagnostics)
	}
	want := []string{"POST /v3/domains", "GET /v3/domains/dom-1", "GET /v3/auth/tokens",
		"PATCH /v3/domains/dom-1", "DELETE /v3/domains/dom-1"}
	if fmt.Sprint(f.paths()) != fmt.Sprint(want) {
		t.Fatalf("calls = %v, want %v", f.paths(), want)
	}

	// The next refresh finds the domain gone and drops it from state.
	goneResp := resource.ReadResponse{State: readResp.State}
	r.Read(ctx, resource.ReadRequest{State: readResp.State}, &goneResp)
	if goneResp.Diagnostics.HasError() || !goneResp.State.Raw.IsNull() {
		t.Fatalf("read after delete: diags %v, state null %v; want the resource removed", goneResp.Diagnostics, goneResp.State.Raw.IsNull())
	}
}

// A 2xx answer without the domain object decodes to nil. Create and Read report
// it as an error instead of crashing on it, and Read keeps the domain in state
// instead of treating the answer as not-found.
func TestDomainRefusesAnAnswerWithoutTheDomain(t *testing.T) {
	ctx := context.Background()
	f := newKeystoneFake(t, map[string]func() (int, string){
		"POST /v3/domains":      keystoneReply(201, `{}`),
		"GET /v3/domains/dom-1": keystoneReply(200, `{}`),
	})
	r := &domainResource{config: fakeConfig(f.srv.URL)}
	var sch resource.SchemaResponse
	r.Schema(ctx, resource.SchemaRequest{}, &sch)
	s := sch.Schema

	plan := tfsdk.Plan{Schema: s, Raw: tftypes.NewValue(s.Type().TerraformType(ctx), nil)}
	if d := plan.Set(ctx, domainModelFor("tf-dom", "d", true)); d.HasError() {
		t.Fatalf("building the plan: %v", d)
	}
	createResp := resource.CreateResponse{State: tfsdk.State{Schema: s, Raw: tftypes.NewValue(s.Type().TerraformType(ctx), nil)}}
	r.Create(ctx, resource.CreateRequest{Plan: plan}, &createResp)
	if !createResp.Diagnostics.HasError() || !createResp.State.Raw.IsNull() {
		t.Fatalf("create: diags %v, state null %v; want an error and no state", createResp.Diagnostics, createResp.State.Raw.IsNull())
	}

	state := tfsdk.State{Schema: s, Raw: tftypes.NewValue(s.Type().TerraformType(ctx), nil)}
	if d := state.Set(ctx, domainModelFor("tf-dom", "d", true)); d.HasError() {
		t.Fatalf("building the state: %v", d)
	}
	readResp := resource.ReadResponse{State: state}
	r.Read(ctx, resource.ReadRequest{State: state}, &readResp)
	if !readResp.Diagnostics.HasError() || readResp.State.Raw.IsNull() {
		t.Fatalf("read: diags %v, state null %v; want an error and the domain kept", readResp.Diagnostics, readResp.State.Raw.IsNull())
	}
}

// An empty import ID names no domain, so import refuses it before it sets any
// attribute; any other ID is passed through as the domain ID.
func TestDomainImportRefusesAnEmptyID(t *testing.T) {
	ctx := context.Background()
	r := &domainResource{}
	var sch resource.SchemaResponse
	r.Schema(ctx, resource.SchemaRequest{}, &sch)
	s := sch.Schema
	for _, tc := range []struct {
		name, id, wantError string
	}{
		{name: "empty", id: "", wantError: "Import needs a domain ID"},
		{name: "domain ID", id: "dom-1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := resource.ImportStateResponse{State: tfsdk.State{Schema: s, Raw: tftypes.NewValue(s.Type().TerraformType(ctx), nil)}}
			r.ImportState(ctx, resource.ImportStateRequest{ID: tc.id}, &resp)
			var summaries []string
			for _, d := range resp.Diagnostics.Errors() {
				summaries = append(summaries, d.Summary())
			}
			if tc.wantError != "" {
				if len(summaries) != 1 || summaries[0] != tc.wantError || !resp.State.Raw.IsNull() {
					t.Fatalf("errors = %v, state null %v; want [%s] and no state", summaries, resp.State.Raw.IsNull(), tc.wantError)
				}
				return
			}
			if len(summaries) != 0 {
				t.Fatalf("errors = %v, want none", summaries)
			}
			var got domainModel
			resp.State.Get(ctx, &got)
			if got.ID.ValueString() != tc.id {
				t.Fatalf("imported id = %s, want %s", got.ID, tc.id)
			}
		})
	}
}

// The data source matches the exact name and says so when nothing matches.
func TestDomainByName(t *testing.T) {
	all := []domains.Domain{{ID: "d1", Name: "Default"}, {ID: "d2", Name: "tenant-a"}}
	if d, err := domainByName(all, "tenant-a"); err != nil || d.ID != "d2" {
		t.Fatalf("domainByName(tenant-a) = %v, %v; want d2", d, err)
	}
	if _, err := domainByName(all, "tenant"); err == nil || !strings.Contains(err.Error(), `"tenant"`) {
		t.Fatalf("domainByName(tenant) error = %v, want one naming the domain", err)
	}
	if _, err := domainByName(nil, "x"); err == nil {
		t.Fatal("domainByName on an empty list returned no error")
	}
}
