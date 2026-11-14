// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package identity

import (
	"context"
	"fmt"
	"net/http"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/hashicorp/terraform-plugin-framework-validators/setvalidator"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/platform9/terraform-provider-pcd/internal/clients"
)

var (
	_ resource.Resource                = (*federationIdentityProviderResource)(nil)
	_ resource.ResourceWithConfigure   = (*federationIdentityProviderResource)(nil)
	_ resource.ResourceWithImportState = (*federationIdentityProviderResource)(nil)
)

// NewFederationIdentityProviderResource is the factory registered with the provider.
func NewFederationIdentityProviderResource() resource.Resource {
	return &federationIdentityProviderResource{}
}

type federationIdentityProviderResource struct {
	config *clients.Config
}

type federationIdentityProviderModel struct {
	ID          types.String `tfsdk:"id"`
	Name        types.String `tfsdk:"name"`
	DomainID    types.String `tfsdk:"domain_id"`
	RemoteIDs   types.Set    `tfsdk:"remote_ids"`
	Enabled     types.Bool   `tfsdk:"enabled"`
	Description types.String `tfsdk:"description"`
}

// federationIdentityProviderAPI is Keystone's identity_provider object.
// gophercloud v2.13.0 has no identity-provider API, so the calls are made
// directly on the identity client.
type federationIdentityProviderAPI struct {
	ID          string   `json:"id"`
	DomainID    string   `json:"domain_id"`
	Description *string  `json:"description"`
	Enabled     bool     `json:"enabled"`
	RemoteIDs   []string `json:"remote_ids"`
}

// federationIdentityProviderAnswer is the body of every answer that carries
// an identity provider.
type federationIdentityProviderAnswer struct {
	IdentityProvider federationIdentityProviderAPI `json:"identity_provider"`
}

// object returns the identity provider the answer holds. A success answer
// without it (no object, or one without an ID) is clients.ErrNoObject: read as
// it is, it would hold domain_id "", which plans a replacement, and destroying
// an identity provider also deletes its protocols.
func (a *federationIdentityProviderAnswer) object() (*federationIdentityProviderAPI, error) {
	if a.IdentityProvider.ID == "" {
		return nil, clients.ErrNoObject
	}
	return &a.IdentityProvider, nil
}

func federationIdentityProviderURL(client *gophercloud.ServiceClient, name string) string {
	return client.ServiceURL("OS-FEDERATION", "identity_providers", name)
}

// federationIdentityProviderCreateBody builds the PUT body. Keystone defaults
// enabled to false, so it is always sent.
func federationIdentityProviderCreateBody(ctx context.Context, m *federationIdentityProviderModel, diags *diag.Diagnostics) map[string]any {
	var remoteIDs []string
	diags.Append(m.RemoteIDs.ElementsAs(ctx, &remoteIDs, false)...)
	idp := map[string]any{
		"domain_id":  m.DomainID.ValueString(),
		"remote_ids": remoteIDs,
		"enabled":    m.Enabled.ValueBool(),
	}
	if !m.Description.IsNull() && !m.Description.IsUnknown() {
		idp["description"] = m.Description.ValueString()
	}
	return map[string]any{"identity_provider": idp}
}

// federationIdentityProviderUpdateBody builds the PATCH body from what changed.
// Keystone cannot move an identity provider to another domain (domain_id
// forces a new resource) and refuses a PATCH with nothing in it, so an empty
// body means no call.
func federationIdentityProviderUpdateBody(ctx context.Context, plan, state *federationIdentityProviderModel, diags *diag.Diagnostics) map[string]any {
	idp := map[string]any{}
	if !plan.RemoteIDs.Equal(state.RemoteIDs) {
		var remoteIDs []string
		diags.Append(plan.RemoteIDs.ElementsAs(ctx, &remoteIDs, false)...)
		idp["remote_ids"] = remoteIDs
	}
	if !plan.Enabled.Equal(state.Enabled) && !plan.Enabled.IsUnknown() {
		idp["enabled"] = plan.Enabled.ValueBool()
	}
	if !plan.Description.Equal(state.Description) && !plan.Description.IsNull() && !plan.Description.IsUnknown() {
		idp["description"] = plan.Description.ValueString()
	}
	if len(idp) == 0 {
		return nil
	}
	return map[string]any{"identity_provider": idp}
}

