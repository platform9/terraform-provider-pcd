// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0
//
// Ported from terraform-provider-openstack v3.4.0
// (openstack/resource_openstack_images_image_access_accept_v2.go), adapted for
// the terraform-plugin-framework and PCD. The member project's side of image
// sharing: accept, reject or leave pending an image another project shared
// (PUT /v2/images/{id}/members/{member}). Glance lets only the member decide
// (or an admin), so this runs under a provider scoped to the member project.

package images

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/identity/v3/tokens"
	"github.com/gophercloud/gophercloud/v2/openstack/image/v2/members"
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
	_ resource.Resource                = (*imageAccessAcceptResource)(nil)
	_ resource.ResourceWithConfigure   = (*imageAccessAcceptResource)(nil)
	_ resource.ResourceWithImportState = (*imageAccessAcceptResource)(nil)
)

// NewImageAccessAcceptResource is the factory registered with the provider.
func NewImageAccessAcceptResource() resource.Resource {
	return &imageAccessAcceptResource{}
}

type imageAccessAcceptResource struct {
	config *clients.Config
}

func (r *imageAccessAcceptResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_images_image_access_accept"
}

func (r *imageAccessAcceptResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	stable := []planmodifier.String{stringplanmodifier.UseStateForUnknown()}
	resp.Schema = schema.Schema{
		MarkdownDescription: "Accepts or rejects an image another project shared with this one: the member's side " +
			"of PCD image sharing (the UI's \"Accept Shared Images\"). Glance lets only the member project (or an " +
			"admin) decide, so use it under a provider scoped to the member project, for example a second " +
			"`provider \"pcd\"` block with an `alias` and that project's `tenant_name`. Give the alias literal values " +
			"or variables: an auth setting that is unknown at plan time (a value from a resource in the same " +
			"configuration) is ignored, and the provider falls back to the `OS_*` environment without an error. " +
			"Do not export `OS_PROJECT_ID` or `OS_TENANT_ID` when using such an alias: the alias would inherit it " +
			"next to `tenant_name`, and authentication fails with \"You must provide at most one of ProjectID or " +
			"ProjectName in a Scope\". " +
			"Destroying the resource rejects the image, since a member cannot remove itself. Do not combine it with " +
			"`status` on `pcd_images_image_access` for the same membership: each would undo the other's status.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{Computed: true, PlanModifiers: stable,
				MarkdownDescription: "The composite `<image_id>/<member_id>` ID."},
			"image_id": schema.StringAttribute{Required: true,
				PlanModifiers:       []planmodifier.String{stringplanmodifier.RequiresReplace()},
				MarkdownDescription: "The ID of the shared image. Changing this forces a new resource."},
			"member_id": schema.StringAttribute{Optional: true, Computed: true,
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace(), stringplanmodifier.UseStateForUnknown()},
				MarkdownDescription: "The member project's ID. When omitted, the provider uses the image's only member " +
					"if exactly one is visible (always the case for a project that is not an admin), and otherwise the " +
					"project the provider is scoped to. Changing this forces a new resource."},
			"status": schema.StringAttribute{Required: true,
				Validators:          []validator.String{stringvalidator.OneOf(memberStatuses...)},
				MarkdownDescription: "The decision: `accepted`, `rejected` or `pending`."},
			"created_at": schema.StringAttribute{Computed: true, PlanModifiers: stable,
				MarkdownDescription: "When the membership was created (RFC3339)."},
			"updated_at": schema.StringAttribute{Computed: true,
				MarkdownDescription: "When the membership last changed (RFC3339)."},
			"schema": schema.StringAttribute{Computed: true, PlanModifiers: stable,
				MarkdownDescription: "The JSON schema of the member record."},
			"region": schema.StringAttribute{Optional: true, Computed: true,
				PlanModifiers:       []planmodifier.String{stringplanmodifier.RequiresReplace(), stringplanmodifier.UseStateForUnknown()},
				MarkdownDescription: "The region. Defaults to the provider's region. Changing this forces a new resource."},
		},
	}
}

