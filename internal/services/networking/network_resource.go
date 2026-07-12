// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0
//
// Ported from terraform-provider-openstack v3.4.0
// (openstack/resource_openstack_networking_network_v2.go), adapted for the
// terraform-plugin-framework and PCD.

package networking

import (
	"context"
	"fmt"
	"net/http"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/extensions/external"
	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/networks"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/setplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/platform9/terraform-provider-pcd/internal/clients"
)

var (
	_ resource.Resource                = (*networkResource)(nil)
	_ resource.ResourceWithConfigure   = (*networkResource)(nil)
	_ resource.ResourceWithImportState = (*networkResource)(nil)
)

// NewNetworkResource is the factory registered with the provider.
func NewNetworkResource() resource.Resource {
	return &networkResource{}
}

type networkResource struct {
	config *clients.Config
}

type networkModel struct {
	ID           types.String `tfsdk:"id"`
	Name         types.String `tfsdk:"name"`
	Description  types.String `tfsdk:"description"`
	AdminStateUp types.Bool   `tfsdk:"admin_state_up"`
	Shared       types.Bool   `tfsdk:"shared"`
	External     types.Bool   `tfsdk:"external"`
	TenantID     types.String `tfsdk:"tenant_id"`
	Tags         types.Set    `tfsdk:"tags"`
	Region       types.String `tfsdk:"region"`
}

// networkExtended embeds the base network plus the external-router extension so
// a single Get returns everything.
type networkExtended struct {
	networks.Network
	external.NetworkExternalExt
}

func (r *networkResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_networking_network"
}

func (r *networkResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Manages a Neutron network in PCD.",
		Attributes: map[string]schema.Attribute{
			"id":   schema.StringAttribute{Computed: true, MarkdownDescription: "The network ID.", PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()}},
			"name": schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "The name of the network.", PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()}},
			"description": schema.StringAttribute{
				Optional: true, Computed: true, MarkdownDescription: "A description of the network.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"admin_state_up": schema.BoolAttribute{Optional: true, Computed: true, Default: booldefault.StaticBool(true), MarkdownDescription: "The administrative state of the network."},
			"shared":         schema.BoolAttribute{Optional: true, Computed: true, MarkdownDescription: "Whether the network is shared across projects."},
			"external":       schema.BoolAttribute{Optional: true, Computed: true, MarkdownDescription: "Whether the network has an external routing facility."},
			"tenant_id": schema.StringAttribute{
				Optional: true, Computed: true, MarkdownDescription: "The owning project. Changing this forces a new resource.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace(), stringplanmodifier.UseStateForUnknown()},
			},
			"tags":   schema.SetAttribute{Optional: true, Computed: true, ElementType: types.StringType, MarkdownDescription: "Tags applied to the network.", PlanModifiers: []planmodifier.Set{setplanmodifier.UseStateForUnknown()}},
			"region": schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "The region. Defaults to the provider's region.", PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()}},
		},
	}
}

func (r *networkResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.config = configureClient(req.ProviderData, &resp.Diagnostics)
}

