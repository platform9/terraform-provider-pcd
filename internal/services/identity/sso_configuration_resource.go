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
	"net/url"
	"strings"
	"time"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/platform9/terraform-provider-pcd/internal/clients"
	"github.com/platform9/terraform-provider-pcd/internal/tfstate"
)

var (
	_ resource.Resource                = (*ssoConfigurationResource)(nil)
	_ resource.ResourceWithConfigure   = (*ssoConfigurationResource)(nil)
	_ resource.ResourceWithImportState = (*ssoConfigurationResource)(nil)
)

// After a hagrid write, PCD rewrites the Shibboleth configuration and restarts
// Keystone's Apache and shibd. The restart starts within seconds of the write
// (up to ten seconds on releases that poll for the change), and the outage
// has been measured at about half a minute; the PCD UI polls for up to five. These
// are vars so unit tests can shorten them.
var (
	// ssoRestartInitialDelay lets the restart begin, so the wait never takes
	// the services' answers from before the restart for after it.
	ssoRestartInitialDelay = 30 * time.Second
	ssoRestartPollInterval = 10 * time.Second
	ssoRestartTimeout      = 10 * time.Minute
	// ssoRestartCheckTimeout bounds one check, so a request that hangs while
	// Keystone restarts cannot hold the wait past its deadline.
	ssoRestartCheckTimeout = 30 * time.Second
)

// ssoRestartHealthyPolls is how many answers in a row count as recovered.
const ssoRestartHealthyPolls = 2

// NewSSOConfigurationResource is the factory registered with the provider.
func NewSSOConfigurationResource() resource.Resource {
	return &ssoConfigurationResource{}
}

type ssoConfigurationResource struct {
	config *clients.Config
}

type ssoConfigurationModel struct {
	ID               types.String `tfsdk:"id"`
	IdentityProvider types.String `tfsdk:"identity_provider"`
	SSOProvider      types.String `tfsdk:"sso_provider"`
	EntityID         types.String `tfsdk:"entity_id"`
	MetadataURL      types.String `tfsdk:"metadata_url"`
	AttributeMapXML  types.String `tfsdk:"attribute_map_xml"`
	LoginURL         types.String `tfsdk:"login_url"`
	ACSURL           types.String `tfsdk:"acs_url"`
	SPEntityID       types.String `tfsdk:"sp_entity_id"`
}

// ssoConfigurationAPI is hagrid's SSO configuration. The GET response also
// carries `metadata`, the document hagrid downloaded from metadata_url, which
// changes whenever the identity provider rotates a certificate, and
// `signing`; neither is managed.
type ssoConfigurationAPI struct {
	DomainName      string `json:"domain_name"`
	Provider        string `json:"provider"`
	EntityID        string `json:"entity_id"`
	MetadataURL     string `json:"metadata_url"`
	AttributeMapXML string `json:"attr_map_xml"`
	LoginURL        string `json:"login_url,omitempty"`
}

func ssoConfigurationURL(hagrid *gophercloud.ServiceClient, idp string) string {
	return hagrid.ServiceURL("ssoconfig", idp)
}

// ssoConfigurationBody builds the POST body, which creates and updates alike:
// hagrid keys the configuration by domain_name and overwrites it whole.
func ssoConfigurationBody(m *ssoConfigurationModel) ssoConfigurationAPI {
	return ssoConfigurationAPI{
		DomainName:      m.IdentityProvider.ValueString(),
		Provider:        m.SSOProvider.ValueString(),
		EntityID:        m.EntityID.ValueString(),
		MetadataURL:     m.MetadataURL.ValueString(),
		AttributeMapXML: m.AttributeMapXML.ValueString(),
	}
}

// ssoServiceProviderURLs returns the SAML service-provider entity ID and
// assertion consumer service URL that PCD's Shibboleth serves for idp: the
// values the identity provider's side needs. They live on hagrid's host.
func ssoServiceProviderURLs(hagrid *gophercloud.ServiceClient, idp string) (entityID, acs string) {
	u, err := url.Parse(hagrid.Endpoint)
	if err != nil || u.Host == "" {
		return "", ""
	}
	origin := u.Scheme + "://" + u.Host
	return origin + "/keystone", origin + "/sso/" + idp + "/Shibboleth.sso/SAML2/POST"
}

// errSSONotConfigured is getSSOConfiguration's answer when hagrid holds no SSO
// configuration under the name.
var errSSONotConfigured = errors.New("hagrid holds no SSO configuration under this name")

