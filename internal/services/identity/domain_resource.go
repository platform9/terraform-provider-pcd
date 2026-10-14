// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0
//
// terraform-provider-openstack v3.4.0 has no domain resource (its only path is
// openstack_identity_project_v3.is_domain); this one follows the structure of
// group_resource.go over gophercloud's identity/v3/domains package.

package identity

import (
	"context"
	"fmt"
	"net/http"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/identity/v3/domains"
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
	_ resource.Resource                = (*domainResource)(nil)
	_ resource.ResourceWithConfigure   = (*domainResource)(nil)
	_ resource.ResourceWithImportState = (*domainResource)(nil)
	_ resource.ResourceWithModifyPlan  = (*domainResource)(nil)
)

// NewDomainResource is the factory registered with the provider.
func NewDomainResource() resource.Resource {
	return &domainResource{}
}

type domainResource struct {
	config *clients.Config
}

type domainModel struct {
	ID          types.String `tfsdk:"id"`
	Name        types.String `tfsdk:"name"`
	Description types.String `tfsdk:"description"`
	Enabled     types.Bool   `tfsdk:"enabled"`
	Region      types.String `tfsdk:"region"`
}

func (r *domainResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_identity_domain"
}

func (r *domainResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	useState := []planmodifier.String{stringplanmodifier.UseStateForUnknown()}
	resp.Schema = schema.Schema{
		MarkdownDescription: "Manages a domain in PCD's Keystone identity service: the container the PCD UI lists " +
			"under Domains, which holds its own projects, users and groups.\n\n" +
			"**Destroying a domain deletes everything in it.** Keystone deletes every project, user and group " +
			"in the domain and every identity provider registered to it; servers, volumes and networks in " +
			"those projects are left behind without an owner. Keystone refuses to delete an enabled domain, so " +
			"destroy disables the domain first and then deletes it.\n\n" +
			"The provider refuses to disable or destroy Keystone's `default` domain, or the domain that the " +
			"user or project it authenticates as belongs to, because that would revoke its own token. To stop " +
			"managing such a domain, remove it from state instead (a `removed` block with `destroy = false`, or " +
			"`terraform state rm`). Do not manage PCD's own service domains, such as `heat`, where a deployment " +
			"has them: PCD's services use them, and the PCD UI does not let you edit them.\n\n" +
			"To grant a role on every project in a domain, the way the PCD UI's domain user assignment does, use " +
			"`pcd_identity_inherit_role_assignment`.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{Computed: true, MarkdownDescription: "The domain ID.", PlanModifiers: useState},
			"name": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "The name of the domain, unique in Keystone. Renaming updates the domain in place. " +
					"The PCD UI derives the name of a domain's SAML identity provider from the domain name, so " +
					"renaming a domain that has single sign-on configured breaks its sign-on until the identity " +
					"provider is renamed too.",
				Validators: []validator.String{stringvalidator.LengthBetween(1, 64)},
			},
			"description": schema.StringAttribute{
				Optional: true,
				Computed: true,
				MarkdownDescription: "A description of the domain. Removing the attribute from the configuration " +
					"keeps the current description; set it to `\"\"` to clear it.",
				PlanModifiers: useState,
			},
			"enabled": schema.BoolAttribute{
				Optional:            true,
				Computed:            true,
				Default:             booldefault.StaticBool(true),
				MarkdownDescription: "Whether the domain is enabled. Users in a disabled domain cannot authenticate. Defaults to `true`.",
			},
			"region": schema.StringAttribute{
				Optional: true,
				Computed: true,
				MarkdownDescription: "The region whose identity endpoint the provider reaches the domain through. Keystone " +
					"is global, so the domain is the same in every region. Defaults to the provider's region. Replacing " +
					"a domain deletes everything in it (see above). Changing this forces a new resource.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace(), stringplanmodifier.UseStateForUnknown()},
			},
		},
	}
}

func (r *domainResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.config = configureClient(req.ProviderData, &resp.Diagnostics)
}

