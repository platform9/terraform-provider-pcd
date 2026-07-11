// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0
//
// Ported from terraform-provider-openstack v3.4.0
// (openstack/resource_openstack_identity_application_credential_v3.go), adapted
// for the terraform-plugin-framework and PCD.

package identity

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/identity/v3/applicationcredentials"
	"github.com/gophercloud/gophercloud/v2/openstack/identity/v3/tokens"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/boolplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/setplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/platform9/terraform-provider-pcd/internal/clients"
)

var (
	_ resource.Resource                = (*appCredResource)(nil)
	_ resource.ResourceWithConfigure   = (*appCredResource)(nil)
	_ resource.ResourceWithImportState = (*appCredResource)(nil)
)

// NewApplicationCredentialResource is the factory registered with the provider.
func NewApplicationCredentialResource() resource.Resource {
	return &appCredResource{}
}

type appCredResource struct {
	config *clients.Config
}

type appCredModel struct {
	ID           types.String `tfsdk:"id"`
	Name         types.String `tfsdk:"name"`
	Description  types.String `tfsdk:"description"`
	Secret       types.String `tfsdk:"secret"`
	ProjectID    types.String `tfsdk:"project_id"`
	Roles        types.Set    `tfsdk:"roles"`
	ExpiresAt    types.String `tfsdk:"expires_at"`
	Unrestricted types.Bool   `tfsdk:"unrestricted"`
	Region       types.String `tfsdk:"region"`
}

func (r *appCredResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_identity_application_credential"
}

func (r *appCredResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	forceNewString := []planmodifier.String{stringplanmodifier.RequiresReplace(), stringplanmodifier.UseStateForUnknown()}
	resp.Schema = schema.Schema{
		MarkdownDescription: "Manages an application credential for the authenticated user. Application " +
			"credentials are immutable — any change forces a new resource.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "The application credential ID.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"name": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "The name of the application credential.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"description": schema.StringAttribute{
				Optional:            true,
				Computed:            true,
				MarkdownDescription: "A description of the application credential.",
				PlanModifiers:       forceNewString,
			},
			"secret": schema.StringAttribute{
				Optional:            true,
				Computed:            true,
				Sensitive:           true,
				MarkdownDescription: "The secret. If omitted, one is generated and returned on create only.",
				PlanModifiers:       forceNewString,
			},
			"project_id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "The project the credential is scoped to.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"roles": schema.SetAttribute{
				Optional:            true,
				Computed:            true,
				ElementType:         types.StringType,
				MarkdownDescription: "Role names the credential is limited to. Defaults to all of the user's roles.",
				PlanModifiers:       []planmodifier.Set{setplanmodifier.RequiresReplace(), setplanmodifier.UseStateForUnknown()},
			},
			"expires_at": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "RFC3339 expiry timestamp. If omitted, the credential does not expire.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"unrestricted": schema.BoolAttribute{
				Optional:            true,
				Computed:            true,
				Default:             booldefault.StaticBool(false),
				MarkdownDescription: "Whether the credential may be used to create/delete other application credentials and trusts.",
				PlanModifiers:       []planmodifier.Bool{boolplanmodifier.RequiresReplace()},
			},
			"region": schema.StringAttribute{
				Optional:            true,
				Computed:            true,
				MarkdownDescription: "The region. Defaults to the provider's region.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
		},
	}
}

func (r *appCredResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.config = configureClient(req.ProviderData, &resp.Diagnostics)
}

