// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0
//
// Ported from terraform-provider-openstack v3.4.0
// (openstack/resource_openstack_vpnaas_service_v2.go), adapted for the
// terraform-plugin-framework and PCD.

package vpnaas

import (
	"context"
	"fmt"
	"net/http"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/extensions/vpnaas/services"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/platform9/terraform-provider-pcd/internal/clients"
)

var (
	_ resource.Resource                = (*serviceResource)(nil)
	_ resource.ResourceWithConfigure   = (*serviceResource)(nil)
	_ resource.ResourceWithImportState = (*serviceResource)(nil)
)

// NewServiceResource is the factory registered with the provider.
func NewServiceResource() resource.Resource {
	return &serviceResource{}
}

type serviceResource struct {
	config *clients.Config
}

type serviceModel struct {
	ID           types.String `tfsdk:"id"`
	Region       types.String `tfsdk:"region"`
	Name         types.String `tfsdk:"name"`
	Description  types.String `tfsdk:"description"`
	AdminStateUp types.Bool   `tfsdk:"admin_state_up"`
	SubnetID     types.String `tfsdk:"subnet_id"`
	RouterID     types.String `tfsdk:"router_id"`
	TenantID     types.String `tfsdk:"tenant_id"`
	Status       types.String `tfsdk:"status"`
	ExternalV4IP types.String `tfsdk:"external_v4_ip"`
	ExternalV6IP types.String `tfsdk:"external_v6_ip"`
}

func (r *serviceResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_vpnaas_service"
}

func (r *serviceResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	useState := []planmodifier.String{stringplanmodifier.UseStateForUnknown()}
	replaceKeep := []planmodifier.String{stringplanmodifier.RequiresReplace(), stringplanmodifier.UseStateForUnknown()}
	resp.Schema = schema.Schema{
		MarkdownDescription: "Manages a Neutron VPN service (the VPN endpoint on a router) in PCD.",
		Attributes: map[string]schema.Attribute{
			"id":             schema.StringAttribute{Computed: true, MarkdownDescription: "The VPN service ID.", PlanModifiers: useState},
			"region":         schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "The region. Defaults to the provider's region.", PlanModifiers: useState},
			"name":           schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "The name of the VPN service.", PlanModifiers: useState},
			"description":    schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "A description of the VPN service.", PlanModifiers: useState},
			"admin_state_up": schema.BoolAttribute{Optional: true, Computed: true, Default: booldefault.StaticBool(true), MarkdownDescription: "The administrative state of the service."},
			"subnet_id":      schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "The subnet on which the local endpoints reside. Changing this forces a new resource.", PlanModifiers: replaceKeep},
			"router_id":      schema.StringAttribute{Required: true, MarkdownDescription: "The router the VPN service is attached to. Changing this forces a new resource.", PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()}},
			"tenant_id":      schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "The owning project. Changing this forces a new resource.", PlanModifiers: replaceKeep},
			"status":         schema.StringAttribute{Computed: true, MarkdownDescription: "The operational status of the service.", PlanModifiers: useState},
			"external_v4_ip": schema.StringAttribute{Computed: true, MarkdownDescription: "The read-only external (public) IPv4 address.", PlanModifiers: useState},
			"external_v6_ip": schema.StringAttribute{Computed: true, MarkdownDescription: "The read-only external (public) IPv6 address.", PlanModifiers: useState},
		},
	}
}

func (r *serviceResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.config = configureClient(req.ProviderData, &resp.Diagnostics)
}

func (r *serviceResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan serviceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := r.config.NetworkV2Client()
	if err != nil {
		resp.Diagnostics.AddError("vpnaas: building network client", err.Error())
		return
	}

	adminUp := plan.AdminStateUp.ValueBool()
	svc, err := services.Create(ctx, client, services.CreateOpts{
		Name:         plan.Name.ValueString(),
		Description:  plan.Description.ValueString(),
		AdminStateUp: &adminUp,
		SubnetID:     plan.SubnetID.ValueString(),
		RouterID:     plan.RouterID.ValueString(),
		TenantID:     plan.TenantID.ValueString(),
	}).Extract()
	if err != nil {
		resp.Diagnostics.AddError("vpnaas: creating VPN service", err.Error())
		return
	}

	resp.Diagnostics.Append(r.readInto(ctx, client, svc.ID, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *serviceResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state serviceModel
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

func (r *serviceResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state serviceModel
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

	opts := services.UpdateOpts{}
	if !plan.Name.Equal(state.Name) {
		v := plan.Name.ValueString()
		opts.Name = &v
	}
	if !plan.Description.Equal(state.Description) {
		v := plan.Description.ValueString()
		opts.Description = &v
	}
	if !plan.AdminStateUp.Equal(state.AdminStateUp) {
		v := plan.AdminStateUp.ValueBool()
		opts.AdminStateUp = &v
	}

	id := plan.ID.ValueString()
	if _, err := services.Update(ctx, client, id, opts).Extract(); err != nil {
		resp.Diagnostics.AddError("vpnaas: updating VPN service", err.Error())
		return
	}

	resp.Diagnostics.Append(r.readInto(ctx, client, id, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *serviceResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state serviceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := r.config.NetworkV2Client()
	if err != nil {
		resp.Diagnostics.AddError("vpnaas: building network client", err.Error())
		return
	}

	id := state.ID.ValueString()
	if err := services.Delete(ctx, client, id).ExtractErr(); err != nil {
		if gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
			return
		}
		resp.Diagnostics.AddError("vpnaas: deleting VPN service", err.Error())
		return
	}

	if err := waitForDeletion(ctx, defaultVPNTimeout, func(ctx context.Context) error {
		_, e := services.Get(ctx, client, id).Extract()
		return e
	}); err != nil {
		resp.Diagnostics.AddError("vpnaas: waiting for VPN service delete", err.Error())
	}
}

func (r *serviceResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, path.Root("id"), req, resp)
}

func (r *serviceResource) readInto(ctx context.Context, client *gophercloud.ServiceClient, id string, m *serviceModel) diag.Diagnostics {
	notFound, diags := r.get(ctx, client, id, m)
	if notFound {
		diags.AddError("vpnaas: VPN service not found after write",
			fmt.Sprintf("VPN service %s was not found immediately after a create/update.", id))
	}
	return diags
}

func (r *serviceResource) get(ctx context.Context, client *gophercloud.ServiceClient, id string, m *serviceModel) (notFound bool, diags diag.Diagnostics) {
	svc, err := services.Get(ctx, client, id).Extract()
	if err != nil {
		if gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
			return true, diags
		}
		diags.AddError("vpnaas: reading VPN service", err.Error())
		return false, diags
	}

	m.ID = types.StringValue(svc.ID)
	m.Name = types.StringValue(svc.Name)
	m.Description = types.StringValue(svc.Description)
	m.AdminStateUp = types.BoolValue(svc.AdminStateUp)
	m.SubnetID = types.StringValue(svc.SubnetID)
	m.RouterID = types.StringValue(svc.RouterID)
	m.TenantID = types.StringValue(svc.TenantID)
	m.Status = types.StringValue(svc.Status)
	m.ExternalV4IP = types.StringValue(svc.ExternalV4IP)
	m.ExternalV6IP = types.StringValue(svc.ExternalV6IP)
	if m.Region.IsNull() || m.Region.IsUnknown() {
		m.Region = types.StringValue(r.config.Region)
	}
	return false, diags
}
