// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package blockstorage

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/blockstorage/v3/transfers"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/platform9/terraform-provider-pcd/internal/clients"
)

var (
	_ resource.Resource              = (*volumeTransferAcceptResource)(nil)
	_ resource.ResourceWithConfigure = (*volumeTransferAcceptResource)(nil)
)

// NewVolumeTransferAcceptResource is the factory registered with the provider.
func NewVolumeTransferAcceptResource() resource.Resource {
	return &volumeTransferAcceptResource{}
}

type volumeTransferAcceptResource struct {
	config *clients.Config
}

type volumeTransferAcceptModel struct {
	ID         types.String `tfsdk:"id"`
	TransferID types.String `tfsdk:"transfer_id"`
	AuthKey    types.String `tfsdk:"auth_key"`
	VolumeID   types.String `tfsdk:"volume_id"`
	Region     types.String `tfsdk:"region"`
}

func (r *volumeTransferAcceptResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_blockstorage_volume_transfer_accept"
}

func (r *volumeTransferAcceptResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	stable := []planmodifier.String{stringplanmodifier.UseStateForUnknown()}
	forceNew := []planmodifier.String{stringplanmodifier.RequiresReplace()}
	resp.Schema = schema.Schema{
		MarkdownDescription: "Accepts a volume transfer offered with `pcd_blockstorage_volume_transfer`, moving the " +
			"volume and its snapshots into the project the provider authenticates in. Use it through a provider " +
			"configuration for the receiving project, for example an aliased `provider \"pcd\"` block that sets " +
			"`tenant_id` or `tenant_name` (the receiving project must already exist when Terraform plans, because " +
			"the provider signs in during planning).\n\n" +
			"Accepting is a one-time event, and Cinder keeps nothing the receiving project can read afterward, so " +
			"this resource records the acceptance and never refreshes it. Destroying it calls nothing: the volume " +
			"stays in the receiving project, which manages it from then on (import it with its `volume_id`). It " +
			"cannot be imported.\n\n" +
			"**After the accept**, the offering configuration's blocks for the volume, the offer and the volume's " +
			"snapshots must become `removed` blocks (Terraform 1.7 or later) before that configuration's next apply. " +
			"With member credentials, its refresh no longer finds the moved volume, so Terraform plans a new volume " +
			"and a new offer for it there; because `transfer_id` and `auth_key` force replacement, the next apply of " +
			"this resource would then accept that new offer as well. With admin credentials, the offering " +
			"configuration keeps managing the moved volume, and a destroy there would delete it.\n\n" +
			"The accept fails when the key is wrong, when the offer was already accepted or canceled, or when the " +
			"receiving project's quotas cannot hold the volume and its snapshots: Cinder counts the volume, each " +
			"snapshot and their gigabytes against the project's overall and per-volume-type quotas, and checks the " +
			"volume's size against the per-volume limit.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed: true, PlanModifiers: stable,
				MarkdownDescription: "The accepted transfer's ID, the same as `transfer_id`.",
			},
			"transfer_id": schema.StringAttribute{
				Required: true, PlanModifiers: forceNew,
				MarkdownDescription: "The ID of the offer to accept (`pcd_blockstorage_volume_transfer.id`). " +
					"Changing this accepts a new offer.",
			},
			"auth_key": schema.StringAttribute{
				Required: true, Sensitive: true, PlanModifiers: forceNew,
				MarkdownDescription: "The offer's `auth_key`. It is stored in the Terraform state; it is useless once " +
					"the offer is accepted.",
			},
			"volume_id": schema.StringAttribute{
				Computed: true, PlanModifiers: stable,
				MarkdownDescription: "The ID of the volume the transfer moved into this project.",
			},
			"region": schema.StringAttribute{
				Optional: true, Computed: true,
				PlanModifiers:       []planmodifier.String{stringplanmodifier.RequiresReplace(), stringplanmodifier.UseStateForUnknown()},
				MarkdownDescription: "The region. Defaults to the provider's region. Changing this forces a new resource.",
			},
		},
	}
}

func (r *volumeTransferAcceptResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.config = configureClient(req.ProviderData, &resp.Diagnostics)
}

func (r *volumeTransferAcceptResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan volumeTransferAcceptModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := r.config.ForRegion(plan.Region.ValueString()).BlockStorageV3Client()
	if err != nil {
		resp.Diagnostics.AddError("blockstorage: building v3 client", err.Error())
		return
	}

	// A 202 without the transfer, or without the volume it moved, decodes to an
	// empty transfer, which names no volume to record.
	transfer, err := clients.RequireObject(transfers.Accept(ctx, client, plan.TransferID.ValueString(),
		transfers.AcceptOpts{AuthKey: plan.AuthKey.ValueString()}).Extract())
	if err == nil && transfer.VolumeID == "" {
		err = clients.ErrNoObject
	}
	if err != nil {
		resp.Diagnostics.AddError("blockstorage: accepting volume transfer", err.Error()+transferAcceptLostHint(err))
		return
	}

	// The volume belongs to this project when Cinder answers: nothing to wait for.
	plan.ID = plan.TransferID
	plan.VolumeID = types.StringValue(transfer.VolumeID)
	if plan.Region.IsNull() || plan.Region.IsUnknown() {
		plan.Region = types.StringValue(r.config.Region)
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

// Read keeps the recorded acceptance as it is. Cinder hides a transfer once it
// is accepted, and reading the volume instead would drop this resource when the
// volume is deleted later, after which Terraform would plan an accept that can
// never succeed again.
func (r *volumeTransferAcceptResource) Read(_ context.Context, _ resource.ReadRequest, _ *resource.ReadResponse) {
}

// Update is unreachable: every argument forces a new acceptance, and an
// acceptance cannot be changed.
func (r *volumeTransferAcceptResource) Update(_ context.Context, _ resource.UpdateRequest, resp *resource.UpdateResponse) {
	resp.Diagnostics.AddError("blockstorage: volume transfer accept update",
		"transfer_id, auth_key and region force replacement; this should not be reached.")
}

// Delete removes the acceptance from state only: a transfer cannot be undone.
func (r *volumeTransferAcceptResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state volumeTransferAcceptModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.AddWarning("Volume transfer acceptance cannot be undone",
		fmt.Sprintf("Removed the acceptance of transfer %s from state only. Volume %s stays in the project that "+
			"accepted it; to move it back, offer it again with pcd_blockstorage_volume_transfer.",
			state.ID.ValueString(), state.VolumeID.ValueString()))
}

// transferAcceptLostHint explains an accept error that an earlier accept may
// have caused without being recorded: Cinder hides a spent transfer, so a
// second accept answers 404, and an answer without the volume may still have
// moved it.
func transferAcceptLostHint(err error) string {
	if !errors.Is(err, clients.ErrNoObject) && !gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
		return ""
	}
	return "\n\nIf an earlier apply reached Cinder, the volume is already in this project: import it with " +
		"pcd_blockstorage_volume (the offer's volume_id) and remove this resource."
}
