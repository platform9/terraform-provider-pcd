// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0
//
// Ported from terraform-provider-openstack v3.4.0
// (openstack/resource_openstack_networking_secgroup_rule_v2.go), adapted for the
// terraform-plugin-framework and PCD.

package networking

import (
	"context"
	"fmt"
	"net/http"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/extensions/security/rules"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/platform9/terraform-provider-pcd/internal/clients"
)

var (
	_ resource.Resource                = (*secgroupRuleResource)(nil)
	_ resource.ResourceWithConfigure   = (*secgroupRuleResource)(nil)
	_ resource.ResourceWithImportState = (*secgroupRuleResource)(nil)
)

// NewSecgroupRuleResource is the factory registered with the provider.
func NewSecgroupRuleResource() resource.Resource {
	return &secgroupRuleResource{}
}

type secgroupRuleResource struct {
	config *clients.Config
}

type secgroupRuleModel struct {
	ID              types.String `tfsdk:"id"`
	Direction       types.String `tfsdk:"direction"`
	EtherType       types.String `tfsdk:"ethertype"`
	SecurityGroupID types.String `tfsdk:"security_group_id"`
	Protocol        types.String `tfsdk:"protocol"`
	PortRangeMin    types.Int64  `tfsdk:"port_range_min"`
	PortRangeMax    types.Int64  `tfsdk:"port_range_max"`
	RemoteGroupID   types.String `tfsdk:"remote_group_id"`
	RemoteIPPrefix  types.String `tfsdk:"remote_ip_prefix"`
	Description     types.String `tfsdk:"description"`
	TenantID        types.String `tfsdk:"tenant_id"`
	Region          types.String `tfsdk:"region"`
}

func (r *secgroupRuleResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_networking_secgroup_rule"
}

func (r *secgroupRuleResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	fnStr := []planmodifier.String{stringplanmodifier.RequiresReplace()}
	fnStrC := []planmodifier.String{stringplanmodifier.RequiresReplace(), stringplanmodifier.UseStateForUnknown()}
	fnInt := []planmodifier.Int64{}
	resp.Schema = schema.Schema{
		MarkdownDescription: "Manages a rule within a Neutron security group. Rules are immutable: any change forces a new resource.",
		Attributes: map[string]schema.Attribute{
			"id":                schema.StringAttribute{Computed: true, MarkdownDescription: "The rule ID.", PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()}},
			"direction":         schema.StringAttribute{Required: true, MarkdownDescription: "Direction: ingress or egress.", PlanModifiers: fnStr},
			"ethertype":         schema.StringAttribute{Required: true, MarkdownDescription: "EtherType: IPv4 or IPv6.", PlanModifiers: fnStr},
			"security_group_id": schema.StringAttribute{Required: true, MarkdownDescription: "The security group this rule belongs to.", PlanModifiers: fnStr},
			"protocol":          schema.StringAttribute{Optional: true, MarkdownDescription: "Protocol (tcp, udp, icmp, ...).", PlanModifiers: fnStr},
			"port_range_min":    schema.Int64Attribute{Optional: true, MarkdownDescription: "Lower bound of the port range.", PlanModifiers: fnInt},
			"port_range_max":    schema.Int64Attribute{Optional: true, MarkdownDescription: "Upper bound of the port range.", PlanModifiers: fnInt},
			"remote_group_id":   schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "Remote security group ID (mutually exclusive with remote_ip_prefix).", PlanModifiers: fnStrC},
			"remote_ip_prefix":  schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "Remote CIDR (mutually exclusive with remote_group_id).", PlanModifiers: fnStrC},
			"description":       schema.StringAttribute{Optional: true, MarkdownDescription: "A description of the rule.", PlanModifiers: fnStr},
			"tenant_id":         schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "The owning project.", PlanModifiers: fnStrC},
			"region":            schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "The region. Defaults to the provider's region.", PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()}},
		},
	}
}

