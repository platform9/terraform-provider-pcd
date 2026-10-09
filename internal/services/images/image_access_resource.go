// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0
//
// Ported from terraform-provider-openstack v3.4.0
// (openstack/resource_openstack_images_image_access_v2.go), adapted for the
// terraform-plugin-framework and PCD. The owner's side of image sharing: one
// resource is one project's membership of one image
// (POST/GET/PUT/DELETE /v2/images/{id}/members[/{member}]).

package images

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/image/v2/images"
	"github.com/gophercloud/gophercloud/v2/openstack/image/v2/members"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/platform9/terraform-provider-pcd/internal/clients"
	"github.com/platform9/terraform-provider-pcd/internal/tfstate"
)

var (
	_ resource.Resource                = (*imageAccessResource)(nil)
	_ resource.ResourceWithConfigure   = (*imageAccessResource)(nil)
	_ resource.ResourceWithImportState = (*imageAccessResource)(nil)
)

// memberStatuses are the statuses Glance accepts for an image member.
var memberStatuses = []string{"pending", "accepted", "rejected"}

// NewImageAccessResource is the factory registered with the provider.
func NewImageAccessResource() resource.Resource {
	return &imageAccessResource{}
}

type imageAccessResource struct {
	config *clients.Config
}

// imageAccessModel is shared by pcd_images_image_access and
// pcd_images_image_access_accept: both describe one membership.
type imageAccessModel struct {
	ID        types.String `tfsdk:"id"`
	ImageID   types.String `tfsdk:"image_id"`
	MemberID  types.String `tfsdk:"member_id"`
	Status    types.String `tfsdk:"status"`
	CreatedAt types.String `tfsdk:"created_at"`
	UpdatedAt types.String `tfsdk:"updated_at"`
	Schema    types.String `tfsdk:"schema"`
	Region    types.String `tfsdk:"region"`
}

func (r *imageAccessResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_images_image_access"
}

func (r *imageAccessResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	forceNew := []planmodifier.String{stringplanmodifier.RequiresReplace()}
	stable := []planmodifier.String{stringplanmodifier.UseStateForUnknown()}
	resp.Schema = schema.Schema{
		MarkdownDescription: "Shares an image with another project: the owner's side of PCD image sharing (the UI's " +
			"image members). Glance accepts and serves members only while the image's `visibility` is `\"shared\"` (an image created " +
			"without `visibility` gets Glance's default, `\"shared\"`), and only the image's owner or an admin can add " +
			"one. The member starts `pending` until the other project accepts or rejects the image, which it does with " +
			"`pcd_images_image_access_accept` under a provider scoped to that project. An admin can instead set " +
			"`status` here to decide for the member. Use one or the other for a given membership, never both: each " +
			"would undo the other's status on every apply. A membership that already exists when the resource is " +
			"created is adopted, so destroying the resource removes it. Glance refuses to remove a member once the " +
			"image is no longer shared; destroying the resource then only drops it from state, with a warning, and " +
			"Glance keeps the membership, which grants access again if the image is shared again.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{Computed: true, PlanModifiers: stable,
				MarkdownDescription: "The composite `<image_id>/<member_id>` ID."},
			"image_id": schema.StringAttribute{Required: true, PlanModifiers: forceNew,
				MarkdownDescription: "The ID of the image to share. Its `visibility` must be `\"shared\"`. Changing this " +
					"forces a new resource."},
			"member_id": schema.StringAttribute{Required: true, PlanModifiers: forceNew,
				MarkdownDescription: "The ID of the project to share the image with. Changing this forces a new resource."},
			"status": schema.StringAttribute{Optional: true, Computed: true,
				Validators: []validator.String{stringvalidator.OneOf(memberStatuses...)},
				MarkdownDescription: "The member's status: `pending`, `accepted` or `rejected`. Leave it unset to let the " +
					"member project decide; it then reports that project's decision. Setting it needs the admin role, " +
					"and makes the provider enforce the value."},
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

func (r *imageAccessResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
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

func (r *imageAccessResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
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
	member, err := memberOf(members.Create(ctx, client, imageID, memberID).Extract())
	switch {
	case err == nil:
	case gophercloud.ResponseCodeIs(err, http.StatusConflict):
		// Glance keeps a membership when the image leaves "shared" and comes back,
		// and the UI may have added it first: adopt it instead of failing.
		member, err = memberOf(members.Get(ctx, client, imageID, memberID).Extract())
		if err != nil {
			resp.Diagnostics.AddError("images: reading existing image member", err.Error())
			return
		}
	case gophercloud.ResponseCodeIs(err, http.StatusForbidden):
		resp.Diagnostics.AddAttributeError(path.Root("image_id"), "Image cannot be shared",
			fmt.Sprintf("Glance refused to add project %s as a member of image %s (403). Members can be added only "+
				"while the image's visibility is \"shared\", by the image's owner or an admin. Glance said: %v",
				memberID, imageID, err))
		return
	default:
		resp.Diagnostics.AddError("images: adding image member", err.Error())
		return
	}

	plan.ID = types.StringValue(imageID + "/" + memberID)
	if plan.Region.IsNull() || plan.Region.IsUnknown() {
		plan.Region = types.StringValue(r.config.Region)
	}
	want := plan.Status
	setImageMember(&plan, member)
	// The member exists now. Record it before the status change, so a refused
	// status leaves the member in state (tainted) instead of behind Terraform's
	// back.
	if !tfstate.RecordCreated(ctx, resp, &plan) {
		return
	}

	if !want.IsNull() && !want.IsUnknown() && want.ValueString() != member.Status {
		member, err = memberOf(members.Update(ctx, client, imageID, memberID, members.UpdateOpts{Status: want.ValueString()}).Extract())
		if err != nil {
			resp.Diagnostics.AddAttributeError(path.Root("status"), "Setting the member status failed",
				fmt.Sprintf("Project %s was added to image %s, but setting its status to %q failed; an owner "+
					"needs the admin role to decide for a member. Glance said: %v", memberID, imageID, want.ValueString(), err))
			return
		}
		setImageMember(&plan, member)
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *imageAccessResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
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

// Update changes the member status, the only attribute that does not force
// replacement.
func (r *imageAccessResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
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
	var member *members.Member
	if !plan.Status.IsNull() && !plan.Status.IsUnknown() {
		member, err = memberOf(members.Update(ctx, client, imageID, memberID, members.UpdateOpts{Status: plan.Status.ValueString()}).Extract())
	} else {
		member, err = memberOf(members.Get(ctx, client, imageID, memberID).Extract())
	}
	if err != nil {
		resp.Diagnostics.AddError("images: updating image member", err.Error())
		return
	}
	setImageMember(&plan, member)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *imageAccessResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
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
	err = members.Delete(ctx, client, imageID, memberID).ExtractErr()
	switch {
	case err == nil, gophercloud.ResponseCodeIs(err, http.StatusNotFound):
	case gophercloud.ResponseCodeIs(err, http.StatusForbidden):
		gone, readErr := imageNoLongerShared(ctx, client, imageID)
		switch {
		case readErr != nil:
			resp.Diagnostics.AddError("images: removing image member",
				fmt.Sprintf("Glance refused to remove project %s from image %s (403), and reading the image to learn "+
					"why failed: %v. Glance said: %v", memberID, imageID, readErr, err))
		case !gone:
			resp.Diagnostics.AddError("images: removing image member",
				fmt.Sprintf("Glance refused to remove project %s from image %s (403) although the image is still "+
					"shared, so the membership stays. Only the image's owner or an admin can remove a member: check "+
					"the project this provider is scoped to. Glance said: %v", memberID, imageID, err))
		default:
			resp.Diagnostics.AddWarning("Image member left in place",
				fmt.Sprintf("Glance refused to remove project %s from image %s (403) because the image's visibility "+
					"is no longer \"shared\". The membership was removed from state; Glance keeps it, and it applies "+
					"again if the image is shared again.", memberID, imageID))
		}
	default:
		resp.Diagnostics.AddError("images: removing image member", err.Error())
	}
}

func (r *imageAccessResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	imageID, memberID, ok := strings.Cut(req.ID, "/")
	if !ok || imageID == "" || memberID == "" || strings.Contains(memberID, "/") {
		resp.Diagnostics.AddError("Invalid import ID", fmt.Sprintf("expected <image_id>/<member_id>, got %q", req.ID))
		return
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), req.ID)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("image_id"), imageID)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("member_id"), memberID)...)
}

