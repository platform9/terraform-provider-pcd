// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package identity

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/platform9/terraform-provider-pcd/internal/clients"
)

// fastSSOWait shortens the restart wait for one test. Tests that call it must
// not run in parallel: the timings are package variables.
func fastSSOWait(t *testing.T) {
	t.Helper()
	delay, interval, timeout, check := ssoRestartInitialDelay, ssoRestartPollInterval, ssoRestartTimeout, ssoRestartCheckTimeout
	ssoRestartInitialDelay, ssoRestartPollInterval, ssoRestartTimeout = time.Millisecond, time.Millisecond, 200*time.Millisecond
	ssoRestartCheckTimeout = 100 * time.Millisecond
	t.Cleanup(func() {
		ssoRestartInitialDelay, ssoRestartPollInterval, ssoRestartTimeout, ssoRestartCheckTimeout = delay, interval, timeout, check
	})
}

// hagridFake behaves like hagrid and Keystone's version document for one
// identity provider: POST stores the configuration, GET returns it, and
// DELETE clears it. With nothing stored, GET answers what current hagrid
// releases answer, a 500 "no sso entry for domain" (absent404 makes it the
// older 404), and DELETE answers hagrid's 500. keystoneDown makes the next N
// Keystone checks answer 503, as during the restart a write triggers.
// refusePost makes POST answer hagrid's 500 for a metadata URL it cannot
// download, answerOverride replaces keys of the GET answer with hagrid's own
// rendering of them, and goneOnDelete clears the configuration just before a DELETE,
// as when someone deletes it in the PCD UI between the read and the delete.
type hagridFake struct {
	t              *testing.T
	srv            *httptest.Server
	mu             sync.Mutex
	stored         map[string]any
	absent404      bool
	keystoneDown   int
	refusePost     bool
	answerOverride map[string]any
	goneOnDelete   bool
	calls          []string
}

