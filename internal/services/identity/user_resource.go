// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0
//
// Ported from terraform-provider-openstack v3.4.0
// (openstack/resource_openstack_identity_user_v3.go), adapted for the
// terraform-plugin-framework and PCD.

package identity

import (
	"context"
	"fmt"
	"net/http"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/identity/v3/users"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/platform9/terraform-provider-pcd/internal/clients"
)

var (
	_ resource.Resource                = (*userResource)(nil)
	_ resource.ResourceWithConfigure   = (*userResource)(nil)
	_ resource.ResourceWithImportState = (*userResource)(nil)
)

// NewUserResource is the factory registered with the provider.
func NewUserResource() resource.Resource {
	return &userResource{}
}

type userResource struct {
	config *clients.Config
}

type userModel struct {
	ID               types.String `tfsdk:"id"`
	Name             types.String `tfsdk:"name"`
	Description      types.String `tfsdk:"description"`
	DomainID         types.String `tfsdk:"domain_id"`
	DefaultProjectID types.String `tfsdk:"default_project_id"`
	Enabled          types.Bool   `tfsdk:"enabled"`
	Password         types.String `tfsdk:"password"`
	Email            types.String `tfsdk:"email"`
	DisplayName      types.String `tfsdk:"display_name"`
	Region           types.String `tfsdk:"region"`
}

func (r *userResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_identity_user"
}

func (r *userResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Manages a user in PCD's Keystone identity service. Saving the user in the PCD " +
			"UI's edit form also sets `enabled = true` and rewrites `email` to the user name, so a user that " +
			"Terraform disabled, or whose `email` differs from `name`, shows drift after an edit there.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "The user ID.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"name": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "The name of the user.",
			},
			"description": schema.StringAttribute{
				Optional:            true,
				Computed:            true,
				MarkdownDescription: "A description of the user.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"domain_id": schema.StringAttribute{
				Optional:            true,
				Computed:            true,
				MarkdownDescription: "The domain the user belongs to. Changing this forces a new resource.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"default_project_id": schema.StringAttribute{
				Optional:            true,
				Computed:            true,
				MarkdownDescription: "The default project the user is scoped to.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"enabled": schema.BoolAttribute{
				Optional:            true,
				Computed:            true,
				Default:             booldefault.StaticBool(true),
				MarkdownDescription: "Whether the user is enabled. Defaults to true.",
			},
			"password": schema.StringAttribute{
				Optional:            true,
				Sensitive:           true,
				MarkdownDescription: "The user's password. Write-only: it is never read back from the API.",
			},
			"email": schema.StringAttribute{
				Optional: true,
				Computed: true,
				MarkdownDescription: "The user's email address, stored in Keystone as the extra attribute `email`. " +
					"The PCD UI shows it as the user's owner name in VM search and the app catalog. The PCD UI's " +
					"own \"Email\" field is the user's `name`: the UI expects `name` to be the email address and " +
					"copies it into this attribute whenever someone edits the user there, so a value that differs " +
					"from `name` shows as drift after a UI edit. The PCD UI's Users list hides a user whose email " +
					"is the name of one of PCD's service users, such as `mors` or `octavia`. Unset, Terraform " +
					"leaves the server's value alone; set `\"\"` to clear it.",
			},
			"display_name": schema.StringAttribute{
				Optional: true,
				Computed: true,
				MarkdownDescription: "The user's display name, stored in Keystone as the extra attribute " +
					"`displayname`; the PCD UI's Users list shows it in the Display Name column. Unset, Terraform " +
					"leaves the server's value alone; set `\"\"` to clear it.",
			},
			"region": schema.StringAttribute{
				Optional:            true,
				Computed:            true,
				MarkdownDescription: "The region in which to manage the user. Defaults to the provider's region. Changing this forces a new resource.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.RequiresReplace(), stringplanmodifier.UseStateForUnknown()},
			},
		},
	}
}

func (r *userResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.config = configureClient(req.ProviderData, &resp.Diagnostics)
}

