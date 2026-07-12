// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0
//
// Ported from terraform-provider-openstack v3.4.0
// (openstack/resource_openstack_vpnaas_site_connection_v2.go), adapted for the
// terraform-plugin-framework and PCD.

package vpnaas

import (
	"context"
	"fmt"
	"net/http"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/extensions/vpnaas/siteconnections"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/int64planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/listplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/objectplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/platform9/terraform-provider-pcd/internal/clients"
)

var (
	_ resource.Resource                = (*siteConnectionResource)(nil)
	_ resource.ResourceWithConfigure   = (*siteConnectionResource)(nil)
	_ resource.ResourceWithImportState = (*siteConnectionResource)(nil)
)

// NewSiteConnectionResource is the factory registered with the provider.
func NewSiteConnectionResource() resource.Resource {
	return &siteConnectionResource{}
}

type siteConnectionResource struct {
	config *clients.Config
}

type siteConnectionModel struct {
	ID             types.String `tfsdk:"id"`
	Region         types.String `tfsdk:"region"`
	Name           types.String `tfsdk:"name"`
	Description    types.String `tfsdk:"description"`
	IKEPolicyID    types.String `tfsdk:"ike_policy_id"`
	VPNServiceID   types.String `tfsdk:"vpn_service_id"`
	IPSecPolicyID  types.String `tfsdk:"ipsec_policy_id"`
	PeerID         types.String `tfsdk:"peer_id"`
	PeerAddress    types.String `tfsdk:"peer_address"`
	PSK            types.String `tfsdk:"psk"`
	PeerEPGroupID  types.String `tfsdk:"peer_ep_group_id"`
	LocalEPGroupID types.String `tfsdk:"local_ep_group_id"`
	LocalID        types.String `tfsdk:"local_id"`
	AdminStateUp   types.Bool   `tfsdk:"admin_state_up"`
	Initiator      types.String `tfsdk:"initiator"`
	MTU            types.Int64  `tfsdk:"mtu"`
	PeerCIDRs      types.List   `tfsdk:"peer_cidrs"`
	DPD            types.Object `tfsdk:"dpd"`
	TenantID       types.String `tfsdk:"tenant_id"`
}

func (r *siteConnectionResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_vpnaas_site_connection"
}

func (r *siteConnectionResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	useState := []planmodifier.String{stringplanmodifier.UseStateForUnknown()}
	replace := []planmodifier.String{stringplanmodifier.RequiresReplace()}
	replaceKeep := []planmodifier.String{stringplanmodifier.RequiresReplace(), stringplanmodifier.UseStateForUnknown()}
	resp.Schema = schema.Schema{
		MarkdownDescription: "Manages a Neutron IPsec site-to-site VPN connection in PCD.",
		Attributes: map[string]schema.Attribute{
			"id":                schema.StringAttribute{Computed: true, MarkdownDescription: "The connection ID.", PlanModifiers: useState},
			"region":            schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "The region. Defaults to the provider's region.", PlanModifiers: useState},
			"name":              schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "The name of the connection.", PlanModifiers: useState},
			"description":       schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "A description of the connection.", PlanModifiers: useState},
			"ike_policy_id":     schema.StringAttribute{Required: true, MarkdownDescription: "The IKE policy ID. Changing this forces a new resource.", PlanModifiers: replace},
			"vpn_service_id":    schema.StringAttribute{Required: true, MarkdownDescription: "The VPN service ID. Changing this forces a new resource.", PlanModifiers: replace},
			"ipsec_policy_id":   schema.StringAttribute{Required: true, MarkdownDescription: "The IPsec policy ID. Changing this forces a new resource.", PlanModifiers: replace},
			"peer_id":           schema.StringAttribute{Required: true, MarkdownDescription: "The peer router identity (IP, FQDN, e-mail, or key ID)."},
			"peer_address":      schema.StringAttribute{Required: true, MarkdownDescription: "The peer gateway public address or FQDN."},
			"psk":               schema.StringAttribute{Required: true, Sensitive: true, MarkdownDescription: "The pre-shared key."},
			"peer_ep_group_id":  schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "The peer endpoint group ID (peer CIDRs).", PlanModifiers: useState},
			"local_ep_group_id": schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "The local endpoint group ID (local subnets).", PlanModifiers: useState},
			"local_id":          schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "An ID used instead of the external IP for the local router.", PlanModifiers: useState},
			"admin_state_up":    schema.BoolAttribute{Optional: true, Computed: true, Default: booldefault.StaticBool(true), MarkdownDescription: "The administrative state of the connection."},
			"initiator":         schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "Whether the connection initiates or only responds: bi-directional (default) or response-only.", PlanModifiers: useState},
			"mtu":               schema.Int64Attribute{Optional: true, Computed: true, MarkdownDescription: "The MTU for the connection (min 68 for IPv4, 1280 for IPv6).", PlanModifiers: []planmodifier.Int64{int64planmodifier.UseStateForUnknown()}},
			"peer_cidrs": schema.ListAttribute{
				Optional:            true,
				Computed:            true,
				ElementType:         types.StringType,
				MarkdownDescription: "The peer private CIDRs (used in backward-compatible mode instead of a peer endpoint group).",
				PlanModifiers:       []planmodifier.List{listplanmodifier.UseStateForUnknown()},
			},
			"dpd": schema.SingleNestedAttribute{
				Optional:            true,
				Computed:            true,
				MarkdownDescription: "Dead peer detection (DPD) controls.",
				PlanModifiers:       []planmodifier.Object{objectplanmodifier.UseStateForUnknown()},
				Attributes: map[string]schema.Attribute{
					"action":   schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "The DPD action: hold (default), clear, restart, disabled, or restart-by-peer.", PlanModifiers: useState},
					"timeout":  schema.Int64Attribute{Optional: true, Computed: true, MarkdownDescription: "The DPD timeout in seconds (default 120).", PlanModifiers: []planmodifier.Int64{int64planmodifier.UseStateForUnknown()}},
					"interval": schema.Int64Attribute{Optional: true, Computed: true, MarkdownDescription: "The DPD interval in seconds (default 30).", PlanModifiers: []planmodifier.Int64{int64planmodifier.UseStateForUnknown()}},
				},
			},
			"tenant_id": schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "The owning project. Changing this forces a new resource.", PlanModifiers: replaceKeep},
		},
	}
}