func newHagridFake(t *testing.T) *hagridFake {
	t.Helper()
	f := &hagridFake{t: t}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *hagridFake) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := r.Method + " " + r.URL.Path
	f.calls = append(f.calls, key)
	w.Header().Set("Content-Type", "application/json")
	switch key {
	case "GET /v3":
		if f.keystoneDown > 0 {
			f.keystoneDown--
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		fmt.Fprint(w, `{"version": {"id": "v3.14", "status": "stable"}}`)
	case "POST /hagrid/ssoconfig":
		if f.refusePost {
			w.WriteHeader(http.StatusInternalServerError)
			fmt.Fprint(w, `{"error": "failed to setup SSO", "msg": "Incorrect metadata url"}`)
			return
		}
		raw, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(raw, &f.stored); err != nil {
			f.t.Errorf("POST body is not JSON: %s", raw)
		}
		w.WriteHeader(http.StatusOK)
	case "GET /hagrid/ssoconfig/tf-acc-sso":
		if f.stored == nil && f.absent404 {
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"error": "failed to read SSO config", "msg": "Configuration not found"}`)
			return
		}
		if f.stored == nil {
			w.WriteHeader(http.StatusInternalServerError)
			fmt.Fprint(w, `{"error": "failed to read SSO config", "msg": "no sso entry for domain "tf-acc-sso""}`)
			return
		}
		out := map[string]any{"login_url": "/sso/tf-acc-sso/Shibboleth.sso/Login", "metadata": "<EntityDescriptor/>", "signing": ""}
		for k, v := range f.stored {
			out[k] = v
		}
		for k, v := range f.answerOverride {
			out[k] = v
		}
		b, _ := json.Marshal(out)
		fmt.Fprint(w, string(b))
	case "DELETE /hagrid/ssoconfig/tf-acc-sso":
		if f.goneOnDelete {
			f.stored = nil
		}
		if f.stored == nil {
			w.WriteHeader(http.StatusInternalServerError)
			fmt.Fprint(w, `{"error": "failed to delete SSO config", "msg": "Error, ssoenabled not found"`)
			return
		}
		f.stored = nil
		w.WriteHeader(http.StatusOK)
	default:
		f.t.Errorf("unexpected request %s", key)
		w.WriteHeader(http.StatusNotImplemented)
	}
}

// config points a clients.Config at the fake, with a transport of its own
// (see federationFake.config).
func (f *hagridFake) config() *clients.Config {
	return &clients.Config{
		Region: "region-one",
		Provider: &gophercloud.ProviderClient{
			HTTPClient:      http.Client{Transport: &http.Transport{}},
			EndpointLocator: func(gophercloud.EndpointOpts) (string, error) { return f.srv.URL + "/", nil },
		},
	}
}

func (f *hagridFake) count(key string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if c == key {
			n++
		}
	}
	return n
}

func (f *hagridFake) clients(t *testing.T) (hagrid, identity *gophercloud.ServiceClient) {
	t.Helper()
	cfg := f.config()
	hagrid, err := cfg.HagridClient()
	if err != nil {
		t.Fatal(err)
	}
	identity, err = cfg.IdentityV3Client()
	if err != nil {
		t.Fatal(err)
	}
	return hagrid, identity
}

func ssoModel(attributeMap string) ssoConfigurationModel {
	return ssoConfigurationModel{
		ID:               types.StringUnknown(),
		IdentityProvider: types.StringValue("tf-acc-sso"),
		SSOProvider:      types.StringValue("okta"),
		EntityID:         types.StringValue("https://idp.example.com/entity"),
		MetadataURL:      types.StringValue("https://idp.example.com/metadata"),
		AttributeMapXML:  types.StringValue(attributeMap),
		LoginURL:         types.StringUnknown(),
		ACSURL:           types.StringUnknown(),
		SPEntityID:       types.StringUnknown(),
	}
}

// hagrid takes these five keys; login_url is its own output.
func TestSSOConfigurationBody(t *testing.T) {
	m := ssoModel("<Attributes/>")
	raw, err := json.Marshal(ssoConfigurationBody(&m))
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"domain_name": "tf-acc-sso", "provider": "okta", "entity_id": "https://idp.example.com/entity",
		"metadata_url": "https://idp.example.com/metadata", "attr_map_xml": "<Attributes/>",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("body = %v\nwant  %v", got, want)
	}
}

// PCD's Shibboleth serves entity ID https://<host>/keystone and the ACS at
// /sso/<identity provider>/Shibboleth.sso/SAML2/POST, on hagrid's host.
func TestSSOServiceProviderURLs(t *testing.T) {
	hagrid := &gophercloud.ServiceClient{Endpoint: "https://pcd.example.com/hagrid/"}
	entityID, acs := ssoServiceProviderURLs(hagrid, "IDP1")
	if entityID != "https://pcd.example.com/keystone" || acs != "https://pcd.example.com/sso/IDP1/Shibboleth.sso/SAML2/POST" {
		t.Fatalf("got %s and %s", entityID, acs)
	}
}

func TestSSOAlreadyGone(t *testing.T) {
	codeErr := func(status int, body string) error {
		return gophercloud.ErrUnexpectedResponseCode{Actual: status, Body: []byte(body), Expected: []int{200}, Method: "DELETE", URL: "x"}
	}
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"404", codeErr(404, ``), true},
		{"500 nothing configured", codeErr(500, `{"error": "failed to delete SSO config", "msg": "Error, ssoenabled not found"`), true},
		{"500 no sso entry", codeErr(500, `{"error": "failed to delete SSO config", "msg": "no sso entry for domain "tf-acc-sso""}`), true},
		{"500 consul down", codeErr(500, `{"error": "failed to delete SSO config", "msg": "Unexpected response code: 500"`), false},
		{"401", codeErr(401, ``), false},
		{"transport", errors.New("connection refused"), false},
	} {
		if got := ssoAlreadyGone(tc.err); got != tc.want {
			t.Errorf("%s: ssoAlreadyGone = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestWaitForSSOServicesWaitsOutTheRestart(t *testing.T) {
	fastSSOWait(t)
	f := newHagridFake(t)
	f.stored = map[string]any{"domain_name": "tf-acc-sso", "entity_id": "e"}
	f.keystoneDown = 3
	hagrid, identity := f.clients(t)
	if err := waitForSSOServices(context.Background(), identity, hagrid, "tf-acc-sso"); err != nil {
		t.Fatalf("wait: %v", err)
	}
	// Three 503s, then two answers in a row.
	if got := f.count("GET /v3"); got != 5 {
		t.Fatalf("keystone checked %d times, want 5", got)
	}
	if got := f.count("GET /hagrid/ssoconfig/tf-acc-sso"); got != 2 {
		t.Fatalf("hagrid checked %d times, want 2", got)
	}
}

// hagrid answers a name with nothing configured with a 404 or, in current PCD
// releases, a 500 that says so. Both mean "not configured", and that 500 must
// not go through max_retries, which retries a GET that draws a 5xx. Any other
// 500 is an error that keeps hagrid's message, and a 200 without the
// configuration is clients.ErrNoObject.
func TestGetSSOConfigurationAnswers(t *testing.T) {
	for _, tc := range []struct {
		name          string
		reply         federationReply
		notConfigured bool
		noObject      bool
		wantErr       string
	}{
		{name: "configured", reply: federationReply{http.StatusOK, `{"domain_name": "tf-acc-sso", "entity_id": "e"}`}},
		{name: "404", reply: federationReply{http.StatusNotFound, ``}, notConfigured: true},
		{name: "500 no sso entry", notConfigured: true, reply: federationReply{http.StatusInternalServerError,
			`{"error": "failed to read SSO config", "msg": "no sso entry for domain "tf-acc-sso""}`}},
		{name: "500 other", wantErr: "Unexpected response code: 500", reply: federationReply{http.StatusInternalServerError,
			`{"error": "failed to read SSO config", "msg": "Unexpected response code: 500"`}},
		{name: "200 without the object", reply: federationReply{http.StatusOK, `{}`}, noObject: true},
		{name: "200 null", reply: federationReply{http.StatusOK, `null`}, noObject: true},
		{name: "200 without an entity ID", reply: federationReply{http.StatusOK, `{"domain_name": "tf-acc-sso"}`}, noObject: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFederationFake(t, map[string][]federationReply{"GET /hagrid/ssoconfig/tf-acc-sso": {tc.reply}})
			cfg := f.config()
			consulted := 0
			cfg.Provider.RetryFunc = func(_ context.Context, _, _ string, _ *gophercloud.RequestOpts, err error, _ uint) error {
				consulted++
				return err
			}
			hagrid, err := cfg.HagridClient()
			if err != nil {
				t.Fatal(err)
			}
			sso, err := getSSOConfiguration(context.Background(), hagrid, "tf-acc-sso")
			switch {
			case tc.notConfigured:
				if !errors.Is(err, errSSONotConfigured) {
					t.Fatalf("err = %v, want errSSONotConfigured", err)
				}
			case tc.noObject:
				if !errors.Is(err, clients.ErrNoObject) || sso != nil {
					t.Fatalf("got %+v, %v; want clients.ErrNoObject", sso, err)
				}
			case tc.wantErr != "":
				if errors.Is(err, errSSONotConfigured) || !gophercloud.ResponseCodeIs(err, http.StatusInternalServerError) ||
					!strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want a 500 that carries %q", err, tc.wantErr)
				}
			default:
				if err != nil || sso.EntityID != "e" {
					t.Fatalf("got %+v, %v; want the configuration", sso, err)
				}
			}
			if consulted != 0 {
				t.Fatalf("the retry policy was consulted %d time(s)", consulted)
			}
		})
	}
}

// After a delete, hagrid answers "nothing configured" for the name; that is
// an answer, in either form.
func TestWaitForSSOServicesTakesNothingConfiguredAsUp(t *testing.T) {
	fastSSOWait(t)
	for _, absent404 := range []bool{false, true} {
		f := newHagridFake(t)
		f.absent404 = absent404
		hagrid, identity := f.clients(t)
		if err := waitForSSOServices(context.Background(), identity, hagrid, "tf-acc-sso"); err != nil {
			t.Fatalf("absent404=%v: wait: %v", absent404, err)
		}
	}
}

// One good answer between failures is not recovery: Apache restarts can
// answer once and go down again.
func TestWaitForSSOServicesNeedsAnswersInARow(t *testing.T) {
	fastSSOWait(t)
	var mu sync.Mutex
	answers := []int{200, 503, 200, 200}
	keystoneCalls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v3" {
			status := answers[len(answers)-1]
			if keystoneCalls < len(answers) {
				status = answers[keystoneCalls]
			}
			keystoneCalls++
			w.WriteHeader(status)
			return
		}
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"entity_id": "e"}`)
	}))
	t.Cleanup(srv.Close)
	provider := &gophercloud.ProviderClient{HTTPClient: http.Client{Transport: &http.Transport{}}}
	identity := &gophercloud.ServiceClient{ProviderClient: provider, Endpoint: srv.URL + "/v3/"}
	hagrid := &gophercloud.ServiceClient{ProviderClient: provider, Endpoint: srv.URL + "/hagrid/"}
	if err := waitForSSOServices(context.Background(), identity, hagrid, "tf-acc-sso"); err != nil {
		t.Fatalf("wait: %v", err)
	}
	if keystoneCalls != 4 {
		t.Fatalf("keystone checked %d times, want 4: the flap must reset the count", keystoneCalls)
	}
}

func TestWaitForSSOServicesGivesUp(t *testing.T) {
	fastSSOWait(t)
	f := newHagridFake(t)
	f.keystoneDown = 1 << 30
	hagrid, identity := f.clients(t)
	err := waitForSSOServices(context.Background(), identity, hagrid, "tf-acc-sso")
	if err == nil || !strings.Contains(err.Error(), "did not come back") {
		t.Fatalf("err = %v, want the deadline reported", err)
	}
	// The error names the identity provider and what the last check found.
	for _, want := range []string{"for tf-acc-sso.", "Last check: keystone: ", "503"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %v, want it to contain %q", err, want)
		}
	}
}