func (r *userResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan userModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := r.config.ForRegion(plan.Region.ValueString()).IdentityV3Client()
	if err != nil {
		resp.Diagnostics.AddError("identity: building v3 client", err.Error())
		return
	}

	enabled := plan.Enabled.ValueBool()
	user, err := clients.RequireObject(users.Create(ctx, client, users.CreateOpts{
		Name:             plan.Name.ValueString(),
		Description:      plan.Description.ValueString(),
		DomainID:         plan.DomainID.ValueString(),
		DefaultProjectID: plan.DefaultProjectID.ValueString(),
		Enabled:          &enabled,
		Password:         plan.Password.ValueString(),
		Extra:            userCreateExtras(&plan),
	}).Extract())
	if err != nil {
		resp.Diagnostics.AddError("identity: creating user", err.Error())
		return
	}

	r.flatten(user, &plan)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *userResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state userModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := r.config.ForRegion(state.Region.ValueString()).IdentityV3Client()
	if err != nil {
		resp.Diagnostics.AddError("identity: building v3 client", err.Error())
		return
	}

	user, err := clients.RequireObject(users.Get(ctx, client, state.ID.ValueString()).Extract())
	if err != nil {
		if gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
			resp.Diagnostics.AddWarning("User not found",
				fmt.Sprintf("User %s no longer exists and was removed from state.", state.ID.ValueString()))
			resp.State.RemoveResource(ctx)
			return
		}
		resp.Diagnostics.AddError("identity: reading user", err.Error())
		return
	}

	r.flatten(user, &state)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *userResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state userModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := r.config.ForRegion(plan.Region.ValueString()).IdentityV3Client()
	if err != nil {
		resp.Diagnostics.AddError("identity: building v3 client", err.Error())
		return
	}

	// email and display_name are sent only when they differ from what the
	// user has now, read fresh: a UI edit since the last refresh must not be
	// mistaken for a value Terraform already set. With neither configured
	// there is nothing to compare, so nothing is read.
	var extras map[string]any
	if userCreateExtras(&plan) != nil {
		live, err := clients.RequireObject(users.Get(ctx, client, plan.ID.ValueString()).Extract())
		if err != nil {
			resp.Diagnostics.AddError("identity: reading user before update", err.Error())
			return
		}
		extras = userUpdateExtras(&plan, live)
	}

	enabled := plan.Enabled.ValueBool()
	description := plan.Description.ValueString()
	opts := users.UpdateOpts{
		Name:             plan.Name.ValueString(),
		Description:      &description,
		DefaultProjectID: plan.DefaultProjectID.ValueString(),
		Enabled:          &enabled,
		Extra:            extras,
	}
	// Only send a password when it actually changed (it is never read back).
	if !plan.Password.IsNull() && plan.Password.ValueString() != state.Password.ValueString() {
		opts.Password = plan.Password.ValueString()
	}

	user, err := clients.RequireObject(users.Update(ctx, client, plan.ID.ValueString(), opts).Extract())
	if err != nil {
		resp.Diagnostics.AddError("identity: updating user", err.Error())
		return
	}

	r.flatten(user, &plan)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *userResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state userModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := r.config.ForRegion(state.Region.ValueString()).IdentityV3Client()
	if err != nil {
		resp.Diagnostics.AddError("identity: building v3 client", err.Error())
		return
	}

	if err := users.Delete(ctx, client, state.ID.ValueString()).ExtractErr(); err != nil {
		if gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
			return
		}
		resp.Diagnostics.AddError("identity: deleting user", err.Error())
	}
}

func (r *userResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, path.Root("id"), req, resp)
}

// flatten copies a gophercloud user onto the model. The password is write-only
// and intentionally preserved from prior state, never read from the API.
func (r *userResource) flatten(u *users.User, m *userModel) {
	m.ID = types.StringValue(u.ID)
	m.Name = types.StringValue(u.Name)
	m.Description = types.StringValue(u.Description)
	m.DomainID = types.StringValue(u.DomainID)
	m.DefaultProjectID = types.StringValue(u.DefaultProjectID)
	m.Enabled = types.BoolValue(u.Enabled)
	m.Email = userExtraString(u, userExtraEmail)
	m.DisplayName = userExtraString(u, userExtraDisplayName)
	if m.Region.IsNull() || m.Region.IsUnknown() {
		m.Region = types.StringValue(r.config.Region)
	}
}

// The Keystone extra attributes behind email and display_name. The PCD UI
// writes and reads these exact keys (displayname has no underscore).
const (
	userExtraEmail       = "email"
	userExtraDisplayName = "displayname"
)

// userCreateExtras returns the extra attributes a new user is created with:
// each of email and display_name whose planned value is known and not null,
// "" included. nil when there are none.
func userCreateExtras(plan *userModel) map[string]any {
	extras := map[string]any{}
	for key, v := range map[string]types.String{userExtraEmail: plan.Email, userExtraDisplayName: plan.DisplayName} {
		if !v.IsNull() && !v.IsUnknown() {
			extras[key] = v.ValueString()
		}
	}
	if len(extras) == 0 {
		return nil
	}
	return extras
}

// userUpdateExtras returns the extra attributes an update must send: each of
// email and display_name whose planned value is known, not null, and differs
// from the user's live value. Keystone merges extras on PATCH, so the keys not
// sent keep their values. nil when nothing needs sending.
func userUpdateExtras(plan *userModel, live *users.User) map[string]any {
	extras := map[string]any{}
	for key, v := range map[string]types.String{userExtraEmail: plan.Email, userExtraDisplayName: plan.DisplayName} {
		if v.IsNull() || v.IsUnknown() {
			continue
		}
		if cur, ok := live.Extra[key].(string); ok && cur == v.ValueString() {
			continue
		}
		extras[key] = v.ValueString()
	}
	if len(extras) == 0 {
		return nil
	}
	return extras
}

// userExtraString reads one extra attribute: null when the user has none (or
// a JSON null), the string itself, or any other JSON value printed as text.
func userExtraString(u *users.User, key string) types.String {
	v, ok := u.Extra[key]
	if !ok || v == nil {
		return types.StringNull()
	}
	if s, ok := v.(string); ok {
		return types.StringValue(s)
	}
	return types.StringValue(fmt.Sprint(v))
}