// getSSOConfiguration reads the SSO configuration for idp. hagrid answers a
// name with nothing configured with a 404, or, in current PCD releases, with a
// 500 whose message says "no sso entry for domain"; both come back as
// errSSONotConfigured. The request accepts both codes rather than letting
// gophercloud turn them into errors, so max_retries, which retries a GET that
// draws a 5xx, does not retry that answer. Any other 500 is returned as an
// error that carries hagrid's message, and a 200 without the configuration is
// clients.ErrNoObject.
func getSSOConfiguration(ctx context.Context, hagrid *gophercloud.ServiceClient, idp string) (*ssoConfigurationAPI, error) {
	u := ssoConfigurationURL(hagrid, idp)
	resp, err := hagrid.Get(ctx, u, nil, &gophercloud.RequestOpts{
		OkCodes:          []int{http.StatusOK, http.StatusNotFound, http.StatusInternalServerError},
		KeepResponseBody: true,
	})
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading hagrid's answer for %s: %w", idp, err)
	}
	switch {
	case resp.StatusCode == http.StatusOK:
		var out ssoConfigurationAPI
		if err := json.Unmarshal(body, &out); err != nil {
			return nil, fmt.Errorf("decoding hagrid's SSO configuration for %s: %w", idp, err)
		}
		// Every configuration holds an entity ID: it is always posted, and
		// never empty. A 200 without one carries no configuration.
		if out.EntityID == "" {
			return nil, fmt.Errorf("hagrid answered %s without the configuration: %w", idp, clients.ErrNoObject)
		}
		return &out, nil
	case resp.StatusCode == http.StatusNotFound, strings.Contains(string(body), "no sso entry for domain"):
		return nil, errSSONotConfigured
	default:
		return nil, gophercloud.ErrUnexpectedResponseCode{
			URL: u, Method: http.MethodGet, Expected: []int{http.StatusOK}, Actual: resp.StatusCode, Body: body,
		}
	}
}

func postSSOConfiguration(ctx context.Context, hagrid *gophercloud.ServiceClient, body ssoConfigurationAPI) error {
	_, err := hagrid.Post(ctx, hagrid.ServiceURL("ssoconfig"), body, nil, &gophercloud.RequestOpts{OkCodes: []int{200}})
	return err
}

// ssoRequestError passes hagrid's error through as text (its error bodies are
// not valid JSON) and explains a 401: hagrid could not validate the token with
// Keystone. gophercloud re-authenticates once on a 401 and wraps a second 401
// in ErrErrorAfterReauthentication, which ResponseCodeIs does not unwrap.
func ssoRequestError(err error) string {
	unauthorized := gophercloud.ResponseCodeIs(err, http.StatusUnauthorized)
	var after *gophercloud.ErrErrorAfterReauthentication
	if errors.As(err, &after) {
		unauthorized = unauthorized || gophercloud.ResponseCodeIs(after.ErrOriginal, http.StatusUnauthorized)
	}
	if unauthorized {
		return "hagrid could not validate the provider's token with Keystone (401), even after re-authenticating. " + err.Error()
	}
	return err.Error()
}

// ssoAlreadyGone reports whether a DELETE failed only because nothing is
// configured, for instance when someone deleted the configuration between the
// read and the delete: hagrid answers that with a 404, or a 500 whose message
// says "ssoenabled not found" or "no sso entry for domain".
func ssoAlreadyGone(err error) bool {
	if gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
		return true
	}
	if !gophercloud.ResponseCodeIs(err, http.StatusInternalServerError) {
		return false
	}
	return strings.Contains(err.Error(), "ssoenabled not found") || strings.Contains(err.Error(), "no sso entry for domain")
}

// ssoServicesAnswer reports whether Keystone and hagrid both answer: Keystone's
// version document, and the SSO configuration for idp, which hagrid only
// serves after Keystone has validated the token. hagrid's "nothing configured"
// (as after a delete; see getSSOConfiguration) counts as an answer.
func ssoServicesAnswer(ctx context.Context, identity, hagrid *gophercloud.ServiceClient, idp string) error {
	ctx, cancel := context.WithTimeout(ctx, ssoRestartCheckTimeout)
	defer cancel()
	if _, err := identity.Get(ctx, strings.TrimSuffix(identity.Endpoint, "/"), nil, &gophercloud.RequestOpts{OkCodes: []int{200}}); err != nil {
		return fmt.Errorf("keystone: %w", err)
	}
	if _, err := getSSOConfiguration(ctx, hagrid, idp); err != nil && !errors.Is(err, errSSONotConfigured) {
		return fmt.Errorf("hagrid: %w", err)
	}
	return nil
}

