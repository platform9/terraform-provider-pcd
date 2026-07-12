// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0
//
// Ported from terraform-provider-openstack v3.4.0
// (openstack/resource_openstack_compute_servergroup_v2.go), adapted for the
// terraform-plugin-framework and PCD.

package compute

import (
	"context"
	"fmt"
	"net/http"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/servergroups"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/listplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/platform9/terraform-provider-pcd/internal/clients"
)

var (
	_ resource.Resource                = (*servergroupResource)(nil)
	_ resource.ResourceWithConfigure   = (*servergroupResource)(nil)
	_ resource.ResourceWithImportState = (*servergroupResource)(nil)
)

// NewServergroupResource is the factory registered with the provider.
func NewServergroupResource() resource.Resource {
	return &servergroupResource{}
}

type servergroupResource struct {
	config *clients.Config
}

type servergroupModel struct {
	ID       types.String `tfsdk:"id"`
	Name     types.String `tfsdk:"name"`
	Policies types.List   `tfsdk:"policies"`
	Members  types.List   `tfsdk:"members"`
	Region   types.String `tfsdk:"region"`
}

func (r *servergroupResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_compute_servergroup"
}

func (r *servergroupResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Manages a server group (affinity/anti-affinity policy) in PCD's Nova service. " +
			"Server groups are immutable; any change forces a new resource.",
		Attributes: map[string]schema.Attribute{
			"id":   schema.StringAttribute{Computed: true, MarkdownDescription: "The server group ID.", PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()}},
			"name": schema.StringAttribute{Required: true, MarkdownDescription: "The name of the server group.", PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()}},
			"policies": schema.ListAttribute{
				Required: true, ElementType: types.StringType,
				MarkdownDescription: "Scheduler policies (e.g. affinity, anti-affinity, soft-affinity, soft-anti-affinity).",
				PlanModifiers:       []planmodifier.List{listplanmodifier.RequiresReplace()},
			},
			"members": schema.ListAttribute{
				Computed: true, ElementType: types.StringType,
				MarkdownDescription: "Instance IDs that are members of this server group.",
				PlanModifiers:       []planmodifier.List{listplanmodifier.UseStateForUnknown()},
			},
			"region": schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "The region. Defaults to the provider's region.", PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()}},
		},
	}
}

func (r *servergroupResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.config = configureClient(req.ProviderData, &resp.Diagnostics)
}

func (r *servergroupResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan servergroupModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := r.config.ComputeV2Client()
	if err != nil {
		resp.Diagnostics.AddError("compute: building v2 client", err.Error())
		return
	}

	var policies []string
	resp.Diagnostics.Append(plan.Policies.ElementsAs(ctx, &policies, false)...)
	if resp.Diagnostics.HasError() {
		return
	}

	sg, err := servergroups.Create(ctx, client, servergroups.CreateOpts{
		Name:     plan.Name.ValueString(),
		Policies: policies,
	}).Extract()
	if err != nil {
		resp.Diagnostics.AddError("compute: creating server group", err.Error())
		return
	}

	resp.Diagnostics.Append(r.flatten(ctx, sg, &plan)...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *servergroupResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state servergroupModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := r.config.ComputeV2Client()
	if err != nil {
		resp.Diagnostics.AddError("compute: building v2 client", err.Error())
		return
	}

	sg, err := servergroups.Get(ctx, client, state.ID.ValueString()).Extract()
	if err != nil {
		if gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
			resp.Diagnostics.AddWarning("Server group not found",
				fmt.Sprintf("Server group %s no longer exists and was removed from state.", state.ID.ValueString()))
			resp.State.RemoveResource(ctx)
			return
		}
		resp.Diagnostics.AddError("compute: reading server group", err.Error())
		return
	}

	resp.Diagnostics.Append(r.flatten(ctx, sg, &state)...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

// Update is required by the interface but never invoked (server groups are immutable).
func (r *servergroupResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan servergroupModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *servergroupResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state servergroupModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := r.config.ComputeV2Client()
	if err != nil {
		resp.Diagnostics.AddError("compute: building v2 client", err.Error())
		return
	}

	if err := servergroups.Delete(ctx, client, state.ID.ValueString()).ExtractErr(); err != nil {
		if gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
			return
		}
		resp.Diagnostics.AddError("compute: deleting server group", err.Error())
	}
}

func (r *servergroupResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, path.Root("id"), req, resp)
}

func (r *servergroupResource) flatten(ctx context.Context, sg *servergroups.ServerGroup, m *servergroupModel) (diags diag.Diagnostics) {
	m.ID = types.StringValue(sg.ID)
	m.Name = types.StringValue(sg.Name)

	policies := sg.Policies
	if len(policies) == 0 && sg.Policy != nil && *sg.Policy != "" {
		policies = []string{*sg.Policy}
	}
	polList, d := types.ListValueFrom(ctx, types.StringType, policies)
	diags = append(diags, d...)
	m.Policies = polList

	members := sg.Members
	if members == nil {
		members = []string{}
	}
	memList, d := types.ListValueFrom(ctx, types.StringType, members)
	diags = append(diags, d...)
	m.Members = memList

	if m.Region.IsNull() || m.Region.IsUnknown() {
		m.Region = types.StringValue(r.config.Region)
	}
	return diags
}