// The provider's HTTP client has no timeout, so a Keystone that accepts a
// check and never answers must not hold the wait past its deadline: each
// check is bounded on its own.
func TestWaitForSSOServicesBoundsEachCheck(t *testing.T) {
	fastSSOWait(t)
	done := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-done:
		}
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(done) }) // runs first, so Close does not wait on a hung handler
	provider := &gophercloud.ProviderClient{HTTPClient: http.Client{Transport: &http.Transport{}}}
	identity := &gophercloud.ServiceClient{ProviderClient: provider, Endpoint: srv.URL + "/v3/"}
	hagrid := &gophercloud.ServiceClient{ProviderClient: provider, Endpoint: srv.URL + "/hagrid/"}

	bound := ssoRestartInitialDelay + ssoRestartTimeout + ssoRestartCheckTimeout + time.Second
	result := make(chan error, 1)
	start := time.Now()
	go func() { result <- waitForSSOServices(context.Background(), identity, hagrid, "tf-acc-sso") }()
	select {
	case err := <-result:
		if err == nil || !strings.Contains(err.Error(), "did not come back") || !strings.Contains(err.Error(), "context deadline exceeded") {
			t.Fatalf("err = %v, want the deadline reported with the timed-out check", err)
		}
		if elapsed := time.Since(start); elapsed > bound {
			t.Fatalf("the wait took %s, more than its deadline plus one check's bound (%s)", elapsed, bound)
		}
	case <-time.After(bound):
		t.Fatalf("the wait was still running after %s: a check that never answers holds it past its deadline", bound)
	}
}

// A wait cut short (Ctrl-C ends the operation's context) names what it waited
// for and what answered last, not just "context canceled".
func TestWaitForSSOServicesNamesAnInterruption(t *testing.T) {
	fastSSOWait(t)
	f := newHagridFake(t)
	hagrid, identity := f.clients(t)
	for name, delay := range map[string]time.Duration{"before the first check": time.Hour, "between checks": time.Millisecond} {
		ssoRestartInitialDelay, ssoRestartPollInterval = delay, time.Hour
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		err := waitForSSOServices(ctx, identity, hagrid, "tf-acc-sso")
		cancel()
		if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "tf-acc-sso was interrupted (last check: ") {
			t.Fatalf("%s: err = %v, want the interruption named", name, err)
		}
	}
}

// The first checks must wait for the restart to begin, or the wait returns
// on the answers from before it.
func TestWaitForSSOServicesWaitsBeforeTheFirstCheck(t *testing.T) {
	fastSSOWait(t)
	ssoRestartInitialDelay = 50 * time.Millisecond
	f := newHagridFake(t)
	hagrid, identity := f.clients(t)
	start := time.Now()
	if err := waitForSSOServices(context.Background(), identity, hagrid, "tf-acc-sso"); err != nil {
		t.Fatalf("wait: %v", err)
	}
	if elapsed := time.Since(start); elapsed < 50*time.Millisecond {
		t.Fatalf("returned after %s, before the initial delay", elapsed)
	}
}

