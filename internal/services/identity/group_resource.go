// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0
//
// Ported from terraform-provider-openstack v3.4.0
// (openstack/resource_openstack_identity_group_v3.go), adapted for the
// terraform-plugin-framework and PCD.

package identity

import (
	"context"
	"net/http"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/identity/v3/groups"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/platform9/terraform-provider-pcd/internal/clients"
)

var (
	_ resource.Resource                = (*groupResource)(nil)
	_ resource.ResourceWithConfigure   = (*groupResource)(nil)
	_ resource.ResourceWithImportState = (*groupResource)(nil)
)

// NewGroupResource is the factory registered with the provider.
func NewGroupResource() resource.Resource {
	return &groupResource{}
}

type groupResource struct {
	config *clients.Config
}

type groupModel struct {
	ID          types.String `tfsdk:"id"`
	Name        types.String `tfsdk:"name"`
	Description types.String `tfsdk:"description"`
	DomainID    types.String `tfsdk:"domain_id"`
	Region      types.String `tfsdk:"region"`
}

func (r *groupResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_identity_group"
}

func (r *groupResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	useState := []planmodifier.String{stringplanmodifier.UseStateForUnknown()}
	resp.Schema = schema.Schema{
		MarkdownDescription: "Manages a group in PCD's Keystone identity service. Role assignments and group " +
			"membership can target the group.",
		Attributes: map[string]schema.Attribute{
			"id":          schema.StringAttribute{Computed: true, MarkdownDescription: "The group ID.", PlanModifiers: useState},
			"name":        schema.StringAttribute{Required: true, MarkdownDescription: "The name of the group."},
			"description": schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "A description of the group.", PlanModifiers: useState},
			"domain_id": schema.StringAttribute{
				Optional:            true,
				Computed:            true,
				MarkdownDescription: "The domain the group belongs to. Defaults to the default domain. Changing this forces a new resource.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.RequiresReplace(), stringplanmodifier.UseStateForUnknown()},
			},
			"region": schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "The region. Defaults to the provider's region.", PlanModifiers: useState},
		},
	}
}

func (r *groupResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.config = configureClient(req.ProviderData, &resp.Diagnostics)
}

func (r *groupResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan groupModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := r.config.IdentityV3Client()
	if err != nil {
		resp.Diagnostics.AddError("identity: building v3 client", err.Error())
		return
	}

	group, err := groups.Create(ctx, client, groups.CreateOpts{
		Name:        plan.Name.ValueString(),
		Description: plan.Description.ValueString(),
		DomainID:    plan.DomainID.ValueString(),
	}).Extract()
	if err != nil {
		resp.Diagnostics.AddError("identity: creating group", err.Error())
		return
	}

	r.setState(&plan, group)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *groupResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state groupModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := r.config.IdentityV3Client()
	if err != nil {
		resp.Diagnostics.AddError("identity: building v3 client", err.Error())
		return
	}

	group, err := groups.Get(ctx, client, state.ID.ValueString()).Extract()
	if err != nil {
		if gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
			resp.State.RemoveResource(ctx)
			return
		}
		resp.Diagnostics.AddError("identity: reading group", err.Error())
		return
	}

	r.setState(&state, group)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *groupResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state groupModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := r.config.IdentityV3Client()
	if err != nil {
		resp.Diagnostics.AddError("identity: building v3 client", err.Error())
		return
	}

	updateOpts := groups.UpdateOpts{}
	if !plan.Name.Equal(state.Name) {
		updateOpts.Name = plan.Name.ValueString()
	}
	if !plan.Description.Equal(state.Description) {
		v := plan.Description.ValueString()
		updateOpts.Description = &v
	}

	group, err := groups.Update(ctx, client, plan.ID.ValueString(), updateOpts).Extract()
	if err != nil {
		resp.Diagnostics.AddError("identity: updating group", err.Error())
		return
	}

	r.setState(&plan, group)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *groupResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state groupModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := r.config.IdentityV3Client()
	if err != nil {
		resp.Diagnostics.AddError("identity: building v3 client", err.Error())
		return
	}

	if err := groups.Delete(ctx, client, state.ID.ValueString()).ExtractErr(); err != nil {
		if gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
			return
		}
		resp.Diagnostics.AddError("identity: deleting group", err.Error())
	}
}

func (r *groupResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, path.Root("id"), req, resp)
}

func (r *groupResource) setState(m *groupModel, group *groups.Group) {
	m.ID = types.StringValue(group.ID)
	m.Name = types.StringValue(group.Name)
	m.Description = types.StringValue(group.Description)
	m.DomainID = types.StringValue(group.DomainID)
	if m.Region.IsNull() || m.Region.IsUnknown() {
		m.Region = types.StringValue(r.config.Region)
	}
}