// setFromAPI copies Keystone's answer into m. The ID stays the name m already
// holds (from the plan, or from the import ID), so an answer without the
// object can never leave a state row with no ID.
func (m *federationIdentityProviderModel) setFromAPI(ctx context.Context, idp *federationIdentityProviderAPI, diags *diag.Diagnostics) {
	m.ID = types.StringValue(m.Name.ValueString())
	m.DomainID = types.StringValue(idp.DomainID)
	remoteIDs := idp.RemoteIDs
	if remoteIDs == nil {
		remoteIDs = []string{}
	}
	set, d := types.SetValueFrom(ctx, types.StringType, remoteIDs)
	diags.Append(d...)
	m.RemoteIDs = set
	m.Enabled = types.BoolValue(idp.Enabled)
	if idp.Description != nil {
		m.Description = types.StringValue(*idp.Description)
	} else {
		m.Description = types.StringValue("")
	}
}

func (r *federationIdentityProviderResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_identity_federation_identity_provider"
}

func (r *federationIdentityProviderResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	useState := []planmodifier.String{stringplanmodifier.UseStateForUnknown()}
	resp.Schema = schema.Schema{
		MarkdownDescription: "Manages a Keystone identity provider: the trusted SAML identity provider that " +
			"federated users sign in through. For PCD single sign-on, `name` must equal the " +
			"`identity_provider` of the `pcd_identity_sso_configuration` (PCD only routes SAML sign-in for an " +
			"identity provider that has an SSO configuration of the same name), and `remote_ids` must hold its " +
			"`entity_id`. Referencing the SSO configuration's attributes, as the SSO guide does, keeps the " +
			"three in step.\n\n" +
			"Destroying an identity provider also deletes its protocols. Users who signed in through it keep " +
			"their Keystone shadow users; the provider does not delete them.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "The identity provider name.",
				PlanModifiers:       useState,
			},
			"name": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "The identity provider's ID in Keystone, " + federationNameRule + ". The PCD UI " +
					"uses `IDP1` for the default domain. Changing this forces a new resource.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
				Validators:    []validator.String{stringvalidator.RegexMatches(federationNamePattern, federationNameRule)},
			},
			"domain_id": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "The domain federated users are created in (`default` for the default domain). " +
					"Required because Keystone creates a new, empty domain for an identity provider without one. " +
					"Changing this forces a new resource: Keystone cannot move an identity provider.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
				Validators:    []validator.String{stringvalidator.LengthAtLeast(1)},
			},
			"remote_ids": schema.SetAttribute{
				Required:    true,
				ElementType: types.StringType,
				MarkdownDescription: "The identity provider's entity IDs, as its SAML assertions carry them; for PCD " +
					"single sign-on, the SSO configuration's `entity_id`. Keystone refuses an entity ID that another " +
					"identity provider already lists. Updated in place.",
				Validators: []validator.Set{setvalidator.SizeAtLeast(1)},
			},
			"enabled": schema.BoolAttribute{
				Optional:            true,
				Computed:            true,
				Default:             booldefault.StaticBool(true),
				MarkdownDescription: "Whether users may sign in through the identity provider. Defaults to `true`. Updated in place.",
			},
			"description": schema.StringAttribute{
				Optional:            true,
				Computed:            true,
				MarkdownDescription: "A description. Updated in place.",
				PlanModifiers:       useState,
			},
		},
	}
}

func (r *federationIdentityProviderResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.config = configureClient(req.ProviderData, &resp.Diagnostics)
}