func TestSSOConfigurationLifecycle(t *testing.T) {
	fastSSOWait(t)
	ctx := context.Background()
	f := newHagridFake(t)
	r := &ssoConfigurationResource{config: f.config()}
	s := federationSchemaOf(t, r)

	planned := ssoModel("<Attributes/>")
	f.keystoneDown = 2
	createResp := resource.CreateResponse{State: tfsdk.State{Schema: s, Raw: federationNull(ctx, s)}}
	r.Create(ctx, resource.CreateRequest{Plan: federationPlan(ctx, t, s, &planned)}, &createResp)
	if createResp.Diagnostics.HasError() {
		t.Fatalf("create: %v", createResp.Diagnostics)
	}
	var created ssoConfigurationModel
	createResp.State.Get(ctx, &created)
	if created.ID.ValueString() != "tf-acc-sso" || created.LoginURL.ValueString() != "/sso/tf-acc-sso/Shibboleth.sso/Login" ||
		created.ACSURL.ValueString() != f.srv.URL+"/sso/tf-acc-sso/Shibboleth.sso/SAML2/POST" ||
		created.SPEntityID.ValueString() != f.srv.URL+"/keystone" {
		t.Fatalf("created %+v", created)
	}
	if f.count("GET /v3") < 4 {
		t.Fatalf("create returned after %d keystone checks; it must wait out the restart", f.count("GET /v3"))
	}

	readResp := resource.ReadResponse{State: createResp.State}
	r.Read(ctx, resource.ReadRequest{State: createResp.State}, &readResp)
	var read ssoConfigurationModel
	readResp.State.Get(ctx, &read)
	if readResp.Diagnostics.HasError() || read.AttributeMapXML.ValueString() != "<Attributes/>" {
		t.Fatalf("read: diags %v, attribute_map_xml %s", readResp.Diagnostics, read.AttributeMapXML)
	}

	updated := read
	updated.AttributeMapXML = types.StringValue(`<Attributes><Attribute name="mail" id="mail"/></Attributes>`)
	checksBefore := f.count("GET /v3")
	updateResp := resource.UpdateResponse{State: readResp.State}
	r.Update(ctx, resource.UpdateRequest{Plan: federationPlan(ctx, t, s, &updated), State: readResp.State}, &updateResp)
	if updateResp.Diagnostics.HasError() {
		t.Fatalf("update: %v", updateResp.Diagnostics)
	}
	if f.count("POST /hagrid/ssoconfig") != 2 || f.count("GET /v3")-checksBefore < 2 {
		t.Fatalf("update: %d POSTs, %d keystone checks; want a re-POST and a wait", f.count("POST /hagrid/ssoconfig"), f.count("GET /v3")-checksBefore)
	}

	checksBefore = f.count("GET /v3")
	deleteResp := resource.DeleteResponse{State: updateResp.State}
	r.Delete(ctx, resource.DeleteRequest{State: updateResp.State}, &deleteResp)
	if deleteResp.Diagnostics.HasError() {
		t.Fatalf("delete: %v", deleteResp.Diagnostics)
	}
	if f.count("DELETE /hagrid/ssoconfig/tf-acc-sso") != 1 || f.count("GET /v3")-checksBefore < 2 {
		t.Fatal("delete did not DELETE and wait: a hagrid delete restarts Keystone too")
	}
}

// hagrid's create overwrites a configuration of the same name, which would
// replace a working SSO setup made in the PCD UI.
func TestSSOConfigurationCreateRefusesAnExistingConfiguration(t *testing.T) {
	fastSSOWait(t)
	ctx := context.Background()
	f := newHagridFake(t)
	f.stored = map[string]any{"domain_name": "tf-acc-sso", "provider": "okta", "entity_id": "e"}
	r := &ssoConfigurationResource{config: f.config()}
	s := federationSchemaOf(t, r)
	planned := ssoModel("<Attributes/>")
	resp := resource.CreateResponse{State: tfsdk.State{Schema: s, Raw: federationNull(ctx, s)}}
	r.Create(ctx, resource.CreateRequest{Plan: federationPlan(ctx, t, s, &planned)}, &resp)
	if !resp.Diagnostics.HasError() || !strings.Contains(resp.Diagnostics[0].Detail(), "terraform import") {
		t.Fatalf("diagnostics = %v, want the import named", resp.Diagnostics)
	}
	if f.count("POST /hagrid/ssoconfig") != 0 {
		t.Fatal("the existing configuration was overwritten")
	}
}

// The configuration exists once hagrid accepts it, whether or not Keystone
// comes back in time. A failed wait must leave it in state, where Terraform
// marks it tainted, instead of forgetting it.
func TestSSOConfigurationCreateKeepsTheConfigurationWhenTheWaitFails(t *testing.T) {
	fastSSOWait(t)
	ctx := context.Background()
	f := newHagridFake(t)
	f.keystoneDown = 1 << 30
	r := &ssoConfigurationResource{config: f.config()}
	s := federationSchemaOf(t, r)
	planned := ssoModel("<Attributes/>")
	resp := resource.CreateResponse{State: tfsdk.State{Schema: s, Raw: federationNull(ctx, s)}}
	r.Create(ctx, resource.CreateRequest{Plan: federationPlan(ctx, t, s, &planned)}, &resp)
	if !resp.Diagnostics.HasError() || !strings.Contains(resp.Diagnostics.Errors()[0].Detail(), "terraform untaint") {
		t.Fatalf("diagnostics = %v, want the failed wait reported with terraform untaint named", resp.Diagnostics)
	}
	var got ssoConfigurationModel
	resp.State.Get(ctx, &got)
	if resp.State.Raw.IsNull() || got.ID.ValueString() != "tf-acc-sso" || !resp.State.Raw.IsFullyKnown() {
		t.Fatalf("state after the failed wait: %v; want the configuration recorded with no unknowns", resp.State.Raw)
	}
}

