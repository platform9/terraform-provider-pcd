// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package blockstorage

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/blockstorage/v3/transfers"
	"github.com/gophercloud/gophercloud/v2/openstack/blockstorage/v3/volumes"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
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
	_ resource.Resource                = (*volumeTransferResource)(nil)
	_ resource.ResourceWithConfigure   = (*volumeTransferResource)(nil)
	_ resource.ResourceWithImportState = (*volumeTransferResource)(nil)
)

// NewVolumeTransferResource is the factory registered with the provider.
func NewVolumeTransferResource() resource.Resource {
	return &volumeTransferResource{}
}

type volumeTransferResource struct {
	config *clients.Config
}

type volumeTransferModel struct {
	ID              types.String `tfsdk:"id"`
	VolumeID        types.String `tfsdk:"volume_id"`
	Name            types.String `tfsdk:"name"`
	AuthKey         types.String `tfsdk:"auth_key"`
	SourceProjectID types.String `tfsdk:"source_project_id"`
	Accepted        types.Bool   `tfsdk:"accepted"`
	Region          types.String `tfsdk:"region"`
}

// transferHistory holds the fields a transfer carries from volume API 3.57,
// which gophercloud's Transfer does not decode.
type transferHistory struct {
	SourceProjectID string `json:"source_project_id"`
}

func (r *volumeTransferResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_blockstorage_volume_transfer"
}

func (r *volumeTransferResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	stable := []planmodifier.String{stringplanmodifier.UseStateForUnknown()}
	forceNew := []planmodifier.String{stringplanmodifier.RequiresReplace()}
	resp.Schema = schema.Schema{
		MarkdownDescription: "Offers a Cinder volume for transfer to another project. Creating this resource creates " +
			"the offer and records its `auth_key`; a configuration that authenticates in the receiving project accepts " +
			"it with `pcd_blockstorage_volume_transfer_accept`, which moves the volume, and every snapshot of it, into " +
			"that project. Destroying this resource before the offer is accepted cancels the offer.\n\n" +
			"The volume must be `available` (not attached), and so must each of its snapshots. While the offer is " +
			"pending the volume's status is `awaiting-transfer`, and Cinder refuses to attach, extend or retype it. " +
			"Encrypted volumes cannot be transferred.\n\n" +
			"Anyone who has the transfer ID and `auth_key` can accept the volume into a project they belong to until " +
			"the offer is accepted or canceled. The key is stored in the Terraform state in plain text, so protect " +
			"the state accordingly.\n\n" +
			"A transfer moves a volume between projects. Accepting an offer in the project that made it is not " +
			"supported: Cinder then reports the transfer exactly like a canceled one, and this resource leaves the " +
			"state.\n\n" +
			"**After the offer is accepted**, the offering configuration still declares a volume that now belongs to " +
			"another project. A project member can no longer read it, so Terraform drops the volume from state and " +
			"plans a new, empty volume, and, because this resource refers to the volume, a new offer for it. An admin " +
			"still reads it, so the configuration keeps managing it, and a destroy deletes it from its new project. " +
			"Either way, replace the blocks of the volume, of this resource and of any `pcd_blockstorage_snapshot` of " +
			"the volume with `removed` blocks that keep the objects (Terraform 1.7 or later; use `terraform state rm` " +
			"before that), and import the volume where it now belongs. The examples here and on the " +
			"`pcd_blockstorage_volume_transfer_accept` page show both sides.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed: true, PlanModifiers: stable,
				MarkdownDescription: "The transfer ID. The receiving project needs it, with `auth_key`, to accept the offer.",
			},
			"volume_id": schema.StringAttribute{
				Required: true, PlanModifiers: forceNew,
				MarkdownDescription: "The volume to offer. Changing this forces a new offer.",
			},
			"name": schema.StringAttribute{
				Optional: true, PlanModifiers: forceNew,
				// Cinder strips the name and never receives an empty one, so such a
				// value would never match what Read reports, and every plan would
				// replace the offer.
				Validators: []validator.String{stringvalidator.RegexMatches(regexp.MustCompile(`^\S(.*\S)?$`),
					"must not be empty or start or end with whitespace, because Cinder strips the name")},
				MarkdownDescription: "A name for the offer, not empty and without leading or trailing whitespace. " +
					"Changing this forces a new offer: Cinder cannot change a transfer.",
			},
			"auth_key": schema.StringAttribute{
				Computed: true, Sensitive: true, PlanModifiers: stable,
				MarkdownDescription: "The secret that accepts the offer. Cinder returns it only when the offer is " +
					"created, so it is null after an import.",
			},
			"source_project_id": schema.StringAttribute{
				Computed: true, PlanModifiers: stable,
				MarkdownDescription: "The project the volume belonged to when it was offered.",
			},
			"accepted": schema.BoolAttribute{
				Computed: true,
				MarkdownDescription: "Whether the offer was accepted. Cinder stops reporting a transfer once it is " +
					"accepted or canceled, so the provider decides from the volume: it was accepted when the volume is " +
					"no longer visible or belongs to another project, and canceled when the volume is still in the " +
					"source project (the resource then leaves the state). An offer canceled outside Terraform whose " +
					"volume was then deleted also reads as accepted.",
			},
			"region": schema.StringAttribute{
				Optional: true, Computed: true,
				PlanModifiers:       []planmodifier.String{stringplanmodifier.RequiresReplace(), stringplanmodifier.UseStateForUnknown()},
				MarkdownDescription: "The region. Defaults to the provider's region. Changing this forces a new resource.",
			},
		},
	}
}