// readImageMember refreshes m from Glance. gone reports a membership Glance no
// longer shows: a 404, which Glance also answers once the image's visibility
// leaves "shared".
func readImageMember(ctx context.Context, client *gophercloud.ServiceClient, m *imageAccessModel) (gone bool, diags diag.Diagnostics) {
	imageID := m.ImageID.ValueString()
	memberID := m.MemberID.ValueString()
	member, err := memberOf(members.Get(ctx, client, imageID, memberID).Extract())
	if err != nil {
		if gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
			diags.AddWarning("Image member not found",
				fmt.Sprintf("Project %s is no longer a member of image %s (removed, the image was deleted, or its "+
					"visibility is no longer \"shared\"); it was removed from state.", memberID, imageID))
			return true, diags
		}
		diags.AddError("images: reading image member", err.Error())
		return false, diags
	}
	m.ID = types.StringValue(imageID + "/" + memberID)
	setImageMember(m, member)
	return false, diags
}

// imageNoLongerShared tells the two reasons Glance answers 403 to a member
// DELETE or PUT apart. Either the image's visibility left "shared", so the
// membership is out of reach and gone for Terraform (true), or policy refused
// a caller that may not change the member, such as a provider scoped to the
// wrong project, and the membership is still in force (false). An image the
// caller can no longer see counts as no longer shared.
func imageNoLongerShared(ctx context.Context, client *gophercloud.ServiceClient, imageID string) (bool, error) {
	img, err := clients.RequireObject(images.Get(ctx, client, imageID).Extract())
	switch {
	case gophercloud.ResponseCodeIs(err, http.StatusNotFound):
		return true, nil
	case err != nil:
		return false, err
	case img.ID == "":
		// A 200 without the image is not a deleted image. RequireObject
		// catches an answer that decoded to nothing; one that decoded to an
		// image without an ID is refused the same way.
		return false, clients.ErrNoObject
	}
	return img.Visibility != images.ImageVisibilityShared, nil
}

// memberOf passes a member call's result through clients.RequireObject, and
// turns an answer that decoded to no member record (a 200 whose body is empty
// or holds no member) into clients.ErrNoObject. It is not a not-found, and
// copying it would save empty IDs to state.
func memberOf(member *members.Member, err error) (*members.Member, error) {
	member, err = clients.RequireObject(member, err)
	if err == nil && member.MemberID == "" {
		return nil, clients.ErrNoObject
	}
	return member, err
}

// setImageMember copies Glance's member record onto m.
func setImageMember(m *imageAccessModel, member *members.Member) {
	m.ImageID = types.StringValue(member.ImageID)
	m.MemberID = types.StringValue(member.MemberID)
	m.Status = types.StringValue(member.Status)
	m.CreatedAt = types.StringValue(member.CreatedAt.Format(time.RFC3339))
	m.UpdatedAt = types.StringValue(member.UpdatedAt.Format(time.RFC3339))
	m.Schema = types.StringValue(member.Schema)
}
