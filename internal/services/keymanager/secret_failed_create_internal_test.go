// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package keymanager

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"

	"github.com/platform9/terraform-provider-pcd/internal/clients"
)

// fakeConfig points a clients.Config at a test server, so the Barbican client
// the resource builds resolves to it. The locator ignores the endpoint options,
// so it answers for every service type and availability. NewKeyManagerV1 appends
// "v1/" to the endpoint, so the fake serves /v1/secrets, not /secrets. An
// EndpointOverrides entry would blank that prefix (internal/clients/config.go
// applyOverride), so the locator is the only wiring this test uses.
// Its client gets a transport of its own, not http.DefaultTransport: every
// httptest.Server.Close closes the default transport's idle connections,
// which can cut a request another parallel test is sending.
func fakeConfig(url string) *clients.Config {
	return &clients.Config{
		Region: "region-one",
		Provider: &gophercloud.ProviderClient{
			HTTPClient:      http.Client{Transport: &http.Transport{}},
			EndpointLocator: func(gophercloud.EndpointOpts) (string, error) { return url + "/", nil },
		},
	}
}

// A secret whose payload store ends in ERROR stays in Barbican. Create used to
// return without state, so Terraform forgot the secret, the next apply created
// another, and the first had to be deleted through the API by hand. Create must
// return the error with the secret in state (Terraform then taints it), and the
// refresh and delete a destroy runs must remove the secret even though Barbican
// reports ERROR.
func TestSecretCreateKeepsASecretThatFailedToBecomeActive(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	var mu sync.Mutex
	deleteCalled, getsAfterDelete := false, 0
	// Barbican's timestamps parse only as gophercloud.RFC3339NoZ ("2006-01-02T15:04:05"):
	// a trailing "Z" or an offset makes secrets.Get(...).Extract() fail with a
	// parse error that looks like a provider bug.
	barbican := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch r.Method + " " + r.URL.Path {
		case "POST /v1/secrets":
			// secrets.Create accepts 201 only; 202 is rejected.
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{"secret_ref": "http://barbican.invalid/v1/secrets/sec-1"}`)
		case "GET /v1/secrets/sec-1":
			if deleteCalled {
				getsAfterDelete++
				w.WriteHeader(http.StatusNotFound)
				return
			}
			fmt.Fprint(w, `{"secret_ref": "http://barbican.invalid/v1/secrets/sec-1",
				"name": "tf-acc-secret", "status": "ERROR", "secret_type": "passphrase",
				"algorithm": "", "bit_length": 0, "mode": "", "creator_id": "user-1",
				"content_types": {"default": "text/plain"},
				"created": "2026-09-19T12:00:00", "updated": "2026-09-19T12:00:01"}`)
		case "DELETE /v1/secrets/sec-1":
			// secrets.Delete accepts 202 and 204 only; 200 is rejected.
			deleteCalled = true
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotImplemented)
		}
	}))
	defer barbican.Close()

	r := &secretResource{config: fakeConfig(barbican.URL)}
	var sch resource.SchemaResponse
	r.Schema(ctx, resource.SchemaRequest{}, &sch)
	s := sch.Schema

	// payload and payload_content_type are both required for this test to mean
	// anything: without payload Create skips waitForSecretActive entirely, and
	// with payload but no payload_content_type Create fails before any HTTP call.
	planned := secretModel{
		ID:                     types.StringUnknown(),
		Name:                   types.StringValue("tf-acc-secret"),
		Algorithm:              types.StringUnknown(),
		BitLength:              types.Int64Unknown(),
		Mode:                   types.StringUnknown(),
		SecretType:             types.StringValue("passphrase"),
		Expiration:             types.StringUnknown(),
		Payload:                types.StringValue("s3cr3t-passphrase"),
		PayloadContentType:     types.StringValue("text/plain"),
		PayloadContentEncoding: types.StringNull(),
		SecretRef:              types.StringUnknown(),
		Status:                 types.StringUnknown(),
		CreatorID:              types.StringUnknown(),
		ContentTypes:           types.MapUnknown(types.StringType),
		CreatedAt:              types.StringUnknown(),
		UpdatedAt:              types.StringUnknown(),
		Region:                 types.StringUnknown(),
	}
	plan := tfsdk.Plan{Schema: s, Raw: tftypes.NewValue(s.Type().TerraformType(ctx), nil)}
	if d := plan.Set(ctx, &planned); d.HasError() {
		t.Fatalf("building the plan: %v", d)
	}

	createResp := resource.CreateResponse{State: tfsdk.State{Schema: s, Raw: tftypes.NewValue(s.Type().TerraformType(ctx), nil)}}
	r.Create(ctx, resource.CreateRequest{Plan: plan}, &createResp)
	if !createResp.Diagnostics.HasError() {
		t.Fatal("create succeeded; want the ERROR status reported")
	}
	if createResp.State.Raw.IsNull() {
		t.Fatal("create returned no state: Terraform forgets sec-1 and the next apply creates a second secret")
	}
	if !createResp.State.Raw.IsFullyKnown() {
		t.Fatalf("create state holds unknown values, which Terraform refuses: %v", createResp.State.Raw)
	}
	var got secretModel
	if d := createResp.State.Get(ctx, &got); d.HasError() {
		t.Fatalf("reading the create state: %v", d)
	}
	if got.ID.ValueString() != "sec-1" {
		t.Fatalf("create state id = %s, want sec-1: Delete feeds this straight into the URL", got.ID)
	}
	if got.SecretRef.ValueString() != "http://barbican.invalid/v1/secrets/sec-1" {
		t.Fatalf("create state secret_ref = %s, want the ref Barbican returned", got.SecretRef)
	}
	if got.Name.ValueString() != "tf-acc-secret" || got.SecretType.ValueString() != "passphrase" {
		t.Fatalf("create state name=%s secret_type=%s; want tf-acc-secret, passphrase", got.Name, got.SecretType)
	}

	// terraform destroy (or the replacing apply) refreshes the tainted secret first.
	readResp := resource.ReadResponse{State: createResp.State}
	r.Read(ctx, resource.ReadRequest{State: createResp.State}, &readResp)
	if readResp.Diagnostics.HasError() {
		t.Fatalf("refresh of the failed secret: %v", readResp.Diagnostics)
	}
	if readResp.State.Raw.IsNull() {
		t.Fatal("refresh dropped the secret from state; Barbican still reports it")
	}
	if d := readResp.State.Get(ctx, &got); d.HasError() {
		t.Fatalf("reading the refreshed state: %v", d)
	}
	if got.Status.ValueString() != "ERROR" {
		t.Fatalf("refreshed status = %s, want ERROR", got.Status)
	}
	if got.CreatorID.ValueString() != "user-1" || got.CreatedAt.ValueString() == "" {
		t.Fatalf("refreshed creator_id=%s created_at=%s; want the server values", got.CreatorID, got.CreatedAt)
	}

	deleteResp := resource.DeleteResponse{State: readResp.State}
	r.Delete(ctx, resource.DeleteRequest{State: readResp.State}, &deleteResp)
	if deleteResp.Diagnostics.HasError() {
		t.Fatalf("delete of a secret in ERROR: %v", deleteResp.Diagnostics)
	}
	mu.Lock()
	defer mu.Unlock()
	if !deleteCalled {
		t.Fatal("delete never called DELETE /v1/secrets/sec-1")
	}
	if getsAfterDelete != 0 {
		t.Fatalf("delete polled the secret %d times; keymanager has no delete waiter", getsAfterDelete)
	}
}

// The following two tests drive Create down the path the test above doesn't
// reach: the status wait succeeds (ACTIVE), and the read-back that follows it
// is what fails. readInto returns without touching its model argument on both
// a 404 and a non-404 error, so a Create that unconditionally sets state from
// the plan afterward would write back the plan's own unknown values on top of
// the clean, fully-known row RecordCreated already wrote -- a state Terraform
// refuses. The guard must instead: on a 404, report the error and remove the
// resource (nothing is left to keep); on any other error, report it and leave
// the recorded row in place (the secret still exists).

// A secret create whose status wait reaches ACTIVE, but whose read-back then
// finds the secret already gone, must report the error and drop the secret
// from state.
func TestSecretCreateDropsStateWhenReadBackAfterActive404s(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	var mu sync.Mutex
	gets := 0
	barbican := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch r.Method + " " + r.URL.Path {
		case "POST /v1/secrets":
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{"secret_ref": "http://barbican.invalid/v1/secrets/sec-1"}`)
		case "GET /v1/secrets/sec-1":
			gets++
			if gets == 1 {
				// Satisfies waitForSecretActive on the very first poll.
				fmt.Fprint(w, `{"secret_ref": "http://barbican.invalid/v1/secrets/sec-1",
					"name": "tf-acc-gone", "status": "ACTIVE", "secret_type": "passphrase",
					"algorithm": "", "bit_length": 0, "mode": "", "creator_id": "user-1",
					"content_types": {"default": "text/plain"},
					"created": "2026-09-19T12:00:00", "updated": "2026-09-19T12:00:01"}`)
				return
			}
			// The read-back that immediately follows the wait finds it gone.
			w.WriteHeader(http.StatusNotFound)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotImplemented)
		}
	}))
	defer barbican.Close()

	r := &secretResource{config: fakeConfig(barbican.URL)}
	var sch resource.SchemaResponse
	r.Schema(ctx, resource.SchemaRequest{}, &sch)
	s := sch.Schema

	// payload and payload_content_type are both required for this test to mean
	// anything: without payload Create skips waitForSecretActive entirely, and
	// with payload but no payload_content_type Create fails before any HTTP call.
	planned := secretModel{
		ID:                     types.StringUnknown(),
		Name:                   types.StringValue("tf-acc-gone"),
		Algorithm:              types.StringUnknown(),
		BitLength:              types.Int64Unknown(),
		Mode:                   types.StringUnknown(),
		SecretType:             types.StringValue("passphrase"),
		Expiration:             types.StringUnknown(),
		Payload:                types.StringValue("s3cr3t-passphrase"),
		PayloadContentType:     types.StringValue("text/plain"),
		PayloadContentEncoding: types.StringNull(),
		SecretRef:              types.StringUnknown(),
		Status:                 types.StringUnknown(),
		CreatorID:              types.StringUnknown(),
		ContentTypes:           types.MapUnknown(types.StringType),
		CreatedAt:              types.StringUnknown(),
		UpdatedAt:              types.StringUnknown(),
		Region:                 types.StringUnknown(),
	}
	plan := tfsdk.Plan{Schema: s, Raw: tftypes.NewValue(s.Type().TerraformType(ctx), nil)}
	if d := plan.Set(ctx, &planned); d.HasError() {
		t.Fatalf("building the plan: %v", d)
	}

	createResp := resource.CreateResponse{State: tfsdk.State{Schema: s, Raw: tftypes.NewValue(s.Type().TerraformType(ctx), nil)}}
	r.Create(ctx, resource.CreateRequest{Plan: plan}, &createResp)

	if !createResp.Diagnostics.HasError() {
		t.Fatal("create succeeded; want the post-ACTIVE 404 read-back reported")
	}
	if !createResp.State.Raw.IsNull() {
		t.Fatal("create left a row in state for a secret a 404 read-back confirmed gone: " +
			"a genuine 404 right after ACTIVE means there is nothing left to keep")
	}
	mu.Lock()
	defer mu.Unlock()
	if gets < 2 {
		t.Fatalf("GET called %d times; want the wait's ACTIVE poll and the read-back poll", gets)
	}
}