func (r *volumeTransferResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.config = configureClient(req.ProviderData, &resp.Diagnostics)
}

func (r *volumeTransferResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan volumeTransferModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := r.transferClient(plan.Region.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("blockstorage: building v3 client", err.Error())
		return
	}

	res := transfers.Create(ctx, client, transfers.CreateOpts{
		VolumeID: plan.VolumeID.ValueString(),
		Name:     plan.Name.ValueString(),
	})
	// A 202 without the object decodes to an empty transfer, which names no
	// offer to record.
	transfer, err := clients.RequireObject(res.Extract())
	if err == nil && transfer.ID == "" {
		err = clients.ErrNoObject
	}
	if err != nil {
		resp.Diagnostics.AddError("blockstorage: creating volume transfer", err.Error()+transferPendingOfferHint(err))
		return
	}

	// The offer is complete when Cinder answers: nothing to wait for. Cinder
	// holds it from here on, and the auth key exists only in this answer, so
	// record both before anything else can fail.
	plan.ID = types.StringValue(transfer.ID)
	plan.AuthKey = types.StringValue(transfer.AuthKey)
	plan.Accepted = types.BoolValue(false)
	if plan.Region.IsNull() || plan.Region.IsUnknown() {
		plan.Region = types.StringValue(r.config.Region)
	}
	if !tfstate.RecordCreated(ctx, resp, &plan) {
		return
	}

	var history transferHistory
	if err := res.ExtractInto(&history); err != nil {
		resp.Diagnostics.AddError("blockstorage: reading the created volume transfer", err.Error())
		return // leave the row RecordCreated wrote in place
	}
	plan.SourceProjectID = optionalStr(history.SourceProjectID)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *volumeTransferResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state volumeTransferModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if tfstate.DropRowWithoutID(ctx, resp, state.ID, "volume transfer", state.Name.ValueString()) {
		return
	}
	// An accepted offer is spent. Cinder keeps nothing to read, and the volume
	// may come back to this project through a later transfer, which must not
	// make this offer look canceled.
	if state.Accepted.ValueBool() {
		return
	}

	client, err := r.transferClient(state.Region.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("blockstorage: building v3 client", err.Error())
		return
	}

	res := transfers.Get(ctx, client, state.ID.ValueString())
	// A 200 without the object decodes to an empty transfer. It is not a
	// not-found: saving it would lose the offer and its auth key.
	transfer, err := clients.RequireObject(res.Extract())
	if err == nil && transfer.ID == "" {
		err = clients.ErrNoObject
	}
	if err == nil {
		var history transferHistory
		if err := res.ExtractInto(&history); err != nil {
			resp.Diagnostics.AddError("blockstorage: reading volume transfer", err.Error())
			return
		}
		state.VolumeID = types.StringValue(transfer.VolumeID)
		state.Name = optionalStr(transfer.Name)
		if history.SourceProjectID != "" {
			state.SourceProjectID = types.StringValue(history.SourceProjectID)
		}
		state.Accepted = types.BoolValue(false)
		if state.Region.IsNull() || state.Region.IsUnknown() {
			state.Region = types.StringValue(r.config.Region)
		}
		resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
		return
	}
	if !gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
		resp.Diagnostics.AddError("blockstorage: reading volume transfer", err.Error())
		return
	}

	// The transfer is gone. Cinder hides it once it is accepted or canceled;
	// the volume says which.
	volumeID := state.VolumeID.ValueString()
	if volumeID == "" {
		// An import of a transfer that does not exist.
		resp.State.RemoveResource(ctx)
		return
	}
	visible, volumeProject := true, ""
	// A 200 without the object says nothing about where the volume is, so it
	// is an error, not a volume still in the source project.
	vol, err := clients.RequireObject(volumes.Get(ctx, client, volumeID).Extract())
	if err == nil && vol.ID == "" {
		err = clients.ErrNoObject
	}
	switch {
	case err == nil:
		volumeProject = vol.TenantID
	case gophercloud.ResponseCodeIs(err, http.StatusNotFound):
		visible = false
	default:
		resp.Diagnostics.AddError("blockstorage: reading the transferred volume", err.Error())
		return
	}
	if !offerAccepted(visible, volumeProject, state.SourceProjectID.ValueString()) {
		resp.Diagnostics.AddWarning("Volume transfer canceled",
			fmt.Sprintf("Transfer %s no longer exists and volume %s is still in the source project, so the offer "+
				"was canceled outside Terraform. It was removed from state; the next apply offers the volume again.",
				state.ID.ValueString(), volumeID))
		resp.State.RemoveResource(ctx)
		return
	}
	state.Accepted = types.BoolValue(true)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