// An update is in hagrid as soon as the POST succeeds. A wait that fails after
// it must leave the new values in state, or the next apply posts them again
// and restarts Keystone again.
func TestSSOConfigurationUpdateRecordsTheWriteWhenTheWaitFails(t *testing.T) {
	fastSSOWait(t)
	ctx := context.Background()
	f := newHagridFake(t)
	f.stored = map[string]any{"domain_name": "tf-acc-sso", "provider": "okta", "entity_id": "https://idp.example.com/entity",
		"metadata_url": "https://idp.example.com/metadata", "attr_map_xml": "<Attributes/>"}
	f.keystoneDown = 1 << 30
	r := &ssoConfigurationResource{config: f.config()}
	s := federationSchemaOf(t, r)
	prior := ssoModel("<Attributes/>")
	prior.ID = types.StringValue("tf-acc-sso")
	prior.LoginURL, prior.ACSURL, prior.SPEntityID = types.StringValue("l"), types.StringValue("a"), types.StringValue("e")
	planned := prior
	planned.AttributeMapXML = types.StringValue("<Attributes><Attribute name=\"mail\" id=\"mail\"/></Attributes>")
	resp := resource.UpdateResponse{State: federationState(ctx, t, s, &prior)}
	r.Update(ctx, resource.UpdateRequest{Plan: federationPlan(ctx, t, s, &planned), State: federationState(ctx, t, s, &prior)}, &resp)
	if !resp.Diagnostics.HasError() || !strings.Contains(resp.Diagnostics.Errors()[0].Detail(), "terraform apply") {
		t.Fatalf("diagnostics = %v, want the failed wait reported with terraform apply named", resp.Diagnostics)
	}
	var got ssoConfigurationModel
	resp.State.Get(ctx, &got)
	if got.AttributeMapXML.ValueString() != planned.AttributeMapXML.ValueString() {
		t.Fatalf("state holds %s after the failed wait, want the posted value", got.AttributeMapXML)
	}
}

// A delete whose restart wait times out still deleted the configuration, and
// everything that needed it is already destroyed: a warning, not an error.
func TestSSOConfigurationDeleteWarnsWhenTheWaitFails(t *testing.T) {
	fastSSOWait(t)
	ctx := context.Background()
	f := newHagridFake(t)
	f.stored = map[string]any{"domain_name": "tf-acc-sso", "entity_id": "e"}
	f.keystoneDown = 1 << 30
	r := &ssoConfigurationResource{config: f.config()}
	s := federationSchemaOf(t, r)
	prior := ssoModel("<Attributes/>")
	prior.ID = types.StringValue("tf-acc-sso")
	prior.LoginURL, prior.ACSURL, prior.SPEntityID = types.StringValue("l"), types.StringValue("a"), types.StringValue("e")
	resp := resource.DeleteResponse{State: federationState(ctx, t, s, &prior)}
	r.Delete(ctx, resource.DeleteRequest{State: federationState(ctx, t, s, &prior)}, &resp)
	if resp.Diagnostics.HasError() || resp.Diagnostics.WarningsCount() != 1 {
		t.Fatalf("diagnostics = %v, want one warning and no error", resp.Diagnostics)
	}
}

// Nothing configured: no DELETE (hagrid would answer 500) and no restart to
// wait for.
func TestSSOConfigurationDeleteOfAnAbsentConfiguration(t *testing.T) {
	fastSSOWait(t)
	ctx := context.Background()
	f := newHagridFake(t)
	r := &ssoConfigurationResource{config: f.config()}
	s := federationSchemaOf(t, r)
	prior := ssoModel("<Attributes/>")
	prior.ID = types.StringValue("tf-acc-sso")
	prior.LoginURL, prior.ACSURL, prior.SPEntityID = types.StringValue("x"), types.StringValue("x"), types.StringValue("x")
	resp := resource.DeleteResponse{State: federationState(ctx, t, s, &prior)}
	r.Delete(ctx, resource.DeleteRequest{State: federationState(ctx, t, s, &prior)}, &resp)
	if resp.Diagnostics.HasError() || f.count("DELETE /hagrid/ssoconfig/tf-acc-sso") != 0 || f.count("GET /v3") != 0 {
		t.Fatalf("diags %v; DELETE %d, keystone checks %d; want neither", resp.Diagnostics,
			f.count("DELETE /hagrid/ssoconfig/tf-acc-sso"), f.count("GET /v3"))
	}
}

func TestSSOConfigurationReadDropsAConfigurationThatIsGone(t *testing.T) {
	ctx := context.Background()
	for _, absent404 := range []bool{false, true} {
		f := newHagridFake(t)
		f.absent404 = absent404
		r := &ssoConfigurationResource{config: f.config()}
		s := federationSchemaOf(t, r)
		prior := ssoModel("<Attributes/>")
		prior.ID = types.StringValue("tf-acc-sso")
		prior.LoginURL, prior.ACSURL, prior.SPEntityID = types.StringValue("x"), types.StringValue("x"), types.StringValue("x")
		resp := resource.ReadResponse{State: federationState(ctx, t, s, &prior)}
		r.Read(ctx, resource.ReadRequest{State: federationState(ctx, t, s, &prior)}, &resp)
		if resp.Diagnostics.HasError() || !resp.State.Raw.IsNull() {
			t.Fatalf("absent404=%v: diags %v, null %v", absent404, resp.Diagnostics, resp.State.Raw.IsNull())
		}
	}
}

