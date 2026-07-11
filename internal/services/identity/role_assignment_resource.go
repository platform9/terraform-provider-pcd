// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0
//
// Ported from terraform-provider-openstack v3.4.0
// (openstack/resource_openstack_identity_role_assignment_v3.go), adapted for the
// terraform-plugin-framework and PCD.

package identity

import (
	"context"
	"fmt"
	"strings"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/identity/v3/roles"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/platform9/terraform-provider-pcd/internal/clients"
)

var (
	_ resource.Resource                = (*roleAssignmentResource)(nil)
	_ resource.ResourceWithConfigure   = (*roleAssignmentResource)(nil)
	_ resource.ResourceWithImportState = (*roleAssignmentResource)(nil)
)

// NewRoleAssignmentResource is the factory registered with the provider.
func NewRoleAssignmentResource() resource.Resource {
	return &roleAssignmentResource{}
}

type roleAssignmentResource struct {
	config *clients.Config
}

type roleAssignmentModel struct {
	ID        types.String `tfsdk:"id"`
	RoleID    types.String `tfsdk:"role_id"`
	UserID    types.String `tfsdk:"user_id"`
	GroupID   types.String `tfsdk:"group_id"`
	ProjectID types.String `tfsdk:"project_id"`
	DomainID  types.String `tfsdk:"domain_id"`
	Region    types.String `tfsdk:"region"`
}

func (r *roleAssignmentResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_identity_role_assignment"
}

func (r *roleAssignmentResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	forceNew := []planmodifier.String{stringplanmodifier.RequiresReplace()}
	resp.Schema = schema.Schema{
		MarkdownDescription: "Assigns a role to a user or group on a project or domain. Assignments are " +
			"immutable: any change forces a new resource. Exactly one of `user_id`/`group_id` and exactly " +
			"one of `project_id`/`domain_id` must be set.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Composite ID: `domain_id/project_id/group_id/user_id/role_id`.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"role_id": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "The role to assign.",
				PlanModifiers:       forceNew,
			},
			"user_id": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "The user to assign the role to (mutually exclusive with group_id).",
				PlanModifiers:       forceNew,
			},
			"group_id": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "The group to assign the role to (mutually exclusive with user_id).",
				PlanModifiers:       forceNew,
			},
			"project_id": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "The project the assignment is scoped to (mutually exclusive with domain_id).",
				PlanModifiers:       forceNew,
			},
			"domain_id": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "The domain the assignment is scoped to (mutually exclusive with project_id).",
				PlanModifiers:       forceNew,
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

func (r *roleAssignmentResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.config = configureClient(req.ProviderData, &resp.Diagnostics)
}

func (r *roleAssignmentResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan roleAssignmentModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	roleID := plan.RoleID.ValueString()
	userID := plan.UserID.ValueString()
	groupID := plan.GroupID.ValueString()
	projectID := plan.ProjectID.ValueString()
	domainID := plan.DomainID.ValueString()

	if (userID == "") == (groupID == "") {
		resp.Diagnostics.AddError("Invalid role assignment", "Exactly one of user_id or group_id must be set.")
	}
	if (projectID == "") == (domainID == "") {
		resp.Diagnostics.AddError("Invalid role assignment", "Exactly one of project_id or domain_id must be set.")
	}
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := r.config.IdentityV3Client()
	if err != nil {
		resp.Diagnostics.AddError("identity: building v3 client", err.Error())
		return
	}

	if err := roles.Assign(ctx, client, roleID, roles.AssignOpts{
		UserID:    userID,
		GroupID:   groupID,
		ProjectID: projectID,
		DomainID:  domainID,
	}).ExtractErr(); err != nil {
		resp.Diagnostics.AddError("identity: assigning role", err.Error())
		return
	}

	plan.ID = types.StringValue(buildRoleAssignmentID(domainID, projectID, groupID, userID, roleID))
	if plan.Region.IsNull() || plan.Region.IsUnknown() {
		plan.Region = types.StringValue(r.config.Region)
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *roleAssignmentResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state roleAssignmentModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	domainID, projectID, groupID, userID, roleID, err := parseRoleAssignmentID(state.ID.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("identity: parsing role assignment id", err.Error())
		return
	}

	client, err := r.config.IdentityV3Client()
	if err != nil {
		resp.Diagnostics.AddError("identity: building v3 client", err.Error())
		return
	}

	exists, err := roleAssignmentExists(ctx, client, domainID, projectID, groupID, userID, roleID)
	if err != nil {
		resp.Diagnostics.AddError("identity: reading role assignment", err.Error())
		return
	}
	if !exists {
		resp.Diagnostics.AddWarning("Role assignment not found",
			fmt.Sprintf("Role assignment %s no longer exists and was removed from state.", state.ID.ValueString()))
		resp.State.RemoveResource(ctx)
		return
	}

	// Repopulate attributes from the ID (supports import) and keep them stable.
	state.RoleID = types.StringValue(roleID)
	state.UserID = optionalString(userID)
	state.GroupID = optionalString(groupID)
	state.ProjectID = optionalString(projectID)
	state.DomainID = optionalString(domainID)
	if state.Region.IsNull() || state.Region.IsUnknown() {
		state.Region = types.StringValue(r.config.Region)
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

// Update is required by the interface but never invoked: every attribute forces
// replacement. It defensively persists the plan.
func (r *roleAssignmentResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan roleAssignmentModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *roleAssignmentResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state roleAssignmentModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	domainID, projectID, groupID, userID, roleID, err := parseRoleAssignmentID(state.ID.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("identity: parsing role assignment id", err.Error())
		return
	}

	client, err := r.config.IdentityV3Client()
	if err != nil {
		resp.Diagnostics.AddError("identity: building v3 client", err.Error())
		return
	}

	if err := roles.Unassign(ctx, client, roleID, roles.UnassignOpts{
		UserID:    userID,
		GroupID:   groupID,
		ProjectID: projectID,
		DomainID:  domainID,
	}).ExtractErr(); err != nil {
		if gophercloud.ResponseCodeIs(err, 404) {
			return
		}
		resp.Diagnostics.AddError("identity: unassigning role", err.Error())
	}
}

func (r *roleAssignmentResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, path.Root("id"), req, resp)
}

func roleAssignmentExists(ctx context.Context, client *gophercloud.ServiceClient, domainID, projectID, groupID, userID, roleID string) (bool, error) {
	pages, err := roles.ListAssignments(client, roles.ListAssignmentsOpts{
		RoleID:         roleID,
		ScopeDomainID:  domainID,
		ScopeProjectID: projectID,
		UserID:         userID,
		GroupID:        groupID,
	}).AllPages(ctx)
	if err != nil {
		return false, err
	}
	all, err := roles.ExtractRoleAssignments(pages)
	if err != nil {
		return false, err
	}
	return len(all) > 0, nil
}

func buildRoleAssignmentID(domainID, projectID, groupID, userID, roleID string) string {
	return strings.Join([]string{domainID, projectID, groupID, userID, roleID}, "/")
}

func parseRoleAssignmentID(id string) (domainID, projectID, groupID, userID, roleID string, err error) {
	parts := strings.SplitN(id, "/", 5)
	if len(parts) != 5 {
		return "", "", "", "", "", fmt.Errorf("expected id in the form domain_id/project_id/group_id/user_id/role_id, got %q", id)
	}
	return parts[0], parts[1], parts[2], parts[3], parts[4], nil
}

func optionalString(v string) types.String {
	if v == "" {
		return types.StringNull()
	}
	return types.StringValue(v)
}