// ModifyPlan refuses, at plan time, to destroy or disable a domain the
// provider itself depends on. Update and Delete check again at apply time, for
// plans made while the provider was not yet configured and for the destroy
// half of a replacement.
func (r *domainResource) ModifyPlan(ctx context.Context, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	if req.State.Raw.IsNull() || r.config == nil || r.config.Provider == nil {
		return
	}
	var state domainModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	action := ""
	if req.Plan.Raw.IsNull() {
		action = "destroy"
	} else {
		var plan domainModel
		resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
		if resp.Diagnostics.HasError() {
			return
		}
		if disablesDomain(&plan, &state) {
			action = "disable"
		}
	}
	if action == "" {
		return
	}
	resp.Diagnostics.Append(r.refuseProtectedDomain(ctx, state.Region.ValueString(), state.ID.ValueString(), action)...)
}

func (r *domainResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan domainModel
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
	domain, err := domains.Create(ctx, client, domains.CreateOpts{
		Name:        plan.Name.ValueString(),
		Description: plan.Description.ValueString(),
		Enabled:     &enabled,
	}).Extract()
	if err != nil {
		resp.Diagnostics.AddError("identity: creating domain", err.Error())
		return
	}
	// A 201 without the object decodes to nil, and leaves no ID to record.
	if domain == nil {
		resp.Diagnostics.AddError("identity: creating domain",
			fmt.Sprintf("The Identity API answered without a domain object for the new domain %q.", plan.Name.ValueString()))
		return
	}

	r.setState(&plan, domain)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *domainResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state domainModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := r.config.ForRegion(state.Region.ValueString()).IdentityV3Client()
	if err != nil {
		resp.Diagnostics.AddError("identity: building v3 client", err.Error())
		return
	}

	domain, err := domains.Get(ctx, client, state.ID.ValueString()).Extract()
	if err != nil {
		if gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
			resp.Diagnostics.AddWarning("Domain not found",
				fmt.Sprintf("Domain %s no longer exists and was removed from state.", state.ID.ValueString()))
			resp.State.RemoveResource(ctx)
			return
		}
		resp.Diagnostics.AddError("identity: reading domain", err.Error())
		return
	}
	// A 200 without the object decodes to nil. It is not a not-found: that
	// would drop a domain that exists from state.
	if domain == nil {
		resp.Diagnostics.AddError("identity: reading domain",
			fmt.Sprintf("The Identity API answered without a domain object for ID %q.", state.ID.ValueString()))
		return
	}

	r.setState(&state, domain)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *domainResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state domainModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	opts, send := domainUpdateOpts(&plan, &state)
	if !send {
		// Nothing Keystone stores changed (a changed region replaces the
		// domain), and Keystone rejects an empty PATCH, so there is nothing to send.
		resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
		return
	}
	if disablesDomain(&plan, &state) {
		resp.Diagnostics.Append(r.refuseProtectedDomain(ctx, state.Region.ValueString(), state.ID.ValueString(), "disable")...)
		if resp.Diagnostics.HasError() {
			return
		}
	}

	client, err := r.config.ForRegion(plan.Region.ValueString()).IdentityV3Client()
	if err != nil {
		resp.Diagnostics.AddError("identity: building v3 client", err.Error())
		return
	}

	domain, err := domains.Update(ctx, client, state.ID.ValueString(), opts).Extract()
	if err != nil {
		resp.Diagnostics.AddError("identity: updating domain", err.Error())
		return
	}
	if domain == nil {
		resp.Diagnostics.AddError("identity: updating domain",
			fmt.Sprintf("The Identity API answered without a domain object for ID %q.", state.ID.ValueString()))
		return // keep the prior state, so the next plan retries the update
	}

	r.setState(&plan, domain)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *domainResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state domainModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	resp.Diagnostics.Append(r.refuseProtectedDomain(ctx, state.Region.ValueString(), state.ID.ValueString(), "destroy")...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := r.config.ForRegion(state.Region.ValueString()).IdentityV3Client()
	if err != nil {
		resp.Diagnostics.AddError("identity: building v3 client", err.Error())
		return
	}

	if err := deleteDomain(ctx, client, state.ID.ValueString()); err != nil {
		resp.Diagnostics.AddError("identity: deleting domain", err.Error())
	}
}

func (r *domainResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	if req.ID == "" {
		resp.Diagnostics.AddError("Import needs a domain ID",
			"Pass the ID of the domain to import, which `pcdctl domain list` shows.")
		return
	}
	resource.ImportStatePassthroughID(ctx, path.Root("id"), req, resp)
}