// waitForSSOServices blocks until Keystone and hagrid are back after a hagrid
// write. It first waits ssoRestartInitialDelay, so that the restart has begun;
// then it polls until both answer ssoRestartHealthyPolls times in a row. A
// write that restarts nothing ends the wait after the delay and two polls.
//
// Its errors name the identity provider and the last check; the callers add
// what the failed wait means for the resource.
func waitForSSOServices(ctx context.Context, identity, hagrid *gophercloud.ServiceClient, idp string) error {
	last := errSSONoCheckYet
	if err := ssoSleep(ctx, ssoRestartInitialDelay); err != nil {
		return ssoWaitInterrupted(idp, last, err)
	}
	deadline := time.Now().Add(ssoRestartTimeout)
	healthy := 0
	for {
		last = ssoServicesAnswer(ctx, identity, hagrid, idp)
		if last == nil {
			healthy++
			if healthy >= ssoRestartHealthyPolls {
				return nil
			}
		} else {
			healthy = 0
		}
		if time.Now().After(deadline) {
			if last == nil {
				last = fmt.Errorf("they had answered only %d time(s) in a row", healthy)
			}
			return fmt.Errorf("keystone and hagrid did not come back within %s of the SSO change for %s. "+
				"Last check: %v", ssoRestartInitialDelay+ssoRestartTimeout, idp, last)
		}
		if err := ssoSleep(ctx, ssoRestartPollInterval); err != nil {
			if last == nil {
				last = fmt.Errorf("both answered, %d time(s) in a row", healthy)
			}
			return ssoWaitInterrupted(idp, last, err)
		}
	}
}

// errSSONoCheckYet is the last check a wait reports before its first check.
var errSSONoCheckYet = errors.New("none yet")

// ssoWaitInterrupted names what the wait was for when its context ends, so the
// error is more than "context canceled".
func ssoWaitInterrupted(idp string, last, err error) error {
	return fmt.Errorf("waiting for keystone and hagrid after the SSO change for %s was interrupted (last check: %v): %w",
		idp, last, err)
}