// Update is unreachable: every argument forces a new offer, and Cinder cannot
// change a transfer.
func (r *volumeTransferResource) Update(_ context.Context, _ resource.UpdateRequest, resp *resource.UpdateResponse) {
	resp.Diagnostics.AddError("blockstorage: volume transfer update",
		"volume_id, name and region force replacement; this should not be reached.")
}

// Delete cancels the offer. A transfer that is already gone, accepted or
// canceled, needs nothing more.
func (r *volumeTransferResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state volumeTransferModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if tfstate.SkipDeleteWithoutID(resp, state.ID, "volume transfer", state.Name.ValueString()) {
		return
	}

	client, err := r.transferClient(state.Region.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("blockstorage: building v3 client", err.Error())
		return
	}
	if err := transfers.Delete(ctx, client, state.ID.ValueString()).ExtractErr(); err != nil {
		if gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
			return
		}
		resp.Diagnostics.AddError("blockstorage: canceling volume transfer", err.Error())
	}
}

func (r *volumeTransferResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, path.Root("id"), req, resp)
}

// transferClient returns the Cinder client pinned to the version whose
// transfers report source_project_id.
func (r *volumeTransferResource) transferClient(region string) (*gophercloud.ServiceClient, error) {
	client, err := r.config.ForRegion(region).BlockStorageV3Client()
	if err != nil {
		return nil, err
	}
	return withMicroversion(client, blockstorageMicroversionTransferResponse), nil
}

// transferPendingOfferHint explains Cinder's refusal to offer a volume that
// is not available, which may mean the volume awaits an earlier offer, such
// as one an interrupted apply left behind.
func transferPendingOfferHint(err error) string {
	if !gophercloud.ResponseCodeIs(err, http.StatusBadRequest) || !strings.Contains(err.Error(), "Invalid volume: status must be available") {
		return ""
	}
	return "\n\nIf the volume's status is awaiting-transfer, an earlier offer for it is still pending: find it with " +
		"\"pcdctl volume transfer request list\" and cancel it with \"pcdctl volume transfer request delete " +
		"<transfer_id>\". Import it into this resource instead only if you still have its auth_key elsewhere: an " +
		"imported offer's auth_key stays empty, and the offer cannot be accepted without it."
}

// offerAccepted decides what a vanished transfer means. An accepted transfer
// moved the volume to another project, where a project member can no longer
// see it and an admin sees its new owner. A canceled one left it in the
// source project. Without a source project to compare with, a visible volume
// counts as not moved.
func offerAccepted(volumeVisible bool, volumeProjectID, sourceProjectID string) bool {
	if !volumeVisible {
		return true
	}
	return volumeProjectID != "" && sourceProjectID != "" && volumeProjectID != sourceProjectID
}