func (r *domainResource) setState(m *domainModel, d *domains.Domain) {
	m.ID = types.StringValue(d.ID)
	m.Name = types.StringValue(d.Name)
	m.Description = types.StringValue(d.Description)
	m.Enabled = types.BoolValue(d.Enabled)
	if m.Region.IsNull() || m.Region.IsUnknown() {
		m.Region = types.StringValue(r.config.Region)
	}
}

// refuseProtectedDomain returns an error diagnostic when id names a domain the
// provider must not disable or destroy (see tokenScope.protectedDomainReason).
func (r *domainResource) refuseProtectedDomain(ctx context.Context, region, id, action string) diag.Diagnostics {
	var diags diag.Diagnostics
	client, err := r.config.ForRegion(region).IdentityV3Client()
	if err != nil {
		diags.AddError("identity: building v3 client", err.Error())
		return diags
	}
	reason, err := domainProtection(ctx, client, id)
	if err != nil {
		diags.AddError("identity: checking which domain the provider authenticates into", err.Error())
		return diags
	}
	if reason == "" {
		return diags
	}
	diags.AddError(
		fmt.Sprintf("Refusing to %s a protected domain", action),
		fmt.Sprintf("The provider will not %s domain %s: %s, so the change would cut off the provider's own "+
			"access. To stop managing the domain without changing it, remove it from state (a `removed` block "+
			"with `destroy = false`, or `terraform state rm`). To keep managing it, leave `enabled` unset or `true`.",
			action, id, reason),
	)
	return diags
}

// domainProtection reports why the domain with this ID is protected, or "".
// Keystone's default domain needs no API call; any other ID is compared with
// the provider's own token.
func domainProtection(ctx context.Context, client *gophercloud.ServiceClient, id string) (string, error) {
	if reason := (tokenScope{}).protectedDomainReason(id); reason != "" {
		return reason, nil
	}
	scope, err := currentTokenScope(ctx, client)
	if err != nil {
		return "", err
	}
	return scope.protectedDomainReason(id), nil
}

// disablesDomain reports whether applying plan turns an enabled domain off.
func disablesDomain(plan, state *domainModel) bool {
	return !plan.Enabled.IsNull() && !plan.Enabled.IsUnknown() && !plan.Enabled.ValueBool() &&
		state.Enabled.ValueBool()
}

// domainUpdateOpts builds the PATCH body. When name, description or enabled
// changed it sends all three as planned, so the domain matches the plan even
// if the PCD UI edited one of them since the last refresh. When none of them
// changed it reports false: Keystone rejects an empty PATCH.
func domainUpdateOpts(plan, state *domainModel) (domains.UpdateOpts, bool) {
	if plan.Name.Equal(state.Name) && plan.Description.Equal(state.Description) && plan.Enabled.Equal(state.Enabled) {
		return domains.UpdateOpts{}, false
	}
	opts := domains.UpdateOpts{Name: plan.Name.ValueString()}
	if !plan.Description.IsNull() && !plan.Description.IsUnknown() {
		d := plan.Description.ValueString()
		opts.Description = &d
	}
	if !plan.Enabled.IsNull() && !plan.Enabled.IsUnknown() {
		e := plan.Enabled.ValueBool()
		opts.Enabled = &e
	}
	return opts, true
}

// deleteDomain disables the domain, then deletes it. Keystone refuses to delete
// an enabled domain (403), and the disable is idempotent, so it is sent even
// when state says the domain is already disabled. A 404 on either call means
// the domain is gone. When the delete fails after the disable succeeded, the
// error says the domain was left disabled.
func deleteDomain(ctx context.Context, client *gophercloud.ServiceClient, id string) error {
	disabled := false
	if _, err := domains.Update(ctx, client, id, domains.UpdateOpts{Enabled: &disabled}).Extract(); err != nil {
		if gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
			return nil
		}
		return fmt.Errorf("disabling domain %s before deleting it: %w", id, err)
	}
	if err := domains.Delete(ctx, client, id).ExtractErr(); err != nil {
		if gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
			return nil
		}
		return fmt.Errorf("domain %s was disabled but could not be deleted, so it stays disabled "+
			"(the next apply re-enables it if the configuration keeps enabled = true): %w", id, err)
	}
	return nil
}