func ssoSleep(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// ssoClients builds the hagrid client and the identity client the wait polls.
func (r *ssoConfigurationResource) ssoClients() (hagrid, identity *gophercloud.ServiceClient, err error) {
	hagrid, err = r.config.HagridClient()
	if err != nil {
		return nil, nil, err
	}
	identity, err = r.config.IdentityV3Client()
	if err != nil {
		return nil, nil, err
	}
	return hagrid, identity, nil
}

func (m *ssoConfigurationModel) setFromAPI(hagrid *gophercloud.ServiceClient, sso *ssoConfigurationAPI) {
	idp := m.IdentityProvider.ValueString()
	m.ID = types.StringValue(idp)
	m.SSOProvider = types.StringValue(sso.Provider)
	m.EntityID = types.StringValue(sso.EntityID)
	m.MetadataURL = types.StringValue(sso.MetadataURL)
	m.AttributeMapXML = types.StringValue(sso.AttributeMapXML)
	m.LoginURL = types.StringValue(sso.LoginURL)
	spEntityID, acs := ssoServiceProviderURLs(hagrid, idp)
	m.SPEntityID = types.StringValue(spEntityID)
	m.ACSURL = types.StringValue(acs)
}

func (r *ssoConfigurationResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_identity_sso_configuration"
}

func (r *ssoConfigurationResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	useState := []planmodifier.String{stringplanmodifier.UseStateForUnknown()}
	required := []validator.String{stringvalidator.LengthAtLeast(1)}
	resp.Schema = schema.Schema{
		MarkdownDescription: "Manages PCD's SAML single sign-on configuration for one identity provider: the " +
			"PCD UI's **SAML Config** under Single Sign-On. It lives in hagrid, PCD's SSO configuration service, " +
			"not in Keystone. Single sign-on also needs a `pcd_identity_federation_identity_provider` with the " +
			"same name, a `pcd_identity_federation_mapping`, and a `saml2` `pcd_identity_federation_protocol`; " +
			"the SSO guide wires all four.\n\n" +
			"~> Creating, changing or destroying the configuration restarts Keystone and the Shibboleth daemon, " +
			"which makes the PCD API unavailable for about half a minute, sometimes longer (the PCD UI waits up " +
			"to five minutes). The resource waits after every write until Keystone and hagrid answer " +
			"again, so resources that depend on it run afterwards; resources that do not depend on it may run " +
			"during a restart and fail.\n\n" +
			"hagrid is not in the service catalog. The provider reaches it on the host of the catalog's identity " +
			"endpoint for the `Infra` region (or the provider's region, if the catalog has none for `Infra`), at " +
			"`/hagrid/`. If that host is not reachable from where Terraform runs, set `hagrid` in the " +
			"provider's `endpoint_overrides` to hagrid's URL, for example `https://pcd.example.com/hagrid/`. " +
			"Destroying the configuration does not delete the users who signed in through it.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "The identity provider name.",
				PlanModifiers:       useState,
			},
			"identity_provider": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "The identity provider's name, " + federationNameRule + ". The PCD UI uses " +
					"`IDP1` for the default domain and, for another domain, the domain name with every character " +
					"other than a letter, digit, `-` or `_` replaced by `_`. The " +
					"`pcd_identity_federation_identity_provider` must have this name. Changing this forces a new " +
					"resource.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
				Validators:    []validator.String{stringvalidator.RegexMatches(federationNamePattern, federationNameRule)},
			},
			"sso_provider": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "Which identity provider product this is, which the PCD UI uses to show the " +
					"right setup help: `okta`, `oneLogin`, `google`, or any other name. Updated in place, which " +
					"restarts Keystone's web server and the Shibboleth daemon like any other change.",
				Validators: required,
			},
			"entity_id": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "The identity provider's SAML entity ID. Put it in the identity provider " +
					"resource's `remote_ids`. Updated in place, which restarts Keystone's web server and the " +
					"Shibboleth daemon (the PCD UI does not allow changing it once saved).",
				Validators: required,
			},
			"metadata_url": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "The URL of the identity provider's SAML metadata. PCD downloads it when the " +
					"configuration is saved, from its own network and without verifying the certificate, so it must " +
					"be reachable from PCD; an unreachable URL fails the apply. Anyone who can intercept that " +
					"download can therefore hand PCD another identity provider's metadata, so use a URL on a network " +
					"you trust. Updated in place, which restarts Keystone's web server and the Shibboleth daemon.",
				Validators: required,
			},
			"attribute_map_xml": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "The Shibboleth attribute map (an `<Attributes>` XML document) that names " +
					"the SAML attributes the mapping rules match. Read it from a file with `file()`. Updated in " +
					"place, which restarts Keystone's web server and the Shibboleth daemon.",
				Validators: required,
			},
			"login_url": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "The path on PCD's host that starts a sign-in through this identity provider.",
				PlanModifiers:       useState,
			},
			"acs_url": schema.StringAttribute{
				Computed: true,
				MarkdownDescription: "The assertion consumer service (reply) URL to register with the identity " +
					"provider.",
				PlanModifiers: useState,
			},
			"sp_entity_id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "PCD's own SAML entity ID (identifier), to register with the identity provider.",
				PlanModifiers:       useState,
			},
		},
	}
}

func (r *ssoConfigurationResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.config = configureClient(req.ProviderData, &resp.Diagnostics)
}

