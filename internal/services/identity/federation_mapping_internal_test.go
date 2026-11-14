// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package identity

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"

	"github.com/platform9/terraform-provider-pcd/internal/clients"
)

// federationReply is one canned answer: a status code and a raw body.
type federationReply struct {
	status int
	body   string
}

// federationCall is one request the fake received: "METHOD /path" and the
// raw body.
type federationCall struct {
	key  string
	body []byte
}

// federationFake is an httptest server for Keystone (paths under /v3/) and
// hagrid (paths under /hagrid/). It answers each "METHOD /path" from a queue of
// canned replies, the last of which repeats, and records every request.
type federationFake struct {
	t      *testing.T
	srv    *httptest.Server
	mu     sync.Mutex
	routes map[string][]federationReply
	calls  []federationCall
}

func newFederationFake(t *testing.T, routes map[string][]federationReply) *federationFake {
	t.Helper()
	f := &federationFake{t: t, routes: routes}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *federationFake) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := r.Method + " " + r.URL.Path
	body, _ := io.ReadAll(r.Body)
	f.calls = append(f.calls, federationCall{key: key, body: body})
	queue, ok := f.routes[key]
	if !ok || len(queue) == 0 {
		f.t.Errorf("unexpected request %s", key)
		w.WriteHeader(http.StatusNotImplemented)
		return
	}
	reply := queue[0]
	if len(queue) > 1 {
		f.routes[key] = queue[1:]
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(reply.status)
	fmt.Fprint(w, reply.body)
}

// config returns a clients.Config whose catalog resolves every service type,
// in every region, to the fake: Keystone answers at /v3/ and hagrid, derived
// from the identity endpoint's origin, at /hagrid/. EndpointOverrides stays
// empty, because an override would drop the /v3/ that NewIdentityV3 adds.
// The provider client gets a transport of its own, not http.DefaultTransport:
// every httptest.Server.Close closes the default transport's idle
// connections, which can cut a request another parallel test is sending.
func (f *federationFake) config() *clients.Config {
	return &clients.Config{
		Region: "region-one",
		Provider: &gophercloud.ProviderClient{
			HTTPClient:      http.Client{Transport: &http.Transport{}},
			EndpointLocator: func(gophercloud.EndpointOpts) (string, error) { return f.srv.URL + "/", nil },
		},
	}
}

// keys lists the requests received, in order.
func (f *federationFake) keys() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.calls))
	for _, c := range f.calls {
		out = append(out, c.key)
	}
	return out
}

// bodyOf decodes the JSON body of the first request with key.
func (f *federationFake) bodyOf(key string) map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		if c.key == key {
			var out map[string]any
			if err := json.Unmarshal(c.body, &out); err != nil {
				f.t.Fatalf("%s body is not a JSON object: %s", key, c.body)
			}
			return out
		}
	}
	f.t.Fatalf("no %s request was made; got %v", key, f.calls)
	return nil
}

// rawBodyOf returns the raw body of the first request with key.
func (f *federationFake) rawBodyOf(key string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		if c.key == key {
			return string(c.body)
		}
	}
	f.t.Fatalf("no %s request was made; got %v", key, f.calls)
	return ""
}

// federationNull is the empty state or plan value for the schema.
func federationNull(ctx context.Context, s schema.Schema) tftypes.Value {
	return tftypes.NewValue(s.Type().TerraformType(ctx), nil)
}

func federationPlan(ctx context.Context, t *testing.T, s schema.Schema, model any) tfsdk.Plan {
	t.Helper()
	plan := tfsdk.Plan{Schema: s, Raw: federationNull(ctx, s)}
	if d := plan.Set(ctx, model); d.HasError() {
		t.Fatalf("building the plan: %v", d)
	}
	return plan
}

func federationState(ctx context.Context, t *testing.T, s schema.Schema, model any) tfsdk.State {
	t.Helper()
	state := tfsdk.State{Schema: s, Raw: federationNull(ctx, s)}
	if d := state.Set(ctx, model); d.HasError() {
		t.Fatalf("building the state: %v", d)
	}
	return state
}