func (r *networkResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan networkModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := r.config.NetworkV2Client()
	if err != nil {
		resp.Diagnostics.AddError("networking: building v2 client", err.Error())
		return
	}

	adminUp := plan.AdminStateUp.ValueBool()
	base := networks.CreateOpts{
		Name:         plan.Name.ValueString(),
		Description:  plan.Description.ValueString(),
		AdminStateUp: &adminUp,
		TenantID:     plan.TenantID.ValueString(),
	}
	if !plan.Shared.IsNull() && !plan.Shared.IsUnknown() {
		shared := plan.Shared.ValueBool()
		base.Shared = &shared
	}

	var createOpts networks.CreateOptsBuilder = base
	if !plan.External.IsNull() && !plan.External.IsUnknown() {
		ext := plan.External.ValueBool()
		createOpts = external.CreateOptsExt{CreateOptsBuilder: base, External: &ext}
	}

	n, err := networks.Create(ctx, client, createOpts).Extract()
	if err != nil {
		resp.Diagnostics.AddError("networking: creating network", err.Error())
		return
	}

	if !plan.Tags.IsNull() && !plan.Tags.IsUnknown() {
		var tags []string
		resp.Diagnostics.Append(plan.Tags.ElementsAs(ctx, &tags, false)...)
		if resp.Diagnostics.HasError() {
			return
		}
		if err := replaceTags(ctx, client, "networks", n.ID, tags); err != nil {
			resp.Diagnostics.AddError("networking: setting network tags", err.Error())
			return
		}
	}

	_, readDiags := r.readInto(ctx, client, n.ID, &plan)
	resp.Diagnostics.Append(readDiags...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *networkResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state networkModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := r.config.NetworkV2Client()
	if err != nil {
		resp.Diagnostics.AddError("networking: building v2 client", err.Error())
		return
	}

	notFound, diags := r.readInto(ctx, client, state.ID.ValueString(), &state)
	if notFound {
		resp.Diagnostics.AddWarning("Network not found",
			fmt.Sprintf("Network %s no longer exists and was removed from state.", state.ID.ValueString()))
		resp.State.RemoveResource(ctx)
		return
	}
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *networkResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state networkModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := r.config.NetworkV2Client()
	if err != nil {
		resp.Diagnostics.AddError("networking: building v2 client", err.Error())
		return
	}

	name := plan.Name.ValueString()
	description := plan.Description.ValueString()
	adminUp := plan.AdminStateUp.ValueBool()
	base := networks.UpdateOpts{Name: &name, Description: &description, AdminStateUp: &adminUp}
	if !plan.Shared.IsNull() && !plan.Shared.IsUnknown() {
		shared := plan.Shared.ValueBool()
		base.Shared = &shared
	}

	var updateOpts networks.UpdateOptsBuilder = base
	if !plan.External.Equal(state.External) && !plan.External.IsNull() && !plan.External.IsUnknown() {
		ext := plan.External.ValueBool()
		updateOpts = external.UpdateOptsExt{UpdateOptsBuilder: base, External: &ext}
	}

	if _, err := networks.Update(ctx, client, plan.ID.ValueString(), updateOpts).Extract(); err != nil {
		resp.Diagnostics.AddError("networking: updating network", err.Error())
		return
	}

	if !plan.Tags.Equal(state.Tags) {
		var tags []string
		if !plan.Tags.IsNull() && !plan.Tags.IsUnknown() {
			resp.Diagnostics.Append(plan.Tags.ElementsAs(ctx, &tags, false)...)
			if resp.Diagnostics.HasError() {
				return
			}
		}
		if err := replaceTags(ctx, client, "networks", plan.ID.ValueString(), tags); err != nil {
			resp.Diagnostics.AddError("networking: updating network tags", err.Error())
			return
		}
	}

	_, readDiags := r.readInto(ctx, client, plan.ID.ValueString(), &plan)
	resp.Diagnostics.Append(readDiags...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *networkResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state networkModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := r.config.NetworkV2Client()
	if err != nil {
		resp.Diagnostics.AddError("networking: building v2 client", err.Error())
		return
	}

	if err := networks.Delete(ctx, client, state.ID.ValueString()).ExtractErr(); err != nil {
		if gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
			return
		}
		resp.Diagnostics.AddError("networking: deleting network", err.Error())
	}
}

func (r *networkResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, path.Root("id"), req, resp)
}

// readInto fetches the network (base + external ext) and populates the model.
// notFound is true when the network no longer exists (HTTP 404).
func (r *networkResource) readInto(ctx context.Context, client *gophercloud.ServiceClient, id string, m *networkModel) (notFound bool, diags diag.Diagnostics) {
	var n networkExtended
	if err := networks.Get(ctx, client, id).ExtractInto(&n); err != nil {
		if gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
			return true, diags
		}
		diags.AddError("networking: reading network", err.Error())
		return false, diags
	}

	m.ID = types.StringValue(n.ID)
	m.Name = types.StringValue(n.Name)
	m.Description = types.StringValue(n.Description)
	m.AdminStateUp = types.BoolValue(n.AdminStateUp)
	m.Shared = types.BoolValue(n.Shared)
	m.External = types.BoolValue(n.External)
	m.TenantID = types.StringValue(n.TenantID)

	tagVals := n.Tags
	if tagVals == nil {
		tagVals = []string{}
	}
	tags, d := types.SetValueFrom(ctx, types.StringType, tagVals)
	diags = append(diags, d...)
	m.Tags = tags

	if m.Region.IsNull() || m.Region.IsUnknown() {
		m.Region = types.StringValue(r.config.Region)
	}
	return false, diags
}
