// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0
//
// Ported from terraform-provider-openstack v3.4.0
// (openstack/resource_openstack_identity_inherit_role_assignment_v3.go), adapted
// for the terraform-plugin-framework and PCD.

package identity

import (
	"context"
	"fmt"
	"net/http"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/identity/v3/osinherit"
	"github.com/hashicorp/terraform-plugin-framework-validators/resourcevalidator"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/platform9/terraform-provider-pcd/internal/clients"
)

var (
	_ resource.Resource                     = (*inheritRoleAssignmentResource)(nil)
	_ resource.ResourceWithConfigure        = (*inheritRoleAssignmentResource)(nil)
	_ resource.ResourceWithImportState      = (*inheritRoleAssignmentResource)(nil)
	_ resource.ResourceWithConfigValidators = (*inheritRoleAssignmentResource)(nil)
)

// NewInheritRoleAssignmentResource is the factory registered with the provider.
func NewInheritRoleAssignmentResource() resource.Resource {
	return &inheritRoleAssignmentResource{}
}

type inheritRoleAssignmentResource struct {
	config *clients.Config
}

func (r *inheritRoleAssignmentResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_identity_inherit_role_assignment"
}

func (r *inheritRoleAssignmentResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	forceNew := []planmodifier.String{stringplanmodifier.RequiresReplace()}
	resp.Schema = schema.Schema{
		MarkdownDescription: "Assigns an *inherited* role to a user or group: the role applies to every current " +
			"and future project in a domain (or to every project below a project), but not to the domain or " +
			"project itself. This is what the PCD UI's \"Domain-Level Access\" creates, for users and for SSO " +
			"groups alike (Keystone's OS-INHERIT `inherited_to_projects` grant). For a role on the domain or " +
			"project itself, use `pcd_identity_role_assignment`.\n\n" +
			"A domain-wide grant reaches every project in the domain, including projects other configurations " +
			"or the PCD UI manage. Assignments are immutable: any change forces a new resource. Replacing one " +
			"create-before-destroy with the same role, actor and target (unchanged arguments, for example " +
			"`terraform apply -replace` under that lifecycle setting, or only `region` changed, since Keystone is " +
			"global) leaves the grant revoked, because the old object's destroy removes the same grant the new " +
			"object just made; the next apply restores it. Exactly one of " +
			"`user_id`/`group_id` and exactly one of `domain_id`/`project_id` must be set.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Composite ID: `domain_id/project_id/group_id/user_id/role_id`, the same form as `pcd_identity_role_assignment`.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"role_id": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "The role to assign.",
				PlanModifiers:       forceNew,
			},
			"user_id": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "The user to assign the role to (mutually exclusive with `group_id`).",
				PlanModifiers:       forceNew,
			},
			"group_id": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "The group to assign the role to (mutually exclusive with `user_id`).",
				PlanModifiers:       forceNew,
			},
			"domain_id": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "The domain whose projects inherit the role (mutually exclusive with `project_id`).",
				PlanModifiers:       forceNew,
			},
			"project_id": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "The project whose subprojects inherit the role (mutually exclusive with `domain_id`).",
				PlanModifiers:       forceNew,
			},
			"region": schema.StringAttribute{
				Optional:            true,
				Computed:            true,
				MarkdownDescription: "The region whose identity endpoint the provider uses; Keystone is global. Defaults to the provider's region. Changing this forces a new resource.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.RequiresReplace(), stringplanmodifier.UseStateForUnknown()},
			},
		},
	}
}

func (r *inheritRoleAssignmentResource) ConfigValidators(_ context.Context) []resource.ConfigValidator {
	return []resource.ConfigValidator{
		resourcevalidator.ExactlyOneOf(path.MatchRoot("user_id"), path.MatchRoot("group_id")),
		resourcevalidator.ExactlyOneOf(path.MatchRoot("domain_id"), path.MatchRoot("project_id")),
	}
}

func (r *inheritRoleAssignmentResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.config = configureClient(req.ProviderData, &resp.Diagnostics)
}

