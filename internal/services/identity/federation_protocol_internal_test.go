// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package identity

import (
	"context"
	"net/http"
	"reflect"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

const protocolPath = "/v3/OS-FEDERATION/identity_providers/tf-idp/protocols/saml2"

func protocolModel(mapping string) federationProtocolModel {
	return federationProtocolModel{
		ID:               types.StringValue("tf-idp/saml2"),
		IdentityProvider: types.StringValue("tf-idp"),
		Name:             types.StringValue("saml2"),
		MappingID:        types.StringValue(mapping),
	}
}

func TestFederationProtocolLifecycle(t *testing.T) {
	ctx := context.Background()
	f := newFederationFake(t, map[string][]federationReply{
		"PUT " + protocolPath:    {{http.StatusCreated, `{"protocol": {"id": "saml2", "mapping_id": "tf-map", "links": {}}}`}},
		"GET " + protocolPath:    {{http.StatusOK, `{"protocol": {"id": "saml2", "mapping_id": "tf-map", "links": {}}}`}},
		"PATCH " + protocolPath:  {{http.StatusOK, `{"protocol": {"id": "saml2", "mapping_id": "tf-map-2", "links": {}}}`}},
		"DELETE " + protocolPath: {{http.StatusNoContent, ``}},
	})
	r := &federationProtocolResource{config: f.config()}
	s := federationSchemaOf(t, r)

	planned := protocolModel("tf-map")
	planned.ID = types.StringUnknown()
	createResp := resource.CreateResponse{State: tfsdk.State{Schema: s, Raw: federationNull(ctx, s)}}
	r.Create(ctx, resource.CreateRequest{Plan: federationPlan(ctx, t, s, &planned)}, &createResp)
	if createResp.Diagnostics.HasError() {
		t.Fatalf("create: %v", createResp.Diagnostics)
	}
	if got := f.bodyOf("PUT " + protocolPath); !reflect.DeepEqual(got, map[string]any{"protocol": map[string]any{"mapping_id": "tf-map"}}) {
		t.Fatalf("PUT body = %v", got)
	}
	var created federationProtocolModel
	createResp.State.Get(ctx, &created)
	if want := protocolModel("tf-map"); created != want {
		t.Fatalf("created state = %+v\nwant            %+v", created, want)
	}

	readResp := resource.ReadResponse{State: createResp.State}
	r.Read(ctx, resource.ReadRequest{State: createResp.State}, &readResp)
	if readResp.Diagnostics.HasError() {
		t.Fatalf("read: %v", readResp.Diagnostics)
	}
	var read federationProtocolModel
	readResp.State.Get(ctx, &read)
	if want := protocolModel("tf-map"); read != want {
		t.Fatalf("read state = %+v\nwant         %+v", read, want)
	}

	moved := protocolModel("tf-map-2")
	updateResp := resource.UpdateResponse{State: readResp.State}
	r.Update(ctx, resource.UpdateRequest{Plan: federationPlan(ctx, t, s, &moved), State: readResp.State}, &updateResp)
	if updateResp.Diagnostics.HasError() {
		t.Fatalf("update: %v", updateResp.Diagnostics)
	}
	if got := f.bodyOf("PATCH " + protocolPath); !reflect.DeepEqual(got, map[string]any{"protocol": map[string]any{"mapping_id": "tf-map-2"}}) {
		t.Fatalf("PATCH body = %v", got)
	}
	var afterUpdate federationProtocolModel
	updateResp.State.Get(ctx, &afterUpdate)
	if afterUpdate != moved {
		t.Fatalf("state after update = %+v\nwant                 %+v", afterUpdate, moved)
	}

	deleteResp := resource.DeleteResponse{State: updateResp.State}
	r.Delete(ctx, resource.DeleteRequest{State: updateResp.State}, &deleteResp)
	if deleteResp.Diagnostics.HasError() {
		t.Fatalf("delete: %v", deleteResp.Diagnostics)
	}
	want := []string{"PUT " + protocolPath, "GET " + protocolPath, "PATCH " + protocolPath, "DELETE " + protocolPath}
	if got := f.keys(); !reflect.DeepEqual(got, want) {
		t.Fatalf("requests = %v\nwant       %v", got, want)
	}
}

// Keystone deletes an identity provider's protocols with it, so in a destroy
// the protocol may already be gone when its own delete runs.
func TestFederationProtocolReadAndDeleteWhenGone(t *testing.T) {
	ctx := context.Background()
	f := newFederationFake(t, map[string][]federationReply{
		"GET " + protocolPath:    {{http.StatusNotFound, `{"error": {"code": 404}}`}},
		"DELETE " + protocolPath: {{http.StatusNotFound, `{"error": {"code": 404}}`}},
	})
	r := &federationProtocolResource{config: f.config()}
	s := federationSchemaOf(t, r)
	prior := protocolModel("tf-map")
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

// A 200 answer without the protocol would read as mapping_id "", which plans
// an update nobody asked for; Read refuses it and keeps the prior state.
func TestFederationProtocolReadKeepsStateOnAnAnswerWithoutTheObject(t *testing.T) {
	for name, body := range map[string]string{
		"empty body":              `{}`,
		"null object":             `{"protocol": null}`,
		"object without ID":       `{"protocol": {"mapping_id": "tf-map"}}`,
		"object without mapping":  `{"protocol": {"id": "saml2"}}`,
		"empty-string mapping ID": `{"protocol": {"id": "saml2", "mapping_id": ""}}`,
	} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			f := newFederationFake(t, map[string][]federationReply{
				"GET " + protocolPath: {{http.StatusOK, body}},
			})
			r := &federationProtocolResource{config: f.config()}
			s := federationSchemaOf(t, r)
			prior := protocolModel("tf-map")
			readResp := resource.ReadResponse{State: federationState(ctx, t, s, &prior)}
			r.Read(ctx, resource.ReadRequest{State: federationState(ctx, t, s, &prior)}, &readResp)
			federationExpectNoObjectError(t, readResp.Diagnostics)
			var got federationProtocolModel
			readResp.State.Get(ctx, &got)
			if got != prior {
				t.Fatalf("state = %+v\nwant the prior state %+v", got, prior)
			}
		})
	}
}

func TestFederationProtocolImportState(t *testing.T) {
	ctx := context.Background()
	s := federationSchemaOf(t, &federationProtocolResource{})
	const wantError = "Import needs a federation protocol ID"
	for _, tc := range []struct {
		name, id, wantError string
	}{
		{name: "empty", id: "", wantError: wantError},
		{name: "no protocol part", id: "IDP1", wantError: wantError},
		{name: "empty identity provider", id: "/saml2", wantError: wantError},
		{name: "empty protocol", id: "IDP1/", wantError: wantError},
		{name: "extra part", id: "IDP1/saml2/x", wantError: wantError},
		{name: "identity provider and protocol", id: "IDP1/saml2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := resource.ImportStateResponse{State: tfsdk.State{Schema: s, Raw: federationNull(ctx, s)}}
			(&federationProtocolResource{}).ImportState(ctx, resource.ImportStateRequest{ID: tc.id}, &resp)
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
			var got federationProtocolModel
			resp.State.Get(ctx, &got)
			if len(summaries) != 0 || got.ID.ValueString() != "IDP1/saml2" || got.IdentityProvider.ValueString() != "IDP1" || got.Name.ValueString() != "saml2" {
				t.Fatalf("import: errors %v, %s / %s / %s", summaries, got.ID, got.IdentityProvider, got.Name)
			}
		})
	}
}
