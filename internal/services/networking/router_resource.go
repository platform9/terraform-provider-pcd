// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0
//
// Ported from terraform-provider-openstack v3.4.0
// (openstack/resource_openstack_networking_router_v2.go), adapted for the
// terraform-plugin-framework and PCD.

package networking

import (
	"context"
	"fmt"
	"net/http"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/extensions/layer3/routers"
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
	_ resource.Resource                = (*routerResource)(nil)
	_ resource.ResourceWithConfigure   = (*routerResource)(nil)
	_ resource.ResourceWithImportState = (*routerResource)(nil)
)

// NewRouterResource is the factory registered with the provider.
func NewRouterResource() resource.Resource {
	return &routerResource{}
}

type routerResource struct {
	config *clients.Config
}

type routerModel struct {
	ID                types.String `tfsdk:"id"`
	Name              types.String `tfsdk:"name"`
	Description       types.String `tfsdk:"description"`
	AdminStateUp      types.Bool   `tfsdk:"admin_state_up"`
	ExternalNetworkID types.String `tfsdk:"external_network_id"`
	EnableSNAT        types.Bool   `tfsdk:"enable_snat"`
	TenantID          types.String `tfsdk:"tenant_id"`
	Tags              types.Set    `tfsdk:"tags"`
	Region            types.String `tfsdk:"region"`
}

func (r *routerResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_networking_router"
}

func (r *routerResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	stable := []planmodifier.String{stringplanmodifier.UseStateForUnknown()}
	resp.Schema = schema.Schema{
		MarkdownDescription: "Manages a Neutron router in PCD.",
		Attributes: map[string]schema.Attribute{
			"id":                  schema.StringAttribute{Computed: true, MarkdownDescription: "The router ID.", PlanModifiers: stable},
			"name":                schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "The name of the router.", PlanModifiers: stable},
			"description":         schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "A description of the router.", PlanModifiers: stable},
			"admin_state_up":      schema.BoolAttribute{Optional: true, Computed: true, Default: booldefault.StaticBool(true), MarkdownDescription: "The administrative state of the router."},
			"external_network_id": schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "The ID of the external network for the router's gateway.", PlanModifiers: stable},
			"enable_snat":         schema.BoolAttribute{Optional: true, Computed: true, MarkdownDescription: "Whether SNAT is enabled on the gateway. Requires external_network_id.", PlanModifiers: []planmodifier.Bool{}},
			"tenant_id":           schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "The owning project. Changing this forces a new resource.", PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace(), stringplanmodifier.UseStateForUnknown()}},
			"tags":                schema.SetAttribute{Optional: true, Computed: true, ElementType: types.StringType, MarkdownDescription: "Tags applied to the router.", PlanModifiers: []planmodifier.Set{setplanmodifier.UseStateForUnknown()}},
			"region":              schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "The region. Defaults to the provider's region.", PlanModifiers: stable},
		},
	}
}

func (r *routerResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.config = configureClient(req.ProviderData, &resp.Diagnostics)
}

func (r *routerResource) gatewayInfo(m *routerModel) *routers.GatewayInfo {
	if m.ExternalNetworkID.ValueString() == "" {
		return nil
	}
	gw := &routers.GatewayInfo{NetworkID: m.ExternalNetworkID.ValueString()}
	if !m.EnableSNAT.IsNull() && !m.EnableSNAT.IsUnknown() {
		snat := m.EnableSNAT.ValueBool()
		gw.EnableSNAT = &snat
	}
	return gw
}

func (r *routerResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan routerModel
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
	createOpts := routers.CreateOpts{
		Name:         plan.Name.ValueString(),
		Description:  plan.Description.ValueString(),
		AdminStateUp: &adminUp,
		TenantID:     plan.TenantID.ValueString(),
		GatewayInfo:  r.gatewayInfo(&plan),
	}

	router, err := routers.Create(ctx, client, createOpts).Extract()
	if err != nil {
		resp.Diagnostics.AddError("networking: creating router", err.Error())
		return
	}

	if !plan.Tags.IsNull() && !plan.Tags.IsUnknown() {
		var tags []string
		resp.Diagnostics.Append(plan.Tags.ElementsAs(ctx, &tags, false)...)
		if resp.Diagnostics.HasError() {
			return
		}
		if err := replaceTags(ctx, client, "routers", router.ID, tags); err != nil {
			resp.Diagnostics.AddError("networking: setting router tags", err.Error())
			return
		}
	}

	_, readDiags := r.readInto(ctx, client, router.ID, &plan)
	resp.Diagnostics.Append(readDiags...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *routerResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state routerModel
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
		resp.Diagnostics.AddWarning("Router not found",
			fmt.Sprintf("Router %s no longer exists and was removed from state.", state.ID.ValueString()))
		resp.State.RemoveResource(ctx)
		return
	}
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *routerResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state routerModel
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
	updateOpts := routers.UpdateOpts{Name: name, Description: &description, AdminStateUp: &adminUp}
	if !plan.ExternalNetworkID.Equal(state.ExternalNetworkID) || !plan.EnableSNAT.Equal(state.EnableSNAT) {
		updateOpts.GatewayInfo = r.gatewayInfo(&plan)
	}

	if _, err := routers.Update(ctx, client, plan.ID.ValueString(), updateOpts).Extract(); err != nil {
		resp.Diagnostics.AddError("networking: updating router", err.Error())
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
		if err := replaceTags(ctx, client, "routers", plan.ID.ValueString(), tags); err != nil {
			resp.Diagnostics.AddError("networking: updating router tags", err.Error())
			return
		}
	}

	_, readDiags := r.readInto(ctx, client, plan.ID.ValueString(), &plan)
	resp.Diagnostics.Append(readDiags...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *routerResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state routerModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := r.config.NetworkV2Client()
	if err != nil {
		resp.Diagnostics.AddError("networking: building v2 client", err.Error())
		return
	}

	if err := routers.Delete(ctx, client, state.ID.ValueString()).ExtractErr(); err != nil {
		if gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
			return
		}
		resp.Diagnostics.AddError("networking: deleting router", err.Error())
	}
}

func (r *routerResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, path.Root("id"), req, resp)
}

func (r *routerResource) readInto(ctx context.Context, client *gophercloud.ServiceClient, id string, m *routerModel) (notFound bool, diags diag.Diagnostics) {
	router, err := routers.Get(ctx, client, id).Extract()
	if err != nil {
		if gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
			return true, diags
		}
		diags.AddError("networking: reading router", err.Error())
		return false, diags
	}

	m.ID = types.StringValue(router.ID)
	m.Name = types.StringValue(router.Name)
	m.Description = types.StringValue(router.Description)
	m.AdminStateUp = types.BoolValue(router.AdminStateUp)
	m.TenantID = types.StringValue(router.TenantID)
	m.ExternalNetworkID = types.StringValue(router.GatewayInfo.NetworkID)
	if router.GatewayInfo.EnableSNAT != nil {
		m.EnableSNAT = types.BoolValue(*router.GatewayInfo.EnableSNAT)
	} else {
		m.EnableSNAT = types.BoolValue(false)
	}

	tagVals := router.Tags
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
