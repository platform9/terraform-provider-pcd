// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0
//
// Ported from terraform-provider-openstack v3.4.0
// (openstack/resource_openstack_vpnaas_endpoint_group_v2.go), adapted for the
// terraform-plugin-framework and PCD.

package vpnaas

import (
	"context"
	"fmt"
	"net/http"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/extensions/vpnaas/endpointgroups"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/setplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/platform9/terraform-provider-pcd/internal/clients"
)

var (
	_ resource.Resource                = (*endpointGroupResource)(nil)
	_ resource.ResourceWithConfigure   = (*endpointGroupResource)(nil)
	_ resource.ResourceWithImportState = (*endpointGroupResource)(nil)
)

// NewEndpointGroupResource is the factory registered with the provider.
func NewEndpointGroupResource() resource.Resource {
	return &endpointGroupResource{}
}

type endpointGroupResource struct {
	config *clients.Config
}

type endpointGroupModel struct {
	ID          types.String `tfsdk:"id"`
	Region      types.String `tfsdk:"region"`
	Name        types.String `tfsdk:"name"`
	Description types.String `tfsdk:"description"`
	Type        types.String `tfsdk:"type"`
	Endpoints   types.Set    `tfsdk:"endpoints"`
	TenantID    types.String `tfsdk:"tenant_id"`
}

func (r *endpointGroupResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_vpnaas_endpoint_group"
}

func (r *endpointGroupResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	useState := []planmodifier.String{stringplanmodifier.UseStateForUnknown()}
	resp.Schema = schema.Schema{
		MarkdownDescription: "Manages a Neutron VPN endpoint group (a set of local subnets or peer CIDRs) in PCD.",
		Attributes: map[string]schema.Attribute{
			"id":          schema.StringAttribute{Computed: true, MarkdownDescription: "The endpoint group ID.", PlanModifiers: useState},
			"region":      schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "The region. Defaults to the provider's region.", PlanModifiers: useState},
			"name":        schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "The name of the endpoint group.", PlanModifiers: useState},
			"description": schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "A description of the endpoint group.", PlanModifiers: useState},
			"type": schema.StringAttribute{
				Optional:            true,
				Computed:            true,
				MarkdownDescription: "The endpoint type: subnet, cidr, network, router, or vlan (default cidr). Changing this forces a new resource.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.RequiresReplace(), stringplanmodifier.UseStateForUnknown()},
			},
			"endpoints": schema.SetAttribute{
				Required:            true,
				ElementType:         types.StringType,
				MarkdownDescription: "The list of endpoints of the given type. Changing this forces a new resource.",
				PlanModifiers:       []planmodifier.Set{setplanmodifier.RequiresReplace()},
			},
			"tenant_id": schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "The owning project. Changing this forces a new resource.", PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace(), stringplanmodifier.UseStateForUnknown()}},
		},
	}
}

func (r *endpointGroupResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.config = configureClient(req.ProviderData, &resp.Diagnostics)
}

func (r *endpointGroupResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan endpointGroupModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := r.config.NetworkV2Client()
	if err != nil {
		resp.Diagnostics.AddError("vpnaas: building network client", err.Error())
		return
	}

	var endpoints []string
	resp.Diagnostics.Append(plan.Endpoints.ElementsAs(ctx, &endpoints, false)...)
	if resp.Diagnostics.HasError() {
		return
	}

	group, err := endpointgroups.Create(ctx, client, endpointgroups.CreateOpts{
		Name:        plan.Name.ValueString(),
		Description: plan.Description.ValueString(),
		TenantID:    plan.TenantID.ValueString(),
		Type:        endpointgroups.EndpointType(plan.Type.ValueString()),
		Endpoints:   endpoints,
	}).Extract()
	if err != nil {
		resp.Diagnostics.AddError("vpnaas: creating endpoint group", err.Error())
		return
	}

	resp.Diagnostics.Append(r.readInto(ctx, client, group.ID, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *endpointGroupResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state endpointGroupModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := r.config.NetworkV2Client()
	if err != nil {
		resp.Diagnostics.AddError("vpnaas: building network client", err.Error())
		return
	}

	notFound, diags := r.get(ctx, client, state.ID.ValueString(), &state)
	if notFound {
		resp.State.RemoveResource(ctx)
		return
	}
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *endpointGroupResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state endpointGroupModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := r.config.NetworkV2Client()
	if err != nil {
		resp.Diagnostics.AddError("vpnaas: building network client", err.Error())
		return
	}

	opts := endpointgroups.UpdateOpts{}
	if !plan.Name.Equal(state.Name) {
		v := plan.Name.ValueString()
		opts.Name = &v
	}
	if !plan.Description.Equal(state.Description) {
		v := plan.Description.ValueString()
		opts.Description = &v
	}

	id := plan.ID.ValueString()
	if _, err := endpointgroups.Update(ctx, client, id, opts).Extract(); err != nil {
		resp.Diagnostics.AddError("vpnaas: updating endpoint group", err.Error())
		return
	}

	resp.Diagnostics.Append(r.readInto(ctx, client, id, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *endpointGroupResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state endpointGroupModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := r.config.NetworkV2Client()
	if err != nil {
		resp.Diagnostics.AddError("vpnaas: building network client", err.Error())
		return
	}

	if err := endpointgroups.Delete(ctx, client, state.ID.ValueString()).ExtractErr(); err != nil {
		if gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
			return
		}
		resp.Diagnostics.AddError("vpnaas: deleting endpoint group", err.Error())
	}
}

func (r *endpointGroupResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, path.Root("id"), req, resp)
}

func (r *endpointGroupResource) readInto(ctx context.Context, client *gophercloud.ServiceClient, id string, m *endpointGroupModel) diag.Diagnostics {
	notFound, diags := r.get(ctx, client, id, m)
	if notFound {
		diags.AddError("vpnaas: endpoint group not found after write",
			fmt.Sprintf("Endpoint group %s was not found immediately after a create/update.", id))
	}
	return diags
}

func (r *endpointGroupResource) get(ctx context.Context, client *gophercloud.ServiceClient, id string, m *endpointGroupModel) (notFound bool, diags diag.Diagnostics) {
	group, err := endpointgroups.Get(ctx, client, id).Extract()
	if err != nil {
		if gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
			return true, diags
		}
		diags.AddError("vpnaas: reading endpoint group", err.Error())
		return false, diags
	}

	m.ID = types.StringValue(group.ID)
	m.Name = types.StringValue(group.Name)
	m.Description = types.StringValue(group.Description)
	m.Type = types.StringValue(group.Type)
	m.TenantID = types.StringValue(group.TenantID)

	eps := group.Endpoints
	if eps == nil {
		eps = []string{}
	}
	epSet, d := types.SetValueFrom(ctx, types.StringType, eps)
	diags = append(diags, d...)
	m.Endpoints = epSet

	if m.Region.IsNull() || m.Region.IsUnknown() {
		m.Region = types.StringValue(r.config.Region)
	}
	return false, diags
}
