// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package identity

import (
	"context"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/platform9/terraform-provider-pcd/internal/clients"
)

func remoteIDSet(t *testing.T, ids ...string) types.Set {
	t.Helper()
	set, d := types.SetValueFrom(context.Background(), types.StringType, ids)
	if d.HasError() {
		t.Fatalf("building remote_ids: %v", d)
	}
	return set
}

func idpModel(t *testing.T, description types.String, enabled bool, remoteIDs ...string) federationIdentityProviderModel {
	return federationIdentityProviderModel{
		ID:          types.StringValue("tf-idp"),
		Name:        types.StringValue("tf-idp"),
		DomainID:    types.StringValue("default"),
		RemoteIDs:   remoteIDSet(t, remoteIDs...),
		Enabled:     types.BoolValue(enabled),
		Description: description,
	}
}

const idpBody = `{"identity_provider": {"id": "tf-idp", "domain_id": "default", "description": null, "enabled": true,
	"remote_ids": ["https://idp.example.com/entity"], "authorization_ttl": null, "links": {"self": "x"}}}`

// Keystone defaults enabled to false, which would leave a new identity
// provider unusable, so the create body always carries it; description goes
// only when set.
func TestFederationIdentityProviderCreateBody(t *testing.T) {
	ctx := context.Background()
	var diags diag.Diagnostics
	m := idpModel(t, types.StringUnknown(), true, "https://idp.example.com/entity")
	got := federationIdentityProviderCreateBody(ctx, &m, &diags)
	want := map[string]any{"identity_provider": map[string]any{
		"domain_id": "default", "remote_ids": []string{"https://idp.example.com/entity"}, "enabled": true,
	}}
	if diags.HasError() || !reflect.DeepEqual(got, want) {
		t.Fatalf("body = %v (diags %v)\nwant  %v", got, diags, want)
	}
	m.Description = types.StringValue("Okta")
	if got := federationIdentityProviderCreateBody(ctx, &m, &diags); got["identity_provider"].(map[string]any)["description"] != "Okta" {
		t.Fatalf("body = %v, want the description sent", got)
	}
}

// Keystone refuses an empty PATCH and cannot change domain_id, so the update
// body carries exactly the fields that changed, and is nil when none did.
func TestFederationIdentityProviderUpdateBody(t *testing.T) {
	ctx := context.Background()
	state := idpModel(t, types.StringValue(""), true, "https://a.example.com")
	for _, tc := range []struct {
		name string
		plan federationIdentityProviderModel
		want map[string]any
	}{
		{"unchanged", idpModel(t, types.StringValue(""), true, "https://a.example.com"), nil},
		{"remote_ids", idpModel(t, types.StringValue(""), true, "https://b.example.com"),
			map[string]any{"remote_ids": []string{"https://b.example.com"}}},
		{"disabled", idpModel(t, types.StringValue(""), false, "https://a.example.com"),
			map[string]any{"enabled": false}},
		{"description", idpModel(t, types.StringValue("Okta"), true, "https://a.example.com"),
			map[string]any{"description": "Okta"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var diags diag.Diagnostics
			got := federationIdentityProviderUpdateBody(ctx, &tc.plan, &state, &diags)
			if diags.HasError() {
				t.Fatal(diags)
			}
			if tc.want == nil {
				if got != nil {
					t.Fatalf("body = %v, want no call", got)
				}
				return
			}
			if !reflect.DeepEqual(got["identity_provider"], tc.want) {
				t.Fatalf("body = %v, want identity_provider %v", got, tc.want)
			}
		})
	}
}