func (r *secgroupRuleResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.config = configureClient(req.ProviderData, &resp.Diagnostics)
}

func (r *secgroupRuleResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan secgroupRuleModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := r.config.NetworkV2Client()
	if err != nil {
		resp.Diagnostics.AddError("networking: building v2 client", err.Error())
		return
	}

	createOpts := rules.CreateOpts{
		Direction:      rules.RuleDirection(plan.Direction.ValueString()),
		EtherType:      rules.RuleEtherType(plan.EtherType.ValueString()),
		SecGroupID:     plan.SecurityGroupID.ValueString(),
		Protocol:       rules.RuleProtocol(plan.Protocol.ValueString()),
		PortRangeMin:   int(plan.PortRangeMin.ValueInt64()),
		PortRangeMax:   int(plan.PortRangeMax.ValueInt64()),
		RemoteGroupID:  plan.RemoteGroupID.ValueString(),
		RemoteIPPrefix: plan.RemoteIPPrefix.ValueString(),
		Description:    plan.Description.ValueString(),
	}

	rule, err := rules.Create(ctx, client, createOpts).Extract()
	if err != nil {
		resp.Diagnostics.AddError("networking: creating security group rule", err.Error())
		return
	}

	r.flatten(rule, &plan)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *secgroupRuleResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state secgroupRuleModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := r.config.NetworkV2Client()
	if err != nil {
		resp.Diagnostics.AddError("networking: building v2 client", err.Error())
		return
	}

	rule, err := rules.Get(ctx, client, state.ID.ValueString()).Extract()
	if err != nil {
		if gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
			resp.Diagnostics.AddWarning("Security group rule not found",
				fmt.Sprintf("Rule %s no longer exists and was removed from state.", state.ID.ValueString()))
			resp.State.RemoveResource(ctx)
			return
		}
		resp.Diagnostics.AddError("networking: reading security group rule", err.Error())
		return
	}

	r.flatten(rule, &state)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

// Update is required by the interface but never invoked (every attribute forces replacement).
func (r *secgroupRuleResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan secgroupRuleModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *secgroupRuleResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state secgroupRuleModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := r.config.NetworkV2Client()
	if err != nil {
		resp.Diagnostics.AddError("networking: building v2 client", err.Error())
		return
	}

	if err := rules.Delete(ctx, client, state.ID.ValueString()).ExtractErr(); err != nil {
		if gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
			return
		}
		resp.Diagnostics.AddError("networking: deleting security group rule", err.Error())
	}
}

func (r *secgroupRuleResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, path.Root("id"), req, resp)
}

func (r *secgroupRuleResource) flatten(rule *rules.SecGroupRule, m *secgroupRuleModel) {
	m.ID = types.StringValue(rule.ID)
	m.Direction = types.StringValue(rule.Direction)
	m.EtherType = types.StringValue(rule.EtherType)
	m.SecurityGroupID = types.StringValue(rule.SecGroupID)
	m.Protocol = optionalRuleString(rule.Protocol)
	m.RemoteGroupID = types.StringValue(rule.RemoteGroupID)
	m.RemoteIPPrefix = types.StringValue(rule.RemoteIPPrefix)
	m.TenantID = types.StringValue(rule.TenantID)
	if rule.PortRangeMin != 0 {
		m.PortRangeMin = types.Int64Value(int64(rule.PortRangeMin))
	}
	if rule.PortRangeMax != 0 {
		m.PortRangeMax = types.Int64Value(int64(rule.PortRangeMax))
	}
	if rule.Description != "" {
		m.Description = types.StringValue(rule.Description)
	}
	if m.Region.IsNull() || m.Region.IsUnknown() {
		m.Region = types.StringValue(r.config.Region)
	}
}

// optionalRuleString keeps a null value null (protocol is optional and Neutron
// may return an empty string when unset).
func optionalRuleString(v string) types.String {
	if v == "" {
		return types.StringNull()
	}
	return types.StringValue(v)
}