// federationStringInvalid reports whether any of attr's validators rejects v.
func federationStringInvalid(ctx context.Context, attr schema.StringAttribute, v types.String) bool {
	for _, val := range attr.Validators {
		resp := &validator.StringResponse{}
		val.ValidateString(ctx, validator.StringRequest{Path: path.Root("attr"), ConfigValue: v}, resp)
		if resp.Diagnostics.HasError() {
			return true
		}
	}
	return false
}

func federationSchemaOf(t *testing.T, r resource.Resource) schema.Schema {
	t.Helper()
	var sch resource.SchemaResponse
	r.Schema(context.Background(), resource.SchemaRequest{}, &sch)
	if sch.Diagnostics.HasError() {
		t.Fatalf("schema: %v", sch.Diagnostics)
	}
	return sch.Schema
}

// The rules as jsonencode() writes them: compact, keys sorted. The second rule
// carries a key gophercloud's typed MappingRule does not know.
const configuredRules = `[{"local":[{"group":{"id":"g1"},"user":{"email":"{1}","name":"{0}"}}],"remote":[{"type":"NameID"},{"type":"mail"}]},` +
	`{"local":[{"group":{"id":"g2"}}],"remote":[{"any_one_of":["admins"],"regex":false,"type":"groups"}],"x_future":true}]`

// Keystone's echo of the same rules: its own spacing and key order.
const keystoneRules = `[{"remote": [{"type": "NameID"}, {"type": "mail"}], "local": [{"user": {"name": "{0}", "email": "{1}"}, "group": {"id": "g1"}}]},
	{"x_future": true, "remote": [{"type": "groups", "regex": false, "any_one_of": ["admins"]}], "local": [{"group": {"id": "g2"}}]}]`

func mappingBody(rules string) string {
	return `{"mapping": {"id": "tf-map", "schema_version": "1.0", "rules": ` + rules + `, "links": {"self": "x"}}}`
}

func TestFederationRulesValidator(t *testing.T) {
	ctx := context.Background()
	attr := federationSchemaOf(t, &federationMappingResource{}).Attributes["rules"].(schema.StringAttribute)
	for in, wantErr := range map[string]bool{
		configuredRules:              false,
		`[{"local":[],"remote":[]}]`: false,
		`[]`:                         true, // Keystone refuses an empty mapping
		`{"local":[]}`:               true, // an object, not an array
		`not json`:                   true,
		``:                           true,
	} {
		if got := federationStringInvalid(ctx, attr, types.StringValue(in)); got != wantErr {
			t.Errorf("rules %q: invalid = %v, want %v", in, got, wantErr)
		}
	}
	if federationStringInvalid(ctx, attr, types.StringUnknown()) {
		t.Error("unknown rules failed validation; rules built from a group created in the same apply are unknown at plan")
	}
	name := federationSchemaOf(t, &federationMappingResource{}).Attributes["name"].(schema.StringAttribute)
	for in, wantErr := range map[string]bool{"idp1_mapping": false, "tf-acc-map": false, "has space": true, "a/b": true, "": true} {
		if got := federationStringInvalid(ctx, name, types.StringValue(in)); got != wantErr {
			t.Errorf("name %q: invalid = %v, want %v", in, got, wantErr)
		}
	}
}

func TestFederationRulesForState(t *testing.T) {
	canonical := federationCanonicalJSON([]byte(keystoneRules))
	if canonical != configuredRules {
		t.Fatalf("canonical form of Keystone's echo:\n got %s\nwant %s (jsonencode's output)", canonical, configuredRules)
	}
	handWritten := "[\n  " + strings.TrimPrefix(configuredRules, "[")
	for _, tc := range []struct {
		name  string
		prior types.String
		want  string
	}{
		{"jsonencode value is kept", types.StringValue(configuredRules), configuredRules},
		{"hand-formatted value with the same rules is kept", types.StringValue(handWritten), handWritten},
		{"different rules show the server's", types.StringValue(`[{"local":[],"remote":[{"type":"other"}]}]`), configuredRules},
		{"import (null prior) takes the server's", types.StringNull(), configuredRules},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := federationRulesForState(tc.prior, json.RawMessage(keystoneRules)); got.ValueString() != tc.want {
				t.Fatalf("got %s\nwant %s", got.ValueString(), tc.want)
			}
		})
	}
}