func (r *inheritRoleAssignmentResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
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

	client, err := r.config.ForRegion(plan.Region.ValueString()).IdentityV3Client()
	if err != nil {
		resp.Diagnostics.AddError("identity: building v3 client", err.Error())
		return
	}

	if err := osinherit.Assign(ctx, client, roleID, osinherit.AssignOpts{
		UserID:    userID,
		GroupID:   groupID,
		ProjectID: projectID,
		DomainID:  domainID,
	}).ExtractErr(); err != nil {
		resp.Diagnostics.AddError("identity: assigning inherited role", err.Error())
		return
	}

	plan.ID = types.StringValue(buildRoleAssignmentID(domainID, projectID, groupID, userID, roleID))
	if plan.Region.IsNull() || plan.Region.IsUnknown() {
		plan.Region = types.StringValue(r.config.Region)
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *inheritRoleAssignmentResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state roleAssignmentModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	domainID, projectID, groupID, userID, roleID, err := parseRoleAssignmentID(state.ID.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("identity: parsing inherited role assignment id", err.Error())
		return
	}

	client, err := r.config.ForRegion(state.Region.ValueString()).IdentityV3Client()
	if err != nil {
		resp.Diagnostics.AddError("identity: building v3 client", err.Error())
		return
	}

	if err := osinherit.Validate(ctx, client, roleID, osinherit.ValidateOpts{
		UserID:    userID,
		GroupID:   groupID,
		ProjectID: projectID,
		DomainID:  domainID,
	}).ExtractErr(); err != nil {
		if gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
			resp.Diagnostics.AddWarning("Inherited role assignment not found",
				fmt.Sprintf("Inherited role assignment %s no longer exists and was removed from state.", state.ID.ValueString()))
			resp.State.RemoveResource(ctx)
			return
		}
		resp.Diagnostics.AddError("identity: reading inherited role assignment", err.Error())
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

// Update is unreachable: every attribute forces replacement.
func (r *inheritRoleAssignmentResource) Update(_ context.Context, _ resource.UpdateRequest, resp *resource.UpdateResponse) {
	resp.Diagnostics.AddError("identity: inherited role assignment update",
		"role_id, user_id, group_id, domain_id, project_id and region force replacement; this should not be reached.")
}

func (r *inheritRoleAssignmentResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state roleAssignmentModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	domainID, projectID, groupID, userID, roleID, err := parseRoleAssignmentID(state.ID.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("identity: parsing inherited role assignment id", err.Error())
		return
	}

	client, err := r.config.ForRegion(state.Region.ValueString()).IdentityV3Client()
	if err != nil {
		resp.Diagnostics.AddError("identity: building v3 client", err.Error())
		return
	}

	if err := osinherit.Unassign(ctx, client, roleID, osinherit.UnassignOpts{
		UserID:    userID,
		GroupID:   groupID,
		ProjectID: projectID,
		DomainID:  domainID,
	}).ExtractErr(); err != nil {
		if gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
			return
		}
		resp.Diagnostics.AddError("identity: unassigning inherited role", err.Error())
	}
}

func (r *inheritRoleAssignmentResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	if req.ID == "" {
		resp.Diagnostics.AddError("Import needs an inherited role assignment ID",
			"Pass `<domain_id>/<project_id>/<group_id>/<user_id>/<role_id>`, leaving the parts the grant does not use "+
				"empty; `pcdctl role assignment list --names --inherited` shows the grant.")
		return
	}
	// An ID with an empty role, or without exactly one actor and one target,
	// names no grant, so it is refused here instead of reaching Keystone. An ID
	// without five parts is refused by the refresh that follows, before any request.
	if domainID, projectID, groupID, userID, roleID, err := parseRoleAssignmentID(req.ID); err == nil &&
		(roleID == "" || (userID == "") == (groupID == "") || (domainID == "") == (projectID == "")) {
		resp.Diagnostics.AddError("Import needs an inherited role assignment ID",
			fmt.Sprintf("%q names no grant. Pass `<domain_id>/<project_id>/<group_id>/<user_id>/<role_id>` with the "+
				"role, exactly one of the group and the user, and exactly one of the domain and the project; "+
				"`pcdctl role assignment list --names --inherited` shows the grant.", req.ID))
		return
	}
	resource.ImportStatePassthroughID(ctx, path.Root("id"), req, resp)
}