// A 200 without the configuration would read as empty strings, which plans
// an update nobody asked for, and that update restarts Keystone. Read refuses
// it and keeps the prior state.
func TestSSOConfigurationReadKeepsStateOnAnAnswerWithoutTheObject(t *testing.T) {
	for name, body := range map[string]string{
		"empty body":        `{}`,
		"null":              `null`,
		"without entity_id": `{"domain_name": "tf-acc-sso", "provider": "okta"}`,
	} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			f := newFederationFake(t, map[string][]federationReply{
				"GET /hagrid/ssoconfig/tf-acc-sso": {{http.StatusOK, body}},
			})
			r := &ssoConfigurationResource{config: f.config()}
			s := federationSchemaOf(t, r)
			prior := ssoModel("<Attributes/>")
			prior.ID = types.StringValue("tf-acc-sso")
			prior.LoginURL, prior.ACSURL, prior.SPEntityID = types.StringValue("l"), types.StringValue("a"), types.StringValue("e")
			resp := resource.ReadResponse{State: federationState(ctx, t, s, &prior)}
			r.Read(ctx, resource.ReadRequest{State: federationState(ctx, t, s, &prior)}, &resp)
			federationExpectNoObjectError(t, resp.Diagnostics)
			var got ssoConfigurationModel
			resp.State.Get(ctx, &got)
			if got != prior {
				t.Fatalf("state = %+v\nwant the prior state %+v", got, prior)
			}
		})
	}
}

// hagrid answers 401 when Keystone does not confirm the token. The provider
// re-authenticates once and retries, as it does by default, so the second 401
// arrives wrapped; the explanation must still be added.
func TestSSOConfigurationExplainsHagrid401(t *testing.T) {
	ctx := context.Background()
	f := newFederationFake(t, map[string][]federationReply{
		"GET /hagrid/ssoconfig/tf-acc-sso": {{http.StatusUnauthorized, ``}},
	})
	cfg := f.config()
	cfg.Provider.ReauthFunc = func(context.Context) error { return nil }
	r := &ssoConfigurationResource{config: cfg}
	s := federationSchemaOf(t, r)
	planned := ssoModel("<Attributes/>")
	resp := resource.CreateResponse{State: tfsdk.State{Schema: s, Raw: federationNull(ctx, s)}}
	r.Create(ctx, resource.CreateRequest{Plan: federationPlan(ctx, t, s, &planned)}, &resp)
	if !resp.Diagnostics.HasError() || !strings.Contains(resp.Diagnostics[0].Detail(), "could not validate") {
		t.Fatalf("diagnostics = %v, want the 401 explained", resp.Diagnostics)
	}
	if got := len(f.keys()); got != 2 {
		t.Fatalf("hagrid got %d requests, want 2: the request and its retry after re-authenticating", got)
	}
}

func TestSSOConfigurationImportState(t *testing.T) {
	ctx := context.Background()
	s := federationSchemaOf(t, &ssoConfigurationResource{})
	resp := resource.ImportStateResponse{State: tfsdk.State{Schema: s, Raw: federationNull(ctx, s)}}
	(&ssoConfigurationResource{}).ImportState(ctx, resource.ImportStateRequest{ID: "IDP1"}, &resp)
	var got ssoConfigurationModel
	resp.State.Get(ctx, &got)
	if resp.Diagnostics.HasError() || got.ID.ValueString() != "IDP1" || got.IdentityProvider.ValueString() != "IDP1" {
		t.Fatalf("import: diags %v, id %s, identity_provider %s", resp.Diagnostics, got.ID, got.IdentityProvider)
	}
}

// An empty ID, or one that is no identity provider name, names nothing hagrid
// could hold: it is refused before any request is sent, and no state is set.
func TestSSOConfigurationImportRefusesAnEmptyID(t *testing.T) {
	ctx := context.Background()
	for _, id := range []string{"", "a/b", "a b", "../x"} {
		f := newHagridFake(t)
		r := &ssoConfigurationResource{config: f.config()}
		s := federationSchemaOf(t, r)
		resp := resource.ImportStateResponse{State: tfsdk.State{Schema: s, Raw: federationNull(ctx, s)}}
		r.ImportState(ctx, resource.ImportStateRequest{ID: id}, &resp)
		var summaries []string
		for _, d := range resp.Diagnostics.Errors() {
			summaries = append(summaries, d.Summary())
		}
		if len(summaries) != 1 || summaries[0] != "Import needs an identity provider name" || !resp.State.Raw.IsNull() {
			t.Fatalf("ID %q: errors = %v, state null %v; want [Import needs an identity provider name] and no state",
				id, summaries, resp.State.Raw.IsNull())
		}
		if n := len(f.calls); n != 0 {
			t.Fatalf("ID %q: %d request(s) sent: %v", id, n, f.calls)
		}
	}
}

// ssoExpectState compares every attribute of an SSO configuration's state
// with what hagrid holds on the fake (the PCD UI's values in these tests).
func ssoExpectState(t *testing.T, f *hagridFake, got ssoConfigurationModel, metadataURL, attributeMap string) {
	t.Helper()
	federationExpectAttrs(t, map[string][2]attr.Value{
		"id":                {got.ID, types.StringValue("tf-acc-sso")},
		"identity_provider": {got.IdentityProvider, types.StringValue("tf-acc-sso")},
		"sso_provider":      {got.SSOProvider, types.StringValue("oneLogin")},
		"entity_id":         {got.EntityID, types.StringValue("https://other.example.com/entity")},
		"metadata_url":      {got.MetadataURL, types.StringValue(metadataURL)},
		"attribute_map_xml": {got.AttributeMapXML, types.StringValue(attributeMap)},
		"login_url":         {got.LoginURL, types.StringValue("/sso/tf-acc-sso/Shibboleth.sso/Login")},
		"acs_url":           {got.ACSURL, types.StringValue(f.srv.URL + "/sso/tf-acc-sso/Shibboleth.sso/SAML2/POST")},
		"sp_entity_id":      {got.SPEntityID, types.StringValue(f.srv.URL + "/keystone")},
	})
}