func TestFederationMappingLifecycle(t *testing.T) {
	ctx := context.Background()
	updatedRules := `[{"local":[{"group":{"id":"g3"}}],"remote":[{"type":"mail"}]}]`
	f := newFederationFake(t, map[string][]federationReply{
		"PUT /v3/OS-FEDERATION/mappings/tf-map": {{http.StatusCreated, mappingBody(keystoneRules)}},
		"GET /v3/OS-FEDERATION/mappings/tf-map": {
			{http.StatusOK, mappingBody(keystoneRules)},
			{http.StatusOK, mappingBody(updatedRules)},
		},
		"PATCH /v3/OS-FEDERATION/mappings/tf-map":  {{http.StatusOK, mappingBody(updatedRules)}},
		"DELETE /v3/OS-FEDERATION/mappings/tf-map": {{http.StatusNoContent, ``}},
	})
	r := &federationMappingResource{config: f.config()}
	s := federationSchemaOf(t, r)

	planned := federationMappingModel{ID: types.StringUnknown(), Name: types.StringValue("tf-map"), Rules: types.StringValue(configuredRules)}
	createResp := resource.CreateResponse{State: tfsdk.State{Schema: s, Raw: federationNull(ctx, s)}}
	r.Create(ctx, resource.CreateRequest{Plan: federationPlan(ctx, t, s, &planned)}, &createResp)
	if createResp.Diagnostics.HasError() {
		t.Fatalf("create: %v", createResp.Diagnostics)
	}
	// Lossless: the key the typed client would have dropped reaches Keystone.
	sent := f.rawBodyOf("PUT /v3/OS-FEDERATION/mappings/tf-map")
	if !strings.Contains(sent, `"x_future":true`) {
		t.Fatalf("PUT body %s lost a rule key", sent)
	}
	if got := f.bodyOf("PUT /v3/OS-FEDERATION/mappings/tf-map"); len(got) != 1 || got["mapping"] == nil {
		t.Fatalf("PUT body %v, want {mapping: {rules: ...}}", got)
	}

	readResp := resource.ReadResponse{State: createResp.State}
	r.Read(ctx, resource.ReadRequest{State: createResp.State}, &readResp)
	var read federationMappingModel
	readResp.State.Get(ctx, &read)
	if readResp.Diagnostics.HasError() || read.Rules.ValueString() != configuredRules || read.ID.ValueString() != "tf-map" {
		t.Fatalf("read: diags %v, id %s, rules %s; want the configured string kept", readResp.Diagnostics, read.ID, read.Rules)
	}

	updated := read
	updated.Rules = types.StringValue(updatedRules)
	updateResp := resource.UpdateResponse{State: readResp.State}
	r.Update(ctx, resource.UpdateRequest{Plan: federationPlan(ctx, t, s, &updated), State: readResp.State}, &updateResp)
	if updateResp.Diagnostics.HasError() {
		t.Fatalf("update: %v", updateResp.Diagnostics)
	}

	r.Delete(ctx, resource.DeleteRequest{State: updateResp.State}, &resource.DeleteResponse{State: updateResp.State})

	want := []string{
		"PUT /v3/OS-FEDERATION/mappings/tf-map", "GET /v3/OS-FEDERATION/mappings/tf-map",
		"PATCH /v3/OS-FEDERATION/mappings/tf-map", "DELETE /v3/OS-FEDERATION/mappings/tf-map",
	}
	if got := f.keys(); !reflect.DeepEqual(got, want) {
		t.Fatalf("requests = %v\nwant       %v", got, want)
	}
}