func TestFederationIdentityProviderLifecycle(t *testing.T) {
	ctx := context.Background()
	updated := strings.Replace(idpBody, `"enabled": true`, `"enabled": false`, 1)
	f := newFederationFake(t, map[string][]federationReply{
		"PUT /v3/OS-FEDERATION/identity_providers/tf-idp":    {{http.StatusCreated, idpBody}},
		"GET /v3/OS-FEDERATION/identity_providers/tf-idp":    {{http.StatusOK, idpBody}},
		"PATCH /v3/OS-FEDERATION/identity_providers/tf-idp":  {{http.StatusOK, updated}},
		"DELETE /v3/OS-FEDERATION/identity_providers/tf-idp": {{http.StatusNoContent, ``}},
	})
	r := &federationIdentityProviderResource{config: f.config()}
	s := federationSchemaOf(t, r)

	planned := idpModel(t, types.StringUnknown(), true, "https://idp.example.com/entity")
	planned.ID = types.StringUnknown()
	createResp := resource.CreateResponse{State: tfsdk.State{Schema: s, Raw: federationNull(ctx, s)}}
	r.Create(ctx, resource.CreateRequest{Plan: federationPlan(ctx, t, s, &planned)}, &createResp)
	if createResp.Diagnostics.HasError() {
		t.Fatalf("create: %v", createResp.Diagnostics)
	}
	wantPut := map[string]any{"identity_provider": map[string]any{
		"domain_id": "default", "remote_ids": []any{"https://idp.example.com/entity"}, "enabled": true,
	}}
	if got := f.bodyOf("PUT /v3/OS-FEDERATION/identity_providers/tf-idp"); !reflect.DeepEqual(got, wantPut) {
		t.Fatalf("PUT body = %v\nwant       %v", got, wantPut)
	}
	var created federationIdentityProviderModel
	createResp.State.Get(ctx, &created)
	// Keystone answers description null; state must hold a known value.
	if created.ID.ValueString() != "tf-idp" || created.Description.IsNull() || created.Description.ValueString() != "" {
		t.Fatalf("created id=%s description=%v", created.ID, created.Description)
	}

	readResp := resource.ReadResponse{State: createResp.State}
	r.Read(ctx, resource.ReadRequest{State: createResp.State}, &readResp)
	if readResp.Diagnostics.HasError() {
		t.Fatalf("read: %v", readResp.Diagnostics)
	}
	var read federationIdentityProviderModel
	readResp.State.Get(ctx, &read)
	wantRead := idpModel(t, types.StringValue(""), true, "https://idp.example.com/entity")
	if !reflect.DeepEqual(read, wantRead) {
		t.Fatalf("read state = %+v\nwant         %+v", read, wantRead)
	}

	disabled := read
	disabled.Enabled = types.BoolValue(false)
	updateResp := resource.UpdateResponse{State: readResp.State}
	r.Update(ctx, resource.UpdateRequest{Plan: federationPlan(ctx, t, s, &disabled), State: readResp.State}, &updateResp)
	if updateResp.Diagnostics.HasError() {
		t.Fatalf("update: %v", updateResp.Diagnostics)
	}
	if got := f.bodyOf("PATCH /v3/OS-FEDERATION/identity_providers/tf-idp"); !reflect.DeepEqual(got, map[string]any{"identity_provider": map[string]any{"enabled": false}}) {
		t.Fatalf("PATCH body = %v, want only enabled", got)
	}
	var afterUpdate federationIdentityProviderModel
	updateResp.State.Get(ctx, &afterUpdate)
	if !reflect.DeepEqual(afterUpdate, disabled) {
		t.Fatalf("state after update = %+v\nwant                 %+v", afterUpdate, disabled)
	}

	deleteResp := resource.DeleteResponse{State: updateResp.State}
	r.Delete(ctx, resource.DeleteRequest{State: updateResp.State}, &deleteResp)
	if deleteResp.Diagnostics.HasError() {
		t.Fatalf("delete: %v", deleteResp.Diagnostics)
	}
	want := []string{
		"PUT /v3/OS-FEDERATION/identity_providers/tf-idp", "GET /v3/OS-FEDERATION/identity_providers/tf-idp",
		"PATCH /v3/OS-FEDERATION/identity_providers/tf-idp", "DELETE /v3/OS-FEDERATION/identity_providers/tf-idp",
	}
	if got := f.keys(); !reflect.DeepEqual(got, want) {
		t.Fatalf("requests = %v\nwant       %v", got, want)
	}
}

