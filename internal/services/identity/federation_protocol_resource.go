// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package identity

import (
	"context"
	"fmt"
	"net/http"
	"strings"

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
)

var (
	_ resource.Resource                = (*federationProtocolResource)(nil)
	_ resource.ResourceWithConfigure   = (*federationProtocolResource)(nil)
	_ resource.ResourceWithImportState = (*federationProtocolResource)(nil)
)

// NewFederationProtocolResource is the factory registered with the provider.
func NewFederationProtocolResource() resource.Resource {
	return &federationProtocolResource{}
}

type federationProtocolResource struct {
	config *clients.Config
}

type federationProtocolModel struct {
	ID               types.String `tfsdk:"id"`
	IdentityProvider types.String `tfsdk:"identity_provider"`
	Name             types.String `tfsdk:"name"`
	MappingID        types.String `tfsdk:"mapping_id"`
}

// federationProtocolAPI is Keystone's protocol object. gophercloud v2.13.0
// has no protocol API, so the calls are made directly on the identity client.
type federationProtocolAPI struct {
	ID        string `json:"id"`
	MappingID string `json:"mapping_id"`
}

// federationProtocolAnswer is the body of an answer that carries a protocol.
type federationProtocolAnswer struct {
	Protocol federationProtocolAPI `json:"protocol"`
}

// object returns the protocol the answer holds. A success answer without it
// (no object, or one without an ID or a mapping) is clients.ErrNoObject: read
// as it is, it would hold mapping_id "", which plans an update nobody asked for.
func (a *federationProtocolAnswer) object() (*federationProtocolAPI, error) {
	if a.Protocol.ID == "" || a.Protocol.MappingID == "" {
		return nil, clients.ErrNoObject
	}
	return &a.Protocol, nil
}

func federationProtocolURL(client *gophercloud.ServiceClient, idp, name string) string {
	return client.ServiceURL("OS-FEDERATION", "identity_providers", idp, "protocols", name)
}

func federationProtocolBody(mappingID string) map[string]any {
	return map[string]any{"protocol": map[string]any{"mapping_id": mappingID}}
}

func federationProtocolID(idp, name string) string {
	return idp + "/" + name
}

func (r *federationProtocolResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_identity_federation_protocol"
}

func (r *federationProtocolResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	nameValidators := []validator.String{stringvalidator.RegexMatches(federationNamePattern, federationNameRule)}
	resp.Schema = schema.Schema{
		MarkdownDescription: "Manages a Keystone federation protocol: the link between an identity provider and " +
			"the mapping that turns its assertions into local users and groups. PCD single sign-on signs users " +
			"in through a protocol named `saml2`, and no other: without it, sign-in fails even when the identity " +
			"provider and the SSO configuration are right. The PCD UI creates it when the first SAML group is " +
			"saved.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "`<identity_provider>/<name>`.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"identity_provider": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "The identity provider's name. Changing this forces a new resource.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.RequiresReplace()},
				Validators:          nameValidators,
			},
			"name": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "The protocol's ID. PCD single sign-on uses `saml2`. Changing this forces a new resource.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.RequiresReplace()},
				Validators:          nameValidators,
			},
			"mapping_id": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "The name of the mapping the protocol applies. Keystone refuses a mapping that " +
					"does not exist, so reference a `pcd_identity_federation_mapping`. Updated in place.",
				Validators: nameValidators,
			},
		},
	}
}

func (r *federationProtocolResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.config = configureClient(req.ProviderData, &resp.Diagnostics)
}

func (r *federationProtocolResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan federationProtocolModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := r.config.IdentityV3Client()
	if err != nil {
		resp.Diagnostics.AddError("identity: building v3 client", err.Error())
		return
	}

	idp, name := plan.IdentityProvider.ValueString(), plan.Name.ValueString()
	if _, err := client.Put(ctx, federationProtocolURL(client, idp, name), federationProtocolBody(plan.MappingID.ValueString()), nil,
		&gophercloud.RequestOpts{OkCodes: []int{http.StatusCreated}}); err != nil {
		if gophercloud.ResponseCodeIs(err, http.StatusConflict) {
			id := federationProtocolID(idp, name)
			resp.Diagnostics.AddError("identity: creating federation protocol", fmt.Sprintf(
				"Identity provider %s already has a %s protocol. Import it to manage it here: terraform import "+
					"<resource address> %s", idp, name, id))
			return
		}
		resp.Diagnostics.AddError("identity: creating federation protocol", err.Error())
		return
	}

	plan.ID = types.StringValue(federationProtocolID(idp, name))
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *federationProtocolResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state federationProtocolModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := r.config.IdentityV3Client()
	if err != nil {
		resp.Diagnostics.AddError("identity: building v3 client", err.Error())
		return
	}

	idp, name := state.IdentityProvider.ValueString(), state.Name.ValueString()
	var out federationProtocolAnswer
	if _, err := client.Get(ctx, federationProtocolURL(client, idp, name), &out, nil); err != nil {
		// Gone with its identity provider, too: Keystone deletes an identity
		// provider's protocols with it.
		if gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
			resp.State.RemoveResource(ctx)
			return
		}
		resp.Diagnostics.AddError("identity: reading federation protocol", err.Error())
		return
	}
	protocol, err := out.object()
	if err != nil {
		resp.Diagnostics.AddError("identity: reading federation protocol", err.Error())
		return
	}

	state.ID = types.StringValue(federationProtocolID(idp, name))
	state.MappingID = types.StringValue(protocol.MappingID)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *federationProtocolResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan federationProtocolModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := r.config.IdentityV3Client()
	if err != nil {
		resp.Diagnostics.AddError("identity: building v3 client", err.Error())
		return
	}

	idp, name := plan.IdentityProvider.ValueString(), plan.Name.ValueString()
	if _, err := client.Patch(ctx, federationProtocolURL(client, idp, name), federationProtocolBody(plan.MappingID.ValueString()), nil,
		&gophercloud.RequestOpts{OkCodes: []int{http.StatusOK}}); err != nil {
		resp.Diagnostics.AddError("identity: updating federation protocol", err.Error())
		return
	}

	plan.ID = types.StringValue(federationProtocolID(idp, name))
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *federationProtocolResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state federationProtocolModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := r.config.IdentityV3Client()
	if err != nil {
		resp.Diagnostics.AddError("identity: building v3 client", err.Error())
		return
	}

	url := federationProtocolURL(client, state.IdentityProvider.ValueString(), state.Name.ValueString())
	if _, err := client.Delete(ctx, url, &gophercloud.RequestOpts{OkCodes: []int{http.StatusNoContent}}); err != nil {
		if gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
			return
		}
		resp.Diagnostics.AddError("identity: deleting federation protocol", err.Error())
	}
}

// ImportState takes `<identity_provider>/<name>`. An ID without both parts
// names no protocol, so it is refused before anything is read.
func (r *federationProtocolResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	parts := strings.Split(req.ID, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		resp.Diagnostics.AddError("Import needs a federation protocol ID", fmt.Sprintf(
			"Pass `<identity_provider>/<name>`, such as `IDP1/saml2`; got %q. `pcdctl federation protocol list "+
				"--identity-provider <identity_provider>` shows an identity provider's protocols.", req.ID))
		return
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), req.ID)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("identity_provider"), parts[0])...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("name"), parts[1])...)
}