func (r *imageAccessAcceptResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	if req.ProviderData == nil {
		return
	}
	config, ok := req.ProviderData.(*clients.Config)
	if !ok {
		resp.Diagnostics.AddError("Unexpected provider data type",
			fmt.Sprintf("Expected *clients.Config, got %T. This is a bug in the provider.", req.ProviderData))
		return
	}
	r.config = config
}

func (r *imageAccessAcceptResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan imageAccessModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := r.config.ForRegion(plan.Region.ValueString()).ImageV2Client()
	if err != nil {
		resp.Diagnostics.AddError("images: building v2 client", err.Error())
		return
	}

	imageID := plan.ImageID.ValueString()
	memberID := plan.MemberID.ValueString()
	if plan.MemberID.IsNull() || plan.MemberID.IsUnknown() || memberID == "" {
		memberID, err = r.detectMemberID(ctx, client, plan.Region.ValueString(), imageID)
		if err != nil {
			resp.Diagnostics.AddAttributeError(path.Root("member_id"), "Cannot determine the member project", err.Error())
			return
		}
	}

	status := plan.Status.ValueString()
	member, err := memberOf(members.Update(ctx, client, imageID, memberID, members.UpdateOpts{Status: status}).Extract())
	if err != nil {
		if gophercloud.ResponseCodeIs(err, http.StatusNotFound) || gophercloud.ResponseCodeIs(err, http.StatusForbidden) {
			resp.Diagnostics.AddAttributeError(path.Root("image_id"), "Image is not shared with this project",
				fmt.Sprintf("Image %s has no membership for project %s that this provider can change. The image's "+
					"owner must add the project as a member (pcd_images_image_access) while the image's visibility "+
					"is \"shared\", and this resource must run under a provider scoped to that project. Glance said: %v",
					imageID, memberID, err))
			return
		}
		resp.Diagnostics.AddError("images: setting image member status", err.Error())
		return
	}

	plan.ID = types.StringValue(imageID + "/" + memberID)
	if plan.Region.IsNull() || plan.Region.IsUnknown() {
		plan.Region = types.StringValue(r.config.Region)
	}
	setImageMember(&plan, member)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *imageAccessAcceptResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state imageAccessModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := r.config.ForRegion(state.Region.ValueString()).ImageV2Client()
	if err != nil {
		resp.Diagnostics.AddError("images: building v2 client", err.Error())
		return
	}

	gone, diags := readImageMember(ctx, client, &state)
	resp.Diagnostics.Append(diags...)
	if gone {
		resp.State.RemoveResource(ctx)
		return
	}
	if resp.Diagnostics.HasError() {
		return
	}
	if state.Region.IsNull() || state.Region.IsUnknown() {
		state.Region = types.StringValue(r.config.Region)
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *imageAccessAcceptResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan imageAccessModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := r.config.ForRegion(plan.Region.ValueString()).ImageV2Client()
	if err != nil {
		resp.Diagnostics.AddError("images: building v2 client", err.Error())
		return
	}

	member, err := memberOf(members.Update(ctx, client, plan.ImageID.ValueString(), plan.MemberID.ValueString(),
		members.UpdateOpts{Status: plan.Status.ValueString()}).Extract())
	if err != nil {
		resp.Diagnostics.AddError("images: setting image member status", err.Error())
		return
	}
	setImageMember(&plan, member)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

// Delete rejects the image: Glance does not let a member remove itself.
func (r *imageAccessAcceptResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state imageAccessModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := r.config.ForRegion(state.Region.ValueString()).ImageV2Client()
	if err != nil {
		resp.Diagnostics.AddError("images: building v2 client", err.Error())
		return
	}

	imageID := state.ImageID.ValueString()
	memberID := state.MemberID.ValueString()
	_, err = members.Update(ctx, client, imageID, memberID, members.UpdateOpts{Status: "rejected"}).Extract()
	switch {
	case err == nil, gophercloud.ResponseCodeIs(err, http.StatusNotFound):
	case gophercloud.ResponseCodeIs(err, http.StatusForbidden):
		gone, readErr := imageNoLongerShared(ctx, client, imageID)
		switch {
		case readErr != nil:
			resp.Diagnostics.AddError("images: rejecting image membership",
				fmt.Sprintf("Glance refused to reject image %s for project %s (403), and reading the image to learn "+
					"why failed: %v. Glance said: %v", imageID, memberID, readErr, err))
		case !gone:
			resp.Diagnostics.AddError("images: rejecting image membership",
				fmt.Sprintf("Glance refused to reject image %s for project %s (403) although the image is still "+
					"shared, so the membership keeps its status. Only the member project or an admin can decide: "+
					"check the project this provider is scoped to. Glance said: %v", imageID, memberID, err))
		default:
			resp.Diagnostics.AddWarning("Image membership not rejected",
				fmt.Sprintf("Glance refused to reject image %s for project %s (403) because the image's visibility "+
					"is no longer \"shared\". The resource was removed from state; the membership keeps its last "+
					"status and applies again if the image is shared again.", imageID, memberID))
		}
	default:
		resp.Diagnostics.AddError("images: rejecting image membership", err.Error())
	}
}

// ImportState accepts "<image_id>/<member_id>", or a bare "<image_id>", whose
// member is found the way Create finds an omitted member_id.
func (r *imageAccessAcceptResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	imageID, memberID, hasMember := strings.Cut(req.ID, "/")
	if imageID == "" || (hasMember && (memberID == "" || strings.Contains(memberID, "/"))) {
		resp.Diagnostics.AddError("Invalid import ID", fmt.Sprintf("expected <image_id>/<member_id> or <image_id>, got %q", req.ID))
		return
	}
	if !hasMember {
		client, err := r.config.ForRegion("").ImageV2Client() // the ID names no region: the provider's region
		if err != nil {
			resp.Diagnostics.AddError("images: building v2 client", err.Error())
			return
		}
		memberID, err = r.detectMemberID(ctx, client, "", imageID)
		if err != nil {
			resp.Diagnostics.AddError("Cannot determine the member project", err.Error())
			return
		}
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), imageID+"/"+memberID)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("image_id"), imageID)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("member_id"), memberID)...)
}