// IDP1 made in the PCD UI already exists, or an entity ID can belong to only
// one identity provider; both answer 409, and the user needs the way out of
// either.
func TestFederationIdentityProviderCreateNamesTheWayOut(t *testing.T) {
	ctx := context.Background()
	f := newFederationFake(t, map[string][]federationReply{
		"PUT /v3/OS-FEDERATION/identity_providers/tf-idp": {{http.StatusConflict, `{"error": {"code": 409}}`}},
	})
	r := &federationIdentityProviderResource{config: f.config()}
	s := federationSchemaOf(t, r)
	planned := idpModel(t, types.StringUnknown(), true, "https://idp.example.com/entity")
	resp := resource.CreateResponse{State: tfsdk.State{Schema: s, Raw: federationNull(ctx, s)}}
	r.Create(ctx, resource.CreateRequest{Plan: federationPlan(ctx, t, s, &planned)}, &resp)
	if !resp.Diagnostics.HasError() {
		t.Fatal("create: no error for a 409")
	}
	detail := resp.Diagnostics[0].Detail()
	for _, want := range []string{"terraform import <resource address> tf-idp", "remove the entity ID from the other identity provider"} {
		if !strings.Contains(detail, want) {
			t.Errorf("detail %q does not contain %q", detail, want)
		}
	}
	if !resp.State.Raw.IsNull() {
		t.Error("state was written for a create that failed")
	}
}

func TestFederationIdentityProviderReadAndDeleteWhenGone(t *testing.T) {
	ctx := context.Background()
	f := newFederationFake(t, map[string][]federationReply{
		"GET /v3/OS-FEDERATION/identity_providers/tf-idp":    {{http.StatusNotFound, `{"error": {"code": 404}}`}},
		"DELETE /v3/OS-FEDERATION/identity_providers/tf-idp": {{http.StatusNotFound, `{"error": {"code": 404}}`}},
	})
	r := &federationIdentityProviderResource{config: f.config()}
	s := federationSchemaOf(t, r)
	prior := idpModel(t, types.StringValue(""), true, "https://idp.example.com/entity")
	readResp := resource.ReadResponse{State: federationState(ctx, t, s, &prior)}
	r.Read(ctx, resource.ReadRequest{State: federationState(ctx, t, s, &prior)}, &readResp)
	if readResp.Diagnostics.HasError() || !readResp.State.Raw.IsNull() {
		t.Fatalf("read: diags %v, null %v", readResp.Diagnostics, readResp.State.Raw.IsNull())
	}
	deleteResp := resource.DeleteResponse{State: federationState(ctx, t, s, &prior)}
	r.Delete(ctx, resource.DeleteRequest{State: federationState(ctx, t, s, &prior)}, &deleteResp)
	if deleteResp.Diagnostics.HasError() {
		t.Fatalf("delete: %v", deleteResp.Diagnostics)
	}
}

// federationIdentityProviderNoObjectAnswers are 2xx answers that hold no
// identity provider. Decoded as they are, they would read as domain_id "",
// which plans a replacement, and destroying an identity provider also deletes
// its protocols.
var federationIdentityProviderNoObjectAnswers = map[string]string{
	"empty body":         `{}`,
	"null object":        `{"identity_provider": null}`,
	"object without ID":  `{"identity_provider": {"domain_id": "default", "enabled": true}}`,
	"empty-string ID":    `{"identity_provider": {"id": "", "domain_id": "default", "enabled": true}}`,
	"unrelated key only": `{"links": {"self": "x"}}`,
}

func federationExpectNoObjectError(t *testing.T, diags diag.Diagnostics) {
	t.Helper()
	if !diags.HasError() || !strings.Contains(diags.Errors()[0].Detail(), clients.ErrNoObject.Error()) {
		t.Fatalf("diagnostics = %v, want %q", diags, clients.ErrNoObject)
	}
}

func TestFederationIdentityProviderCreateRefusesAnAnswerWithoutTheObject(t *testing.T) {
	for name, body := range federationIdentityProviderNoObjectAnswers {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			f := newFederationFake(t, map[string][]federationReply{
				"PUT /v3/OS-FEDERATION/identity_providers/tf-idp": {{http.StatusCreated, body}},
			})
			r := &federationIdentityProviderResource{config: f.config()}
			s := federationSchemaOf(t, r)
			planned := idpModel(t, types.StringUnknown(), true, "https://idp.example.com/entity")
			planned.ID = types.StringUnknown()
			resp := resource.CreateResponse{State: tfsdk.State{Schema: s, Raw: federationNull(ctx, s)}}
			r.Create(ctx, resource.CreateRequest{Plan: federationPlan(ctx, t, s, &planned)}, &resp)
			federationExpectNoObjectError(t, resp.Diagnostics)
			if !resp.State.Raw.IsNull() {
				t.Fatal("state was written from an answer without the object")
			}
		})
	}
}

