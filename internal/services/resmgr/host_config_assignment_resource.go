// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package resmgr

import (
	"context"
	"fmt"
	"strings"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/platform9/terraform-provider-pcd/internal/clients"
)

var (
	_ resource.Resource                = (*hostConfigAssignmentResource)(nil)
	_ resource.ResourceWithConfigure   = (*hostConfigAssignmentResource)(nil)
	_ resource.ResourceWithImportState = (*hostConfigAssignmentResource)(nil)
)

// NewHostConfigAssignmentResource is the factory registered with the provider.
func NewHostConfigAssignmentResource() resource.Resource {
	return &hostConfigAssignmentResource{}
}

type hostConfigAssignmentResource struct {
	config *clients.Config
}

type hostConfigAssignmentModel struct {
	ID           types.String `tfsdk:"id"`
	HostID       types.String `tfsdk:"host_id"`
	HostConfigID types.String `tfsdk:"host_config_id"`
}

func (r *hostConfigAssignmentResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_host_config_assignment"
}

func (r *hostConfigAssignmentResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	forceNew := []planmodifier.String{stringplanmodifier.RequiresReplace()}
	resp.Schema = schema.Schema{
		MarkdownDescription: "Assigns a host configuration to a host (the network-interface/label config a host uses).",
		Attributes: map[string]schema.Attribute{
			"id":             schema.StringAttribute{Computed: true, MarkdownDescription: "The composite `<host_id>/<host_config_id>` ID.", PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()}},
			"host_id":        schema.StringAttribute{Required: true, MarkdownDescription: "The host to assign the config to. Changing this forces a new resource.", PlanModifiers: forceNew},
			"host_config_id": schema.StringAttribute{Required: true, MarkdownDescription: "The host configuration to assign. Changing this forces a new resource.", PlanModifiers: forceNew},
		},
	}
}

func (r *hostConfigAssignmentResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.config = configureClient(req.ProviderData, &resp.Diagnostics)
}

func (r *hostConfigAssignmentResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan hostConfigAssignmentModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := r.config.ResmgrV2Client()
	if err != nil {
		resp.Diagnostics.AddError("resmgr: building client", err.Error())
		return
	}

	hostID := plan.HostID.ValueString()
	hcID := plan.HostConfigID.ValueString()
	url := client.ServiceURL("hosts", hostID, "hostconfig", hcID)
	if _, err := client.Put(ctx, url, map[string]any{}, nil, &gophercloud.RequestOpts{OkCodes: []int{200, 201, 202, 204}}); err != nil {
		resp.Diagnostics.AddError("resmgr: assigning host config", err.Error())
		return
	}

	plan.ID = types.StringValue(hostID + "/" + hcID)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *hostConfigAssignmentResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state hostConfigAssignmentModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := r.config.ResmgrV2Client()
	if err != nil {
		resp.Diagnostics.AddError("resmgr: building client", err.Error())
		return
	}

	var host hostAPI
	if err := getJSON(ctx, client, client.ServiceURL("hosts", state.HostID.ValueString()), &host); err != nil {
		if isNotFound(err) {
			resp.State.RemoveResource(ctx)
			return
		}
		resp.Diagnostics.AddError("resmgr: reading host", err.Error())
		return
	}
	if host.HostConfigID != state.HostConfigID.ValueString() {
		resp.State.RemoveResource(ctx)
		return
	}

	state.ID = types.StringValue(state.HostID.ValueString() + "/" + state.HostConfigID.ValueString())
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

// Update is unreachable: host_id and host_config_id both force replacement.
func (r *hostConfigAssignmentResource) Update(_ context.Context, _ resource.UpdateRequest, resp *resource.UpdateResponse) {
	resp.Diagnostics.AddError("resmgr: host config assignment update", "host_id and host_config_id are immutable; this should not be reached.")
}

func (r *hostConfigAssignmentResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state hostConfigAssignmentModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := r.config.ResmgrV2Client()
	if err != nil {
		resp.Diagnostics.AddError("resmgr: building client", err.Error())
		return
	}

	url := client.ServiceURL("hosts", state.HostID.ValueString(), "hostconfig", state.HostConfigID.ValueString())
	if _, err := client.Delete(ctx, url, &gophercloud.RequestOpts{OkCodes: []int{200, 202, 204}}); err != nil {
		if isNotFound(err) {
			return
		}
		resp.Diagnostics.AddError("resmgr: unassigning host config", err.Error())
	}
}

func (r *hostConfigAssignmentResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	parts := strings.SplitN(req.ID, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		resp.Diagnostics.AddError("Invalid import ID", fmt.Sprintf("expected <host_id>/<host_config_id>, got %q", req.ID))
		return
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), req.ID)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("host_id"), parts[0])...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("host_config_id"), parts[1])...)
}
