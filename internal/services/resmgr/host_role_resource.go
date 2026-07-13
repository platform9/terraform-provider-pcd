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
	_ resource.Resource                = (*hostRoleResource)(nil)
	_ resource.ResourceWithConfigure   = (*hostRoleResource)(nil)
	_ resource.ResourceWithImportState = (*hostRoleResource)(nil)
)

// NewHostRoleResource is the factory registered with the provider.
func NewHostRoleResource() resource.Resource {
	return &hostRoleResource{}
}

type hostRoleResource struct {
	config *clients.Config
}

type hostRoleModel struct {
	ID       types.String `tfsdk:"id"`
	HostID   types.String `tfsdk:"host_id"`
	RoleName types.String `tfsdk:"role_name"`
}

func (r *hostRoleResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_host_role"
}

func (r *hostRoleResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	forceNew := []planmodifier.String{stringplanmodifier.RequiresReplace()}
	resp.Schema = schema.Schema{
		MarkdownDescription: "Assigns a PCD role (e.g. `pf9-ostackhost-neutron`) to a host. Use one resource per " +
			"host↔role pair. Role-specific settings are applied with their defaults.",
		Attributes: map[string]schema.Attribute{
			"id":        schema.StringAttribute{Computed: true, MarkdownDescription: "The composite `<host_id>/<role_name>` ID.", PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()}},
			"host_id":   schema.StringAttribute{Required: true, MarkdownDescription: "The host to assign the role to. Changing this forces a new resource.", PlanModifiers: forceNew},
			"role_name": schema.StringAttribute{Required: true, MarkdownDescription: "The role name (e.g. `pf9-ostackhost-neutron`). Changing this forces a new resource.", PlanModifiers: forceNew},
		},
	}
}

func (r *hostRoleResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.config = configureClient(req.ProviderData, &resp.Diagnostics)
}

func (r *hostRoleResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan hostRoleModel
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
	roleName := plan.RoleName.ValueString()
	url := client.ServiceURL("hosts", hostID, "roles", roleName)
	if _, err := client.Put(ctx, url, map[string]any{}, nil, &gophercloud.RequestOpts{OkCodes: []int{200, 201, 202, 204}}); err != nil {
		resp.Diagnostics.AddError("resmgr: assigning host role", err.Error())
		return
	}

	plan.ID = types.StringValue(hostID + "/" + roleName)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *hostRoleResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state hostRoleModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := r.config.ResmgrV2Client()
	if err != nil {
		resp.Diagnostics.AddError("resmgr: building client", err.Error())
		return
	}

	has, err := hostHasRole(ctx, client, state.HostID.ValueString(), state.RoleName.ValueString())
	if err != nil {
		if isNotFound(err) {
			resp.State.RemoveResource(ctx)
			return
		}
		resp.Diagnostics.AddError("resmgr: reading host roles", err.Error())
		return
	}
	if !has {
		resp.State.RemoveResource(ctx)
		return
	}

	state.ID = types.StringValue(state.HostID.ValueString() + "/" + state.RoleName.ValueString())
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

// Update is unreachable: host_id and role_name both force replacement.
func (r *hostRoleResource) Update(_ context.Context, _ resource.UpdateRequest, resp *resource.UpdateResponse) {
	resp.Diagnostics.AddError("resmgr: host role update", "host_id and role_name are immutable; this should not be reached.")
}

func (r *hostRoleResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state hostRoleModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := r.config.ResmgrV2Client()
	if err != nil {
		resp.Diagnostics.AddError("resmgr: building client", err.Error())
		return
	}

	url := client.ServiceURL("hosts", state.HostID.ValueString(), "roles", state.RoleName.ValueString())
	if _, err := client.Delete(ctx, url, &gophercloud.RequestOpts{OkCodes: []int{200, 202, 204}}); err != nil {
		if isNotFound(err) {
			return
		}
		resp.Diagnostics.AddError("resmgr: removing host role", err.Error())
	}
}

func (r *hostRoleResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	parts := strings.SplitN(req.ID, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		resp.Diagnostics.AddError("Invalid import ID", fmt.Sprintf("expected <host_id>/<role_name>, got %q", req.ID))
		return
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), req.ID)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("host_id"), parts[0])...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("role_name"), parts[1])...)
}

// hostAPI is the subset of GET /hosts/{id} we consume.
type hostAPI struct {
	ID           string   `json:"id"`
	Roles        []string `json:"roles"`
	HostConfigID string   `json:"hostconfig_id"`
}

func hostHasRole(ctx context.Context, client *gophercloud.ServiceClient, hostID, roleName string) (bool, error) {
	var host hostAPI
	if err := getJSON(ctx, client, client.ServiceURL("hosts", hostID), &host); err != nil {
		return false, err
	}
	for _, r := range host.Roles {
		if r == roleName {
			return true, nil
		}
	}
	return false, nil
}