func (r *ssoConfigurationResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan ssoConfigurationModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	hagrid, identity, err := r.ssoClients()
	if err != nil {
		resp.Diagnostics.AddError("hagrid: building clients", err.Error())
		return
	}
	idp := plan.IdentityProvider.ValueString()

	// hagrid's create overwrites whatever is configured under the name, so a
	// configuration made in the PCD UI would be replaced without a word.
	if _, err := getSSOConfiguration(ctx, hagrid, idp); err == nil {
		resp.Diagnostics.AddError("hagrid: creating SSO configuration", fmt.Sprintf(
			"Identity provider %s already has an SSO configuration (made in the PCD UI, for example). Import it "+
				"to manage it here: terraform import <resource address> %s", idp, idp))
		return
	} else if !errors.Is(err, errSSONotConfigured) {
		resp.Diagnostics.AddError("hagrid: checking for an SSO configuration", ssoRequestError(err))
		return
	}

	if err := postSSOConfiguration(ctx, hagrid, ssoConfigurationBody(&plan)); err != nil {
		resp.Diagnostics.AddError("hagrid: creating SSO configuration", ssoRequestError(err))
		return
	}
	plan.ID = types.StringValue(idp)
	if !tfstate.RecordCreated(ctx, resp, &plan) {
		return
	}

	if err := waitForSSOServices(ctx, identity, hagrid, idp); err != nil {
		resp.Diagnostics.AddError("hagrid: waiting for Keystone to restart", err.Error()+ssoCreateTaintedHint)
		return
	}
	sso, err := getSSOConfiguration(ctx, hagrid, idp)
	if err != nil {
		resp.Diagnostics.AddError("hagrid: reading SSO configuration", ssoRequestError(err)+ssoCreateTaintedHint)
		return
	}
	plan.setFromAPI(hagrid, sso)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

// ssoCreateTaintedHint ends every Create error after the configuration was
// saved and recorded: Terraform taints the row, and replacing it would restart
// Keystone twice more.
const ssoCreateTaintedHint = "\n\n" +
	"The SSO configuration was saved in hagrid, and Terraform records it as tainted. Once PCD answers " +
	"again, run `terraform untaint <resource address>` and then `terraform apply` to create what " +
	"depends on it. Without the untaint, the next apply replaces the configuration, which restarts " +
	"Keystone twice more."

func (r *ssoConfigurationResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state ssoConfigurationModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	hagrid, err := r.config.HagridClient()
	if err != nil {
		resp.Diagnostics.AddError("hagrid: building client", err.Error())
		return
	}

	sso, err := getSSOConfiguration(ctx, hagrid, state.IdentityProvider.ValueString())
	if err != nil {
		if errors.Is(err, errSSONotConfigured) {
			resp.State.RemoveResource(ctx)
			return
		}
		resp.Diagnostics.AddError("hagrid: reading SSO configuration", ssoRequestError(err))
		return
	}
	state.setFromAPI(hagrid, sso)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *ssoConfigurationResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan ssoConfigurationModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	hagrid, identity, err := r.ssoClients()
	if err != nil {
		resp.Diagnostics.AddError("hagrid: building clients", err.Error())
		return
	}
	idp := plan.IdentityProvider.ValueString()

	if err := postSSOConfiguration(ctx, hagrid, ssoConfigurationBody(&plan)); err != nil {
		resp.Diagnostics.AddError("hagrid: updating SSO configuration", ssoRequestError(err))
		return
	}
	// hagrid holds the new values from here on, so state does too, even if
	// the wait fails: otherwise the next apply would post them again and
	// restart Keystone once more.
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if err := waitForSSOServices(ctx, identity, hagrid, idp); err != nil {
		resp.Diagnostics.AddError("hagrid: waiting for Keystone to restart", err.Error()+"\n\n"+
			"The new values are saved in hagrid and in Terraform's state, so the next plan shows no change for "+
			"this SSO configuration. Once PCD answers again, run `terraform apply` to create what depends on it.")
		return
	}
	sso, err := getSSOConfiguration(ctx, hagrid, idp)
	if err != nil {
		resp.Diagnostics.AddError("hagrid: reading SSO configuration", ssoRequestError(err))
		return
	}
	plan.setFromAPI(hagrid, sso)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *ssoConfigurationResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state ssoConfigurationModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	hagrid, identity, err := r.ssoClients()
	if err != nil {
		resp.Diagnostics.AddError("hagrid: building clients", err.Error())
		return
	}
	idp := state.IdentityProvider.ValueString()

	// hagrid answers a DELETE for a name with nothing configured with a 500,
	// so look first. Nothing configured means nothing restarts either.
	if _, err := getSSOConfiguration(ctx, hagrid, idp); err != nil {
		if errors.Is(err, errSSONotConfigured) {
			return
		}
		resp.Diagnostics.AddError("hagrid: reading SSO configuration", ssoRequestError(err))
		return
	}
	if _, err := hagrid.Delete(ctx, ssoConfigurationURL(hagrid, idp), &gophercloud.RequestOpts{OkCodes: []int{200}}); err != nil {
		if ssoAlreadyGone(err) {
			return
		}
		resp.Diagnostics.AddError("hagrid: deleting SSO configuration", ssoRequestError(err))
		return
	}
	// The configuration is gone whatever the wait finds, and everything that
	// depended on it was destroyed first, so a slow restart is a warning.
	if err := waitForSSOServices(ctx, identity, hagrid, idp); err != nil {
		resp.Diagnostics.AddWarning("hagrid: SSO configuration deleted, but Keystone has not come back yet", err.Error()+"\n\n"+
			"The SSO configuration is deleted and gone from Terraform's state; nothing more needs doing for it.")
	}
}

// ImportState takes the identity provider name. An empty ID, or one that is no
// identity provider name, names no configuration, so it is refused before
// anything is read.
func (r *ssoConfigurationResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	if !federationNamePattern.MatchString(req.ID) {
		resp.Diagnostics.AddError("Import needs an identity provider name", fmt.Sprintf(
			"Pass the identity provider's name, %s, such as `IDP1` for the default domain; got %q. "+
				"`pcdctl identity provider list` shows the identity providers.", federationNameRule, req.ID))
		return
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), req.ID)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("identity_provider"), req.ID)...)
}