// Read copies hagrid's configuration into state: a metadata URL or attribute
// map changed in the PCD UI shows in the next plan, and an import, whose state
// holds only the name, gets every attribute (otherwise the next apply would
// post the configuration again and restart Keystone for nothing).
func TestSSOConfigurationReadTakesHagridsValues(t *testing.T) {
	const metadataURL, attributeMap = "https://other.example.com/metadata", `<Attributes><Attribute name="uid" id="uid"/></Attributes>`
	drift := ssoModel("<Attributes/>")
	drift.ID = types.StringValue("tf-acc-sso")
	drift.LoginURL, drift.ACSURL, drift.SPEntityID = types.StringValue("l"), types.StringValue("a"), types.StringValue("e")
	imported := ssoConfigurationModel{
		ID: types.StringValue("tf-acc-sso"), IdentityProvider: types.StringValue("tf-acc-sso"),
		SSOProvider: types.StringNull(), EntityID: types.StringNull(), MetadataURL: types.StringNull(),
		AttributeMapXML: types.StringNull(), LoginURL: types.StringNull(), ACSURL: types.StringNull(), SPEntityID: types.StringNull(),
	}
	for name, prior := range map[string]ssoConfigurationModel{"drift": drift, "import": imported} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			f := newHagridFake(t)
			f.stored = map[string]any{"domain_name": "tf-acc-sso", "provider": "oneLogin", "entity_id": "https://other.example.com/entity",
				"metadata_url": metadataURL, "attr_map_xml": attributeMap}
			r := &ssoConfigurationResource{config: f.config()}
			s := federationSchemaOf(t, r)
			resp := resource.ReadResponse{State: federationState(ctx, t, s, &prior)}
			r.Read(ctx, resource.ReadRequest{State: federationState(ctx, t, s, &prior)}, &resp)
			if resp.Diagnostics.HasError() {
				t.Fatalf("read: %v", resp.Diagnostics)
			}
			var got ssoConfigurationModel
			resp.State.Get(ctx, &got)
			ssoExpectState(t, f, got, metadataURL, attributeMap)
		})
	}
}

// After its wait, Update stores what hagrid reports, not just the plan: the
// computed values come from hagrid's answer, and so does a configured value
// that hagrid stores in its own form (here a reformatted attribute map).
func TestSSOConfigurationUpdateStoresHagridsAnswer(t *testing.T) {
	fastSSOWait(t)
	ctx := context.Background()
	const metadataURL, attributeMap = "https://other.example.com/metadata", `<Attributes><Attribute name="mail" id="mail"/></Attributes>`
	f := newHagridFake(t)
	f.stored = map[string]any{"domain_name": "tf-acc-sso", "provider": "oneLogin", "entity_id": "https://other.example.com/entity",
		"metadata_url": metadataURL, "attr_map_xml": "<Attributes/>"}
	r := &ssoConfigurationResource{config: f.config()}
	s := federationSchemaOf(t, r)
	prior := ssoModel("<Attributes/>")
	prior.ID, prior.SSOProvider = types.StringValue("tf-acc-sso"), types.StringValue("oneLogin")
	prior.EntityID, prior.MetadataURL = types.StringValue("https://other.example.com/entity"), types.StringValue(metadataURL)
	prior.LoginURL, prior.ACSURL, prior.SPEntityID = types.StringValue("l"), types.StringValue("a"), types.StringValue("e")
	planned := prior
	planned.AttributeMapXML = types.StringValue(attributeMap)
	const hagridsAttributeMap = "<Attributes>\n  <Attribute name=\"mail\" id=\"mail\"/>\n</Attributes>\n"
	f.answerOverride = map[string]any{"attr_map_xml": hagridsAttributeMap}
	resp := resource.UpdateResponse{State: federationState(ctx, t, s, &prior)}
	r.Update(ctx, resource.UpdateRequest{Plan: federationPlan(ctx, t, s, &planned), State: federationState(ctx, t, s, &prior)}, &resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("update: %v", resp.Diagnostics)
	}
	var got ssoConfigurationModel
	resp.State.Get(ctx, &got)
	ssoExpectState(t, f, got, metadataURL, hagridsAttributeMap)
}

