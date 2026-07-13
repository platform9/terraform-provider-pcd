// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0
//
// Ported from terraform-provider-openstack v3.4.0 (identity group membership),
// adapted for the terraform-plugin-framework and PCD. This resource models a
// single user↔group membership (PUT/DELETE /groups/{id}/users/{user_id}).

package identity

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/identity/v3/users"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/platform9/terraform-provider-pcd/internal/clients"
)

var (
	_ resource.Resource                = (*groupMembershipResource)(nil)
	_ resource.ResourceWithConfigure   = (*groupMembershipResource)(nil)
	_ resource.ResourceWithImportState = (*groupMembershipResource)(nil)
)

// NewGroupMembershipResource is the factory registered with the provider.
func NewGroupMembershipResource() resource.Resource {
	return &groupMembershipResource{}
}

type groupMembershipResource struct {
	config *clients.Config
}

type groupMembershipModel struct {
	ID      types.String `tfsdk:"id"`
	GroupID types.String `tfsdk:"group_id"`
	UserID  types.String `tfsdk:"user_id"`
}

func (r *groupMembershipResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_identity_group_membership"
}

func (r *groupMembershipResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	forceNew := []planmodifier.String{stringplanmodifier.RequiresReplace()}
	resp.Schema = schema.Schema{
		MarkdownDescription: "Adds a single user to a group in PCD's Keystone identity service. Use one resource " +
			"per user↔group pair.",
		Attributes: map[string]schema.Attribute{
			"id":       schema.StringAttribute{Computed: true, MarkdownDescription: "The composite `<group_id>/<user_id>` ID.", PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()}},
			"group_id": schema.StringAttribute{Required: true, MarkdownDescription: "The group to add the user to. Changing this forces a new resource.", PlanModifiers: forceNew},
			"user_id":  schema.StringAttribute{Required: true, MarkdownDescription: "The user to add to the group. Changing this forces a new resource.", PlanModifiers: forceNew},
		},
	}
}

func (r *groupMembershipResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.config = configureClient(req.ProviderData, &resp.Diagnostics)
}

func (r *groupMembershipResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan groupMembershipModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := r.config.IdentityV3Client()
	if err != nil {
		resp.Diagnostics.AddError("identity: building v3 client", err.Error())
		return
	}

	groupID := plan.GroupID.ValueString()
	userID := plan.UserID.ValueString()
	if err := users.AddToGroup(ctx, client, groupID, userID).ExtractErr(); err != nil {
		resp.Diagnostics.AddError("identity: adding user to group", err.Error())
		return
	}

	plan.ID = types.StringValue(groupID + "/" + userID)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *groupMembershipResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state groupMembershipModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := r.config.IdentityV3Client()
	if err != nil {
		resp.Diagnostics.AddError("identity: building v3 client", err.Error())
		return
	}

	groupID := state.GroupID.ValueString()
	userID := state.UserID.ValueString()
	member, err := users.IsMemberOfGroup(ctx, client, groupID, userID).Extract()
	if err != nil {
		if gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
			resp.State.RemoveResource(ctx)
			return
		}
		resp.Diagnostics.AddError("identity: checking group membership", err.Error())
		return
	}
	if !member {
		resp.State.RemoveResource(ctx)
		return
	}

	state.ID = types.StringValue(groupID + "/" + userID)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

// Update is unreachable: both group_id and user_id force replacement.
func (r *groupMembershipResource) Update(_ context.Context, _ resource.UpdateRequest, resp *resource.UpdateResponse) {
	resp.Diagnostics.AddError("identity: group membership update", "group_id and user_id are immutable; this should not be reached.")
}

func (r *groupMembershipResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state groupMembershipModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := r.config.IdentityV3Client()
	if err != nil {
		resp.Diagnostics.AddError("identity: building v3 client", err.Error())
		return
	}

	if err := users.RemoveFromGroup(ctx, client, state.GroupID.ValueString(), state.UserID.ValueString()).ExtractErr(); err != nil {
		if gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
			return
		}
		resp.Diagnostics.AddError("identity: removing user from group", err.Error())
	}
}

func (r *groupMembershipResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	parts := strings.SplitN(req.ID, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		resp.Diagnostics.AddError("Invalid import ID", fmt.Sprintf("expected <group_id>/<user_id>, got %q", req.ID))
		return
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), req.ID)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("group_id"), parts[0])...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("user_id"), parts[1])...)
}