// detectMemberID follows upstream: when exactly one member of the image is
// visible, that is the member. A project that is not an admin only ever sees
// its own membership. Otherwise (none visible, or an admin seeing several) it
// falls back to the project the provider's token is scoped to.
func (r *imageAccessAcceptResource) detectMemberID(ctx context.Context, client *gophercloud.ServiceClient, region, imageID string) (string, error) {
	pages, err := members.List(client, imageID).AllPages(ctx)
	switch {
	case err == nil:
		all, err := members.ExtractMembers(pages)
		if err != nil {
			return "", fmt.Errorf("reading the members of image %s: %w", imageID, err)
		}
		if len(all) == 1 {
			return all[0].MemberID, nil
		}
	case gophercloud.ResponseCodeIs(err, http.StatusNotFound), gophercloud.ResponseCodeIs(err, http.StatusForbidden):
		// The image is not visible to this project, so it has no visible member;
		// the token's project is the only candidate left.
	default:
		return "", fmt.Errorf("listing the members of image %s: %w", imageID, err)
	}

	identity, err := r.config.ForRegion(region).IdentityV3Client()
	if err != nil {
		return "", fmt.Errorf("building the identity client: %w", err)
	}
	project, err := clients.RequireObject(tokens.Get(ctx, identity, identity.Token()).ExtractProject())
	switch {
	case errors.Is(err, clients.ErrNoObject), err == nil && project.ID == "":
		// The token carries no project: it is scoped to a domain, or to nothing.
		return "", errors.New("set member_id: the provider's token is not scoped to a project, and image " +
			imageID + " does not show exactly one member")
	case err != nil:
		return "", fmt.Errorf("reading the provider token's project: %w", err)
	}
	return project.ID, nil
}