// A read that fails after a wait that passed leaves the configuration saved,
// recorded and tainted, exactly as a wait that times out does, so the error
// gives the same untaint hint.
func TestSSOConfigurationCreateNamesTheUntaintWhenTheReadBackFails(t *testing.T) {
	fastSSOWait(t)
	ctx := context.Background()
	f := newFederationFake(t, map[string][]federationReply{
		"GET /v3":                {{http.StatusOK, `{"version": {"id": "v3.14", "status": "stable"}}`}},
		"POST /hagrid/ssoconfig": {{http.StatusOK, `{}`}},
		"GET /hagrid/ssoconfig/tf-acc-sso": {
			{http.StatusInternalServerError, `{"error": "failed to read SSO config", "msg": "no sso entry for domain "tf-acc-sso""}`},
			{http.StatusOK, `{"domain_name": "tf-acc-sso", "entity_id": "e"}`},
			{http.StatusOK, `{"domain_name": "tf-acc-sso", "entity_id": "e"}`},
			{http.StatusInternalServerError, `{"error": "failed to read SSO config", "msg": "Unexpected response code: 500"}`},
		},
	})
	r := &ssoConfigurationResource{config: f.config()}
	s := federationSchemaOf(t, r)
	planned := ssoModel("<Attributes/>")
	resp := resource.CreateResponse{State: tfsdk.State{Schema: s, Raw: federationNull(ctx, s)}}
	r.Create(ctx, resource.CreateRequest{Plan: federationPlan(ctx, t, s, &planned)}, &resp)
	if !resp.Diagnostics.HasError() || resp.Diagnostics.Errors()[0].Summary() != "hagrid: reading SSO configuration" ||
		!strings.HasSuffix(resp.Diagnostics.Errors()[0].Detail(), ssoCreateTaintedHint) {
		t.Fatalf("diagnostics = %v, want the failed read reported with the untaint hint", resp.Diagnostics)
	}
	var got ssoConfigurationModel
	resp.State.Get(ctx, &got)
	if resp.State.Raw.IsNull() || got.ID.ValueString() != "tf-acc-sso" {
		t.Fatalf("state after the failed read: %v; want the configuration recorded", resp.State.Raw)
	}
}

// hagrid refuses a configuration whose metadata it cannot download, and then
// nothing was written and nothing restarts: Create records nothing and Update
// keeps the prior values, and neither waits.
func TestSSOConfigurationRefusedWriteRecordsNothingAndDoesNotWait(t *testing.T) {
	fastSSOWait(t)
	t.Run("create", func(t *testing.T) {
		ctx := context.Background()
		f := newHagridFake(t)
		f.refusePost = true
		r := &ssoConfigurationResource{config: f.config()}
		s := federationSchemaOf(t, r)
		planned := ssoModel("<Attributes/>")
		resp := resource.CreateResponse{State: tfsdk.State{Schema: s, Raw: federationNull(ctx, s)}}
		r.Create(ctx, resource.CreateRequest{Plan: federationPlan(ctx, t, s, &planned)}, &resp)
		if !resp.Diagnostics.HasError() || !strings.Contains(resp.Diagnostics.Errors()[0].Detail(), "Incorrect metadata url") {
			t.Fatalf("diagnostics = %v, want hagrid's refusal", resp.Diagnostics)
		}
		if !resp.State.Raw.IsNull() || f.count("GET /v3") != 0 {
			t.Fatalf("state null %v, keystone checks %d; want no state and no wait", resp.State.Raw.IsNull(), f.count("GET /v3"))
		}
	})
	t.Run("update", func(t *testing.T) {
		ctx := context.Background()
		f := newHagridFake(t)
		f.stored = map[string]any{"domain_name": "tf-acc-sso", "provider": "okta", "entity_id": "https://idp.example.com/entity",
			"metadata_url": "https://idp.example.com/metadata", "attr_map_xml": "<Attributes/>"}
		f.refusePost = true
		r := &ssoConfigurationResource{config: f.config()}
		s := federationSchemaOf(t, r)
		prior := ssoModel("<Attributes/>")
		prior.ID = types.StringValue("tf-acc-sso")
		prior.LoginURL, prior.ACSURL, prior.SPEntityID = types.StringValue("l"), types.StringValue("a"), types.StringValue("e")
		planned := prior
		planned.MetadataURL = types.StringValue("https://unreachable.example.com/metadata")
		resp := resource.UpdateResponse{State: federationState(ctx, t, s, &prior)}
		r.Update(ctx, resource.UpdateRequest{Plan: federationPlan(ctx, t, s, &planned), State: federationState(ctx, t, s, &prior)}, &resp)
		if !resp.Diagnostics.HasError() || !strings.Contains(resp.Diagnostics.Errors()[0].Detail(), "Incorrect metadata url") {
			t.Fatalf("diagnostics = %v, want hagrid's refusal", resp.Diagnostics)
		}
		var got ssoConfigurationModel
		resp.State.Get(ctx, &got)
		if got != prior || f.count("GET /v3") != 0 {
			t.Fatalf("state = %+v, keystone checks %d; want the prior state %+v and no wait", got, f.count("GET /v3"), prior)
		}
	})
}

// The configuration was there when Delete looked, but hagrid reports it gone
// when the DELETE arrives (deleted in the PCD UI in between): the destroy
// succeeds.
func TestSSOConfigurationDeleteOfAConfigurationGoneInBetween(t *testing.T) {
	fastSSOWait(t)
	ctx := context.Background()
	f := newHagridFake(t)
	f.stored = map[string]any{"domain_name": "tf-acc-sso", "entity_id": "e"}
	f.goneOnDelete = true
	r := &ssoConfigurationResource{config: f.config()}
	s := federationSchemaOf(t, r)
	prior := ssoModel("<Attributes/>")
	prior.ID = types.StringValue("tf-acc-sso")
	prior.LoginURL, prior.ACSURL, prior.SPEntityID = types.StringValue("l"), types.StringValue("a"), types.StringValue("e")
	resp := resource.DeleteResponse{State: federationState(ctx, t, s, &prior)}
	r.Delete(ctx, resource.DeleteRequest{State: federationState(ctx, t, s, &prior)}, &resp)
	if resp.Diagnostics.HasError() || f.count("DELETE /hagrid/ssoconfig/tf-acc-sso") != 1 {
		t.Fatalf("diagnostics = %v, DELETE %d; want one DELETE and no error", resp.Diagnostics, f.count("DELETE /hagrid/ssoconfig/tf-acc-sso"))
	}
}