func TestFederationIdentityProviderReadKeepsStateOnAnAnswerWithoutTheObject(t *testing.T) {
	for name, body := range federationIdentityProviderNoObjectAnswers {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			f := newFederationFake(t, map[string][]federationReply{
				"GET /v3/OS-FEDERATION/identity_providers/tf-idp": {{http.StatusOK, body}},
			})
			r := &federationIdentityProviderResource{config: f.config()}
			s := federationSchemaOf(t, r)
			prior := idpModel(t, types.StringValue("Okta"), true, "https://idp.example.com/entity")
			readResp := resource.ReadResponse{State: federationState(ctx, t, s, &prior)}
			r.Read(ctx, resource.ReadRequest{State: federationState(ctx, t, s, &prior)}, &readResp)
			federationExpectNoObjectError(t, readResp.Diagnostics)
			var got federationIdentityProviderModel
			readResp.State.Get(ctx, &got)
			if !reflect.DeepEqual(got, prior) {
				t.Fatalf("state = %+v\nwant the prior state %+v", got, prior)
			}
		})
	}
}

func TestFederationIdentityProviderUpdateRefusesAnAnswerWithoutTheObject(t *testing.T) {
	for name, body := range federationIdentityProviderNoObjectAnswers {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			f := newFederationFake(t, map[string][]federationReply{
				"PATCH /v3/OS-FEDERATION/identity_providers/tf-idp": {{http.StatusOK, body}},
			})
			r := &federationIdentityProviderResource{config: f.config()}
			s := federationSchemaOf(t, r)
			prior := idpModel(t, types.StringValue(""), true, "https://idp.example.com/entity")
			plan := idpModel(t, types.StringValue(""), false, "https://idp.example.com/entity")
			resp := resource.UpdateResponse{State: federationState(ctx, t, s, &prior)}
			r.Update(ctx, resource.UpdateRequest{Plan: federationPlan(ctx, t, s, &plan), State: federationState(ctx, t, s, &prior)}, &resp)
			federationExpectNoObjectError(t, resp.Diagnostics)
			var got federationIdentityProviderModel
			resp.State.Get(ctx, &got)
			if !reflect.DeepEqual(got, prior) {
				t.Fatalf("state = %+v\nwant the prior state %+v", got, prior)
			}
		})
	}
}

func TestFederationIdentityProviderImportState(t *testing.T) {
	ctx := context.Background()
	s := federationSchemaOf(t, &federationIdentityProviderResource{})
	for _, tc := range []struct {
		name, id, wantError string
	}{
		{name: "empty", id: "", wantError: "Import needs a federation identity provider ID"},
		{name: "slash", id: "a/b", wantError: "Import needs a federation identity provider ID"},
		{name: "space", id: "a b", wantError: "Import needs a federation identity provider ID"},
		{name: "relative path", id: "../x", wantError: "Import needs a federation identity provider ID"},
		{name: "identity provider name", id: "IDP1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := resource.ImportStateResponse{State: tfsdk.State{Schema: s, Raw: federationNull(ctx, s)}}
			(&federationIdentityProviderResource{}).ImportState(ctx, resource.ImportStateRequest{ID: tc.id}, &resp)
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
			var got federationIdentityProviderModel
			resp.State.Get(ctx, &got)
			if len(summaries) != 0 || got.ID.ValueString() != tc.id || got.Name.ValueString() != tc.id {
				t.Fatalf("import: errors %v, id %s, name %s", summaries, got.ID, got.Name)
			}
		})
	}
}