func (r *appCredResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan appCredModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := r.config.IdentityV3Client()
	if err != nil {
		resp.Diagnostics.AddError("identity: building v3 client", err.Error())
		return
	}
	userID, err := currentUserID(ctx, client)
	if err != nil {
		resp.Diagnostics.AddError("identity: resolving current user", err.Error())
		return
	}

	var roleList []applicationcredentials.Role
	if !plan.Roles.IsNull() && !plan.Roles.IsUnknown() {
		var names []string
		resp.Diagnostics.Append(plan.Roles.ElementsAs(ctx, &names, false)...)
		if resp.Diagnostics.HasError() {
			return
		}
		for _, n := range names {
			roleList = append(roleList, applicationcredentials.Role{Name: n})
		}
	}

	opts := applicationcredentials.CreateOpts{
		Name:         plan.Name.ValueString(),
		Description:  plan.Description.ValueString(),
		Unrestricted: plan.Unrestricted.ValueBool(),
		Secret:       plan.Secret.ValueString(),
		Roles:        roleList,
	}
	if v := plan.ExpiresAt.ValueString(); v != "" {
		ts, perr := time.Parse(time.RFC3339, v)
		if perr != nil {
			resp.Diagnostics.AddError("identity: invalid expires_at", fmt.Sprintf("must be RFC3339: %s", perr))
			return
		}
		opts.ExpiresAt = &ts
	}

	ac, err := applicationcredentials.Create(ctx, client, userID, opts).Extract()
	if err != nil {
		resp.Diagnostics.AddError("identity: creating application credential", err.Error())
		return
	}

	// The secret is only ever returned here; capture it into state.
	plan.Secret = types.StringValue(ac.Secret)
	resp.Diagnostics.Append(r.flatten(ctx, ac, &plan)...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *appCredResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state appCredModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := r.config.IdentityV3Client()
	if err != nil {
		resp.Diagnostics.AddError("identity: building v3 client", err.Error())
		return
	}
	userID, err := currentUserID(ctx, client)
	if err != nil {
		resp.Diagnostics.AddError("identity: resolving current user", err.Error())
		return
	}

	ac, err := applicationcredentials.Get(ctx, client, userID, state.ID.ValueString()).Extract()
	if err != nil {
		if gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
			resp.Diagnostics.AddWarning("Application credential not found",
				fmt.Sprintf("Application credential %s no longer exists and was removed from state.", state.ID.ValueString()))
			resp.State.RemoveResource(ctx)
			return
		}
		resp.Diagnostics.AddError("identity: reading application credential", err.Error())
		return
	}

	// secret and expires_at are preserved from prior state (never read back).
	resp.Diagnostics.Append(r.flatten(ctx, ac, &state)...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

// Update is required by the interface but never invoked (every attribute forces
// replacement).
func (r *appCredResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan appCredModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *appCredResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state appCredModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := r.config.IdentityV3Client()
	if err != nil {
		resp.Diagnostics.AddError("identity: building v3 client", err.Error())
		return
	}
	userID, err := currentUserID(ctx, client)
	if err != nil {
		resp.Diagnostics.AddError("identity: resolving current user", err.Error())
		return
	}

	if err := applicationcredentials.Delete(ctx, client, userID, state.ID.ValueString()).ExtractErr(); err != nil {
		if gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
			return
		}
		resp.Diagnostics.AddError("identity: deleting application credential", err.Error())
	}
}

func (r *appCredResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, path.Root("id"), req, resp)
}

// flatten copies server-known fields onto the model; secret and expires_at are
// left untouched (write-only / not returned).
func (r *appCredResource) flatten(ctx context.Context, ac *applicationcredentials.ApplicationCredential, m *appCredModel) (diags diag.Diagnostics) {
	m.ID = types.StringValue(ac.ID)
	m.Name = types.StringValue(ac.Name)
	m.Description = types.StringValue(ac.Description)
	m.ProjectID = types.StringValue(ac.ProjectID)
	m.Unrestricted = types.BoolValue(ac.Unrestricted)

	names := make([]string, 0, len(ac.Roles))
	for _, ro := range ac.Roles {
		names = append(names, ro.Name)
	}
	roles, d := types.SetValueFrom(ctx, types.StringType, names)
	diags = append(diags, d...)
	m.Roles = roles

	if m.Region.IsNull() || m.Region.IsUnknown() {
		m.Region = types.StringValue(r.config.Region)
	}
	return diags
}

// currentUserID returns the user ID of the token the provider is using.
func currentUserID(ctx context.Context, client *gophercloud.ServiceClient) (string, error) {
	user, err := tokens.Get(ctx, client, client.Token()).ExtractUser()
	if err != nil {
		return "", err
	}
	return user.ID, nil
}