func TestFederationMappingCreateNamesTheImport(t *testing.T) {
	ctx := context.Background()
	f := newFederationFake(t, map[string][]federationReply{
		"PUT /v3/OS-FEDERATION/mappings/idp1_mapping": {{http.StatusConflict, `{"error": {"code": 409}}`}},
	})
	r := &federationMappingResource{config: f.config()}
	s := federationSchemaOf(t, r)
	planned := federationMappingModel{ID: types.StringUnknown(), Name: types.StringValue("idp1_mapping"), Rules: types.StringValue(configuredRules)}
	resp := resource.CreateResponse{State: tfsdk.State{Schema: s, Raw: federationNull(ctx, s)}}
	r.Create(ctx, resource.CreateRequest{Plan: federationPlan(ctx, t, s, &planned)}, &resp)
	if !resp.Diagnostics.HasError() || !strings.Contains(resp.Diagnostics[0].Detail(), "terraform import") {
		t.Fatalf("diagnostics = %v, want the import named", resp.Diagnostics)
	}
}

func TestFederationMappingReadAndDeleteOfAMappingThatIsGone(t *testing.T) {
	ctx := context.Background()
	f := newFederationFake(t, map[string][]federationReply{
		"GET /v3/OS-FEDERATION/mappings/tf-map":    {{http.StatusNotFound, `{"error": {"code": 404}}`}},
		"DELETE /v3/OS-FEDERATION/mappings/tf-map": {{http.StatusNotFound, `{"error": {"code": 404}}`}},
	})
	r := &federationMappingResource{config: f.config()}
	s := federationSchemaOf(t, r)
	prior := federationMappingModel{ID: types.StringValue("tf-map"), Name: types.StringValue("tf-map"), Rules: types.StringValue(configuredRules)}
	readResp := resource.ReadResponse{State: federationState(ctx, t, s, &prior)}
	r.Read(ctx, resource.ReadRequest{State: federationState(ctx, t, s, &prior)}, &readResp)
	if readResp.Diagnostics.HasError() || !readResp.State.Raw.IsNull() {
		t.Fatalf("read: diags %v, null %v; want the mapping dropped from state", readResp.Diagnostics, readResp.State.Raw.IsNull())
	}
	deleteResp := resource.DeleteResponse{State: federationState(ctx, t, s, &prior)}
	r.Delete(ctx, resource.DeleteRequest{State: federationState(ctx, t, s, &prior)}, &deleteResp)
	if deleteResp.Diagnostics.HasError() {
		t.Fatalf("delete of a mapping already gone: %v", deleteResp.Diagnostics)
	}
}

func TestFederationMappingReadOfAnAnswerWithoutTheMapping(t *testing.T) {
	for name, body := range map[string]string{
		"no mapping": `{}`,
		"null rules": `{"mapping": {"id": "tf-map", "rules": null}}`,
	} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			f := newFederationFake(t, map[string][]federationReply{
				"GET /v3/OS-FEDERATION/mappings/tf-map": {{http.StatusOK, body}},
			})
			r := &federationMappingResource{config: f.config()}
			s := federationSchemaOf(t, r)
			prior := federationMappingModel{ID: types.StringValue("tf-map"), Name: types.StringValue("tf-map"), Rules: types.StringValue(configuredRules)}
			readResp := resource.ReadResponse{State: federationState(ctx, t, s, &prior)}
			r.Read(ctx, resource.ReadRequest{State: federationState(ctx, t, s, &prior)}, &readResp)
			var got federationMappingModel
			readResp.State.Get(ctx, &got)
			if !readResp.Diagnostics.HasError() || got != prior {
				t.Fatalf("read: diags %v, state %+v; want an error and the prior state unchanged", readResp.Diagnostics, got)
			}
		})
	}
}

func TestFederationMappingImportState(t *testing.T) {
	ctx := context.Background()
	s := federationSchemaOf(t, &federationMappingResource{})
	for _, tc := range []struct {
		name, id, wantError string
	}{
		{name: "empty", id: "", wantError: "Import needs a federation mapping ID"},
		{name: "mapping name", id: "idp1_mapping"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := resource.ImportStateResponse{State: tfsdk.State{Schema: s, Raw: federationNull(ctx, s)}}
			(&federationMappingResource{}).ImportState(ctx, resource.ImportStateRequest{ID: tc.id}, &resp)
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
			var got federationMappingModel
			resp.State.Get(ctx, &got)
			if len(summaries) != 0 || got.ID.ValueString() != tc.id || got.Name.ValueString() != tc.id {
				t.Fatalf("import: errors %v, id %s, name %s", summaries, got.ID, got.Name)
			}
		})
	}
}