// Read copies Keystone's answer into state: drift made outside Terraform shows
// in the next plan, and an import, whose state holds only the name, gets every
// attribute filled (a null domain_id would plan a replacement, which deletes
// the identity provider's protocols).
func TestFederationIdentityProviderReadTakesKeystonesValues(t *testing.T) {
	const answer = `{"identity_provider": {"id": "tf-idp", "domain_id": "other-domain", "description": "edited in the UI",
		"enabled": false, "remote_ids": ["https://other.example.com/entity"], "links": {"self": "x"}}}`
	imported := federationIdentityProviderModel{
		ID: types.StringValue("tf-idp"), Name: types.StringValue("tf-idp"), DomainID: types.StringNull(),
		RemoteIDs: types.SetNull(types.StringType), Enabled: types.BoolNull(), Description: types.StringNull(),
	}
	for name, prior := range map[string]federationIdentityProviderModel{
		"drift":  idpModel(t, types.StringValue(""), true, "https://idp.example.com/entity"),
		"import": imported,
	} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			f := newFederationFake(t, map[string][]federationReply{
				"GET /v3/OS-FEDERATION/identity_providers/tf-idp": {{http.StatusOK, answer}},
			})
			r := &federationIdentityProviderResource{config: f.config()}
			s := federationSchemaOf(t, r)
			resp := resource.ReadResponse{State: federationState(ctx, t, s, &prior)}
			r.Read(ctx, resource.ReadRequest{State: federationState(ctx, t, s, &prior)}, &resp)
			if resp.Diagnostics.HasError() {
				t.Fatalf("read: %v", resp.Diagnostics)
			}
			var got federationIdentityProviderModel
			resp.State.Get(ctx, &got)
			federationExpectAttrs(t, map[string][2]attr.Value{
				"id":          {got.ID, types.StringValue("tf-idp")},
				"name":        {got.Name, types.StringValue("tf-idp")},
				"domain_id":   {got.DomainID, types.StringValue("other-domain")},
				"remote_ids":  {got.RemoteIDs, remoteIDSet(t, "https://other.example.com/entity")},
				"enabled":     {got.Enabled, types.BoolValue(false)},
				"description": {got.Description, types.StringValue("edited in the UI")},
			})
		})
	}
}

// An answer for another identity provider is refused, naming both, whether it
// comes back from a create or a read; state is not written from it.
func TestFederationIdentityProviderRefusesAnAnswerForAnotherIdentityProvider(t *testing.T) {
	const other = `{"identity_provider": {"id": "IDP1", "domain_id": "default", "enabled": true, "remote_ids": []}}`
	expect := func(t *testing.T, diags diag.Diagnostics) {
		t.Helper()
		if !diags.HasError() {
			t.Fatal("no error for an answer about another identity provider")
		}
		for _, want := range []string{`"tf-idp"`, `"IDP1"`} {
			if !strings.Contains(diags.Errors()[0].Detail(), want) {
				t.Errorf("detail %q does not name %s", diags.Errors()[0].Detail(), want)
			}
		}
	}
	t.Run("create", func(t *testing.T) {
		ctx := context.Background()
		f := newFederationFake(t, map[string][]federationReply{
			"PUT /v3/OS-FEDERATION/identity_providers/tf-idp": {{http.StatusCreated, other}},
		})
		r := &federationIdentityProviderResource{config: f.config()}
		s := federationSchemaOf(t, r)
		planned := idpModel(t, types.StringUnknown(), true, "https://idp.example.com/entity")
		planned.ID = types.StringUnknown()
		resp := resource.CreateResponse{State: tfsdk.State{Schema: s, Raw: federationNull(ctx, s)}}
		r.Create(ctx, resource.CreateRequest{Plan: federationPlan(ctx, t, s, &planned)}, &resp)
		expect(t, resp.Diagnostics)
		if !resp.State.Raw.IsNull() {
			t.Fatal("state was written from an answer about another identity provider")
		}
	})
	t.Run("read", func(t *testing.T) {
		ctx := context.Background()
		f := newFederationFake(t, map[string][]federationReply{
			"GET /v3/OS-FEDERATION/identity_providers/tf-idp": {{http.StatusOK, other}},
		})
		r := &federationIdentityProviderResource{config: f.config()}
		s := federationSchemaOf(t, r)
		prior := idpModel(t, types.StringValue(""), true, "https://idp.example.com/entity")
		resp := resource.ReadResponse{State: federationState(ctx, t, s, &prior)}
		r.Read(ctx, resource.ReadRequest{State: federationState(ctx, t, s, &prior)}, &resp)
		expect(t, resp.Diagnostics)
		var got federationIdentityProviderModel
		resp.State.Get(ctx, &got)
		if !reflect.DeepEqual(got, prior) {
			t.Fatalf("state = %+v\nwant the prior state %+v", got, prior)
		}
	})
}