func (r *siteConnectionResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.config = configureClient(req.ProviderData, &resp.Diagnostics)
}

func (r *siteConnectionResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan siteConnectionModel
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
	opts := siteconnections.CreateOpts{
		IKEPolicyID:    plan.IKEPolicyID.ValueString(),
		VPNServiceID:   plan.VPNServiceID.ValueString(),
		IPSecPolicyID:  plan.IPSecPolicyID.ValueString(),
		PeerID:         plan.PeerID.ValueString(),
		PeerAddress:    plan.PeerAddress.ValueString(),
		PSK:            plan.PSK.ValueString(),
		Name:           plan.Name.ValueString(),
		Description:    plan.Description.ValueString(),
		LocalID:        plan.LocalID.ValueString(),
		TenantID:       plan.TenantID.ValueString(),
		PeerEPGroupID:  plan.PeerEPGroupID.ValueString(),
		LocalEPGroupID: plan.LocalEPGroupID.ValueString(),
		Initiator:      siteconnections.Initiator(plan.Initiator.ValueString()),
		AdminStateUp:   &adminUp,
		PeerCIDRs:      listToStrings(ctx, plan.PeerCIDRs, &resp.Diagnostics),
		MTU:            int(plan.MTU.ValueInt64()),
	}
	if action, timeout, interval, ok := dpdFromObject(ctx, plan.DPD, &resp.Diagnostics); ok {
		opts.DPD = &siteconnections.DPDCreateOpts{
			Action:   siteconnections.Action(action),
			Timeout:  timeout,
			Interval: interval,
		}
	}
	if resp.Diagnostics.HasError() {
		return
	}

	conn, err := siteconnections.Create(ctx, client, opts).Extract()
	if err != nil {
		resp.Diagnostics.AddError("vpnaas: creating site connection", err.Error())
		return
	}

	resp.Diagnostics.Append(r.readInto(ctx, client, conn.ID, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *siteConnectionResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state siteConnectionModel
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

func (r *siteConnectionResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state siteConnectionModel
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

	opts := siteconnections.UpdateOpts{}
	if !plan.Name.Equal(state.Name) {
		v := plan.Name.ValueString()
		opts.Name = &v
	}
	if !plan.Description.Equal(state.Description) {
		v := plan.Description.ValueString()
		opts.Description = &v
	}
	if !plan.PeerID.Equal(state.PeerID) {
		opts.PeerID = plan.PeerID.ValueString()
	}
	if !plan.PeerAddress.Equal(state.PeerAddress) {
		opts.PeerAddress = plan.PeerAddress.ValueString()
	}
	if !plan.PSK.Equal(state.PSK) {
		opts.PSK = plan.PSK.ValueString()
	}
	if !plan.LocalID.Equal(state.LocalID) {
		opts.LocalID = plan.LocalID.ValueString()
	}
	if !plan.PeerEPGroupID.Equal(state.PeerEPGroupID) {
		opts.PeerEPGroupID = plan.PeerEPGroupID.ValueString()
	}
	if !plan.LocalEPGroupID.Equal(state.LocalEPGroupID) {
		opts.LocalEPGroupID = plan.LocalEPGroupID.ValueString()
	}
	if !plan.Initiator.Equal(state.Initiator) {
		opts.Initiator = siteconnections.Initiator(plan.Initiator.ValueString())
	}
	if !plan.MTU.Equal(state.MTU) {
		opts.MTU = int(plan.MTU.ValueInt64())
	}
	if !plan.AdminStateUp.Equal(state.AdminStateUp) {
		v := plan.AdminStateUp.ValueBool()
		opts.AdminStateUp = &v
	}
	if !plan.PeerCIDRs.Equal(state.PeerCIDRs) {
		opts.PeerCIDRs = listToStrings(ctx, plan.PeerCIDRs, &resp.Diagnostics)
	}
	if !plan.DPD.Equal(state.DPD) {
		if action, timeout, interval, ok := dpdFromObject(ctx, plan.DPD, &resp.Diagnostics); ok {
			opts.DPD = &siteconnections.DPDUpdateOpts{
				Action:   siteconnections.Action(action),
				Timeout:  timeout,
				Interval: interval,
			}
		}
	}
	if resp.Diagnostics.HasError() {
		return
	}

	id := plan.ID.ValueString()
	if _, err := siteconnections.Update(ctx, client, id, opts).Extract(); err != nil {
		resp.Diagnostics.AddError("vpnaas: updating site connection", err.Error())
		return
	}

	resp.Diagnostics.Append(r.readInto(ctx, client, id, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *siteConnectionResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state siteConnectionModel
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
	if err := siteconnections.Delete(ctx, client, id).ExtractErr(); err != nil {
		if gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
			return
		}
		resp.Diagnostics.AddError("vpnaas: deleting site connection", err.Error())
		return
	}

	if err := waitForDeletion(ctx, defaultVPNTimeout, func(ctx context.Context) error {
		_, e := siteconnections.Get(ctx, client, id).Extract()
		return e
	}); err != nil {
		resp.Diagnostics.AddError("vpnaas: waiting for site connection delete", err.Error())
	}
}

func (r *siteConnectionResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, path.Root("id"), req, resp)
}

func (r *siteConnectionResource) readInto(ctx context.Context, client *gophercloud.ServiceClient, id string, m *siteConnectionModel) diag.Diagnostics {
	notFound, diags := r.get(ctx, client, id, m)
	if notFound {
		diags.AddError("vpnaas: site connection not found after write",
			fmt.Sprintf("Site connection %s was not found immediately after a create/update.", id))
	}
	return diags
}

func (r *siteConnectionResource) get(ctx context.Context, client *gophercloud.ServiceClient, id string, m *siteConnectionModel) (notFound bool, diags diag.Diagnostics) {
	conn, err := siteconnections.Get(ctx, client, id).Extract()
	if err != nil {
		if gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
			return true, diags
		}
		diags.AddError("vpnaas: reading site connection", err.Error())
		return false, diags
	}

	m.ID = types.StringValue(conn.ID)
	m.Name = types.StringValue(conn.Name)
	m.Description = types.StringValue(conn.Description)
	m.IKEPolicyID = types.StringValue(conn.IKEPolicyID)
	m.VPNServiceID = types.StringValue(conn.VPNServiceID)
	m.IPSecPolicyID = types.StringValue(conn.IPSecPolicyID)
	m.PeerID = types.StringValue(conn.PeerID)
	m.PeerAddress = types.StringValue(conn.PeerAddress)
	m.PSK = types.StringValue(conn.PSK)
	m.PeerEPGroupID = types.StringValue(conn.PeerEPGroupID)
	m.LocalEPGroupID = types.StringValue(conn.LocalEPGroupID)
	m.LocalID = types.StringValue(conn.LocalID)
	m.AdminStateUp = types.BoolValue(conn.AdminStateUp)
	m.Initiator = types.StringValue(conn.Initiator)
	m.MTU = types.Int64Value(int64(conn.MTU))
	m.TenantID = types.StringValue(conn.TenantID)

	cidrs := conn.PeerCIDRs
	if cidrs == nil {
		cidrs = []string{}
	}
	cidrList, d := types.ListValueFrom(ctx, types.StringType, cidrs)
	diags = append(diags, d...)
	m.PeerCIDRs = cidrList

	m.DPD = flattenDPD(conn.DPD.Action, conn.DPD.Timeout, conn.DPD.Interval)

	if m.Region.IsNull() || m.Region.IsUnknown() {
		m.Region = types.StringValue(r.config.Region)
	}
	return false, diags
}