// A secret create whose status wait reaches ACTIVE, but whose read-back then
// fails with a server error (not a 404), must report the error and leave the
// row RecordCreated wrote in place: the secret still exists, and dropping it
// would recreate the orphan this change removes.
func TestSecretCreateKeepsStateWhenReadBackAfterActiveFails(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	var mu sync.Mutex
	gets := 0
	barbican := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch r.Method + " " + r.URL.Path {
		case "POST /v1/secrets":
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{"secret_ref": "http://barbican.invalid/v1/secrets/sec-1"}`)
		case "GET /v1/secrets/sec-1":
			gets++
			if gets == 1 {
				// Satisfies waitForSecretActive on the very first poll.
				fmt.Fprint(w, `{"secret_ref": "http://barbican.invalid/v1/secrets/sec-1",
					"name": "tf-acc-flaky", "status": "ACTIVE", "secret_type": "passphrase",
					"algorithm": "", "bit_length": 0, "mode": "", "creator_id": "user-1",
					"content_types": {"default": "text/plain"},
					"created": "2026-09-19T12:00:00", "updated": "2026-09-19T12:00:01"}`)
				return
			}
			// The read-back that immediately follows the wait hits a transient
			// server error; the secret itself is still there.
			w.WriteHeader(http.StatusInternalServerError)
			fmt.Fprint(w, `{"error": "barbican temporarily unavailable"}`)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotImplemented)
		}
	}))
	defer barbican.Close()

	r := &secretResource{config: fakeConfig(barbican.URL)}
	var sch resource.SchemaResponse
	r.Schema(ctx, resource.SchemaRequest{}, &sch)
	s := sch.Schema

	planned := secretModel{
		ID:                     types.StringUnknown(),
		Name:                   types.StringValue("tf-acc-flaky"),
		Algorithm:              types.StringUnknown(),
		BitLength:              types.Int64Unknown(),
		Mode:                   types.StringUnknown(),
		SecretType:             types.StringValue("passphrase"),
		Expiration:             types.StringUnknown(),
		Payload:                types.StringValue("s3cr3t-passphrase"),
		PayloadContentType:     types.StringValue("text/plain"),
		PayloadContentEncoding: types.StringNull(),
		SecretRef:              types.StringUnknown(),
		Status:                 types.StringUnknown(),
		CreatorID:              types.StringUnknown(),
		ContentTypes:           types.MapUnknown(types.StringType),
		CreatedAt:              types.StringUnknown(),
		UpdatedAt:              types.StringUnknown(),
		Region:                 types.StringUnknown(),
	}
	plan := tfsdk.Plan{Schema: s, Raw: tftypes.NewValue(s.Type().TerraformType(ctx), nil)}
	if d := plan.Set(ctx, &planned); d.HasError() {
		t.Fatalf("building the plan: %v", d)
	}

	createResp := resource.CreateResponse{State: tfsdk.State{Schema: s, Raw: tftypes.NewValue(s.Type().TerraformType(ctx), nil)}}
	r.Create(ctx, resource.CreateRequest{Plan: plan}, &createResp)

	if !createResp.Diagnostics.HasError() {
		t.Fatal("create succeeded; want the post-ACTIVE 500 read-back reported")
	}
	if createResp.State.Raw.IsNull() {
		t.Fatal("create dropped sec-1 from state even though the secret still exists behind the failed read-back")
	}
	if !createResp.State.Raw.IsFullyKnown() {
		t.Fatalf("create state holds unknown values, which Terraform refuses: %v", createResp.State.Raw)
	}
	var got secretModel
	if d := createResp.State.Get(ctx, &got); d.HasError() {
		t.Fatalf("reading the create state: %v", d)
	}
	if got.ID.ValueString() != "sec-1" {
		t.Fatalf("create state id = %s, want sec-1: a row without one names nothing a destroy could delete", got.ID)
	}
	mu.Lock()
	defer mu.Unlock()
	if gets < 2 {
		t.Fatalf("GET called %d times; want the wait's ACTIVE poll and the failed read-back poll", gets)
	}
}

// The tests below cover the rows provider v0.1.13 and earlier left in state.
// Create records the secret before anything after the POST can fail, but such
// a row still reaches Read and Delete.

const (
	secretsPath = "/v1/secrets"
	secretPath  = secretsPath + "/sec-1"
)

// secretJSON is sec-1 as Barbican returns it: the body is the secret itself,
// with no wrapping key.
const secretJSON = `{"secret_ref": "http://barbican.invalid/v1/secrets/sec-1",
	"name": "tf-acc-secret", "status": "ACTIVE", "secret_type": "passphrase",
	"algorithm": "", "bit_length": 0, "mode": "", "creator_id": "user-1",
	"content_types": {"default": "text/plain"},
	"created": "2026-10-02T00:00:00", "updated": "2026-10-02T00:00:00"}`

// secretListJSON is Barbican's answer for the collection URL: a page of
// secrets, not a secret.
const secretListJSON = `{"secrets": [` + secretJSON + `], "total": 1}`

// secretRoutes answers the calls secretResource's Read and Delete make for
// sec-1 the way Barbican does. Each test replaces or adds the route it
// exercises.
func secretRoutes() barbicanRoutes {
	return barbicanRoutes{
		"GET " + secretPath:    reply(http.StatusOK, secretJSON),
		"DELETE " + secretPath: reply(http.StatusNoContent, ""),
	}
}

// secretRowWithoutID is the row provider v0.1.13 and earlier left in state when
// the read-back after a successful POST failed: Terraform saved the unknowns
// Create returned as null, the ID among them. A later refresh of that row read
// the collection URL, took the list for an empty secret and saved id "", so id
// is null or empty. Read and Delete look only at the id.
func secretRowWithoutID(t *testing.T, r *secretResource, id types.String) tfsdk.State {
	t.Helper()
	return newState(t, schemaOf(t, r), &secretModel{
		ID:                     id,
		Name:                   types.StringValue("tf-acc-secret"),
		Algorithm:              types.StringNull(),
		BitLength:              types.Int64Null(),
		Mode:                   types.StringNull(),
		SecretType:             types.StringValue("passphrase"),
		Expiration:             types.StringNull(),
		Payload:                types.StringValue("s3cr3t-passphrase"),
		PayloadContentType:     types.StringValue("text/plain"),
		PayloadContentEncoding: types.StringNull(),
		SecretRef:              types.StringNull(),
		Status:                 types.StringNull(),
		CreatorID:              types.StringNull(),
		ContentTypes:           types.MapNull(types.StringType),
		CreatedAt:              types.StringNull(),
		UpdatedAt:              types.StringNull(),
		Region:                 types.StringNull(),
	})
}

// Read used to send GET for an empty ID, which reaches the collection URL;
// Barbican answers that with the list, which gophercloud decodes as an empty
// secret, and Read saved it back as id "". Read must drop the row with a
// warning that names the secret, without sending anything.
func TestSecretReadDropsARowWithNoID(t *testing.T) {
	t.Parallel()
	routes := secretRoutes()
	routes["GET "+secretsPath+"/"] = reply(http.StatusOK, secretListJSON)
	barbican := newFakeBarbican(t, routes)
	r := &secretResource{config: barbican.config}

	for _, id := range []types.String{types.StringNull(), types.StringValue("")} {
		resp := runRead(r, secretRowWithoutID(t, r, id))
		if sent := barbican.received(); len(sent) != 0 {
			t.Fatalf("id %s: refresh of a row with no ID sent %v; it names nothing to read", id, sent)
		}
		if resp.Diagnostics.HasError() || resp.Diagnostics.WarningsCount() != 1 {
			t.Fatalf("id %s: diagnostics = %v, want exactly one warning", id, resp.Diagnostics)
		}
		if detail := resp.Diagnostics[0].Detail(); !strings.Contains(detail, `"tf-acc-secret"`) {
			t.Fatalf("id %s: warning detail %q does not name the secret to look for", id, detail)
		}
		if !resp.State.Raw.IsNull() {
			t.Fatalf("id %s: refresh kept a row with no ID: %v", id, resp.State.Raw)
		}
	}
}

// secrets.Extract decodes the whole body as the secret, so a 200 for the
// secret's own URL whose body is not a secret gives an empty one and no error.
// readInto must report that as an error, and not as not-found: that would drop
// a real secret from state.
func TestSecretReadIntoRefusesAnAnswerWithoutTheSecret(t *testing.T) {
	t.Parallel()
	routes := secretRoutes()
	routes["GET "+secretPath] = reply(http.StatusOK, secretListJSON)
	r := &secretResource{config: newFakeBarbican(t, routes).config}
	client, err := r.config.KeyManagerV1Client()
	if err != nil {
		t.Fatalf("building the fake Barbican client: %v", err)
	}

	m := secretModel{Region: types.StringNull()}
	notFound, diags := r.readInto(context.Background(), client, "sec-1", &m)
	if notFound {
		t.Fatal("readInto reported sec-1 not found; the next refresh would drop a secret that exists")
	}
	if !diags.HasError() {
		t.Fatalf("readInto accepted an answer without the secret: diagnostics %v", diags)
	}
}

// terraform destroy -refresh=false reaches Delete without Read dropping the row
// first. Delete used to send DELETE for an empty ID, which reaches the
// collection URL, and Barbican refuses that. Delete must warn and send nothing,
// so Terraform forgets the row.
func TestSecretDeleteSkipsARowWithNoID(t *testing.T) {
	t.Parallel()
	routes := secretRoutes()
	routes["DELETE "+secretsPath+"/"] = reply(http.StatusMethodNotAllowed, "")
	barbican := newFakeBarbican(t, routes)
	r := &secretResource{config: barbican.config}

	for _, id := range []types.String{types.StringNull(), types.StringValue("")} {
		resp := runDelete(r, secretRowWithoutID(t, r, id))
		if sent := barbican.received(); len(sent) != 0 {
			t.Fatalf("id %s: delete of a row with no ID sent %v; it names nothing to delete", id, sent)
		}
		if resp.Diagnostics.HasError() || resp.Diagnostics.WarningsCount() != 1 {
			t.Fatalf("id %s: diagnostics = %v, want exactly one warning", id, resp.Diagnostics)
		}
	}
}