func (r *federationIdentityProviderResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan federationIdentityProviderModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	body := federationIdentityProviderCreateBody(ctx, &plan, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := r.config.IdentityV3Client()
	if err != nil {
		resp.Diagnostics.AddError("identity: building v3 client", err.Error())
		return
	}

	name := plan.Name.ValueString()
	var out federationIdentityProviderAnswer
	if _, err := client.Put(ctx, federationIdentityProviderURL(client, name), body, &out,
		&gophercloud.RequestOpts{OkCodes: []int{http.StatusCreated}}); err != nil {
		if gophercloud.ResponseCodeIs(err, http.StatusConflict) {
			resp.Diagnostics.AddError("identity: creating federation identity provider", fmt.Sprintf(
				"Keystone already has an identity provider named %s, or another identity provider already lists "+
					"one of these remote_ids. If `pcdctl identity provider list` shows %s, import it to manage it "+
					"here: terraform import <resource address> %s. Otherwise the identity provider that lists the "+
					"entity ID has another name: remove the entity ID from the other identity provider, or import "+
					"that one under its own name.", name, name, name))
			return
		}
		resp.Diagnostics.AddError("identity: creating federation identity provider", err.Error())
		return
	}
	idp, err := out.object()
	if err != nil {
		resp.Diagnostics.AddError("identity: creating federation identity provider", err.Error())
		return
	}

	plan.setFromAPI(ctx, idp, &resp.Diagnostics)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *federationIdentityProviderResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state federationIdentityProviderModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := r.config.IdentityV3Client()
	if err != nil {
		resp.Diagnostics.AddError("identity: building v3 client", err.Error())
		return
	}

	var out federationIdentityProviderAnswer
	if _, err := client.Get(ctx, federationIdentityProviderURL(client, state.Name.ValueString()), &out, nil); err != nil {
		if gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
			resp.State.RemoveResource(ctx)
			return
		}
		resp.Diagnostics.AddError("identity: reading federation identity provider", err.Error())
		return
	}
	idp, err := out.object()
	if err != nil {
		resp.Diagnostics.AddError("identity: reading federation identity provider", err.Error())
		return
	}

	state.setFromAPI(ctx, idp, &resp.Diagnostics)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *federationIdentityProviderResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state federationIdentityProviderModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	body := federationIdentityProviderUpdateBody(ctx, &plan, &state, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}
	if body == nil {
		resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
		return
	}

	client, err := r.config.IdentityV3Client()
	if err != nil {
		resp.Diagnostics.AddError("identity: building v3 client", err.Error())
		return
	}

	var out federationIdentityProviderAnswer
	if _, err := client.Patch(ctx, federationIdentityProviderURL(client, plan.Name.ValueString()), body, &out,
		&gophercloud.RequestOpts{OkCodes: []int{http.StatusOK}}); err != nil {
		resp.Diagnostics.AddError("identity: updating federation identity provider", err.Error())
		return
	}
	idp, err := out.object()
	if err != nil {
		resp.Diagnostics.AddError("identity: updating federation identity provider", err.Error())
		return
	}

	plan.setFromAPI(ctx, idp, &resp.Diagnostics)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *federationIdentityProviderResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state federationIdentityProviderModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := r.config.IdentityV3Client()
	if err != nil {
		resp.Diagnostics.AddError("identity: building v3 client", err.Error())
		return
	}

	if _, err := client.Delete(ctx, federationIdentityProviderURL(client, state.Name.ValueString()),
		&gophercloud.RequestOpts{OkCodes: []int{http.StatusNoContent}}); err != nil {
		if gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
			return
		}
		resp.Diagnostics.AddError("identity: deleting federation identity provider", err.Error())
	}
}

// ImportState takes the identity provider name. An empty ID names no
// identity provider, so it is refused before anything is read.
func (r *federationIdentityProviderResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	if req.ID == "" {
		resp.Diagnostics.AddError("Import needs a federation identity provider ID",
			"Pass the name of the identity provider to import, which `pcdctl identity provider list` shows.")
		return
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), req.ID)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("name"), req.ID)...)
}
