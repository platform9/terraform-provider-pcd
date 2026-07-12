// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0
//
// Ported from terraform-provider-openstack v3.4.0
// (openstack/resource_openstack_networking_quota_v2.go), adapted for the
// terraform-plugin-framework and PCD.

package networking

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/extensions/quotas"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/int64planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/platform9/terraform-provider-pcd/internal/clients"
)

var (
	_ resource.Resource                = (*quotaResource)(nil)
	_ resource.ResourceWithConfigure   = (*quotaResource)(nil)
	_ resource.ResourceWithImportState = (*quotaResource)(nil)
)

// NewQuotaResource is the factory registered with the provider.
func NewQuotaResource() resource.Resource {
	return &quotaResource{}
}

type quotaResource struct {
	config *clients.Config
}

type quotaModel struct {
	ID                types.String `tfsdk:"id"`
	ProjectID         types.String `tfsdk:"project_id"`
	Region            types.String `tfsdk:"region"`
	FloatingIP        types.Int64  `tfsdk:"floatingip"`
	Network           types.Int64  `tfsdk:"network"`
	Port              types.Int64  `tfsdk:"port"`
	RBACPolicy        types.Int64  `tfsdk:"rbac_policy"`
	Router            types.Int64  `tfsdk:"router"`
	SecurityGroup     types.Int64  `tfsdk:"security_group"`
	SecurityGroupRule types.Int64  `tfsdk:"security_group_rule"`
	Subnet            types.Int64  `tfsdk:"subnet"`
	SubnetPool        types.Int64  `tfsdk:"subnetpool"`
}

func (r *quotaResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_networking_quota"
}

// quotaIntAttr builds the schema for a single Optional+Computed quota field. The
// server echoes every field back, so each is Computed; UseStateForUnknown keeps
// fields the user does not manage stable instead of churning on every plan.
func quotaIntAttr(desc string) schema.Int64Attribute {
	return schema.Int64Attribute{
		Optional:            true,
		Computed:            true,
		MarkdownDescription: desc,
		PlanModifiers:       []planmodifier.Int64{int64planmodifier.UseStateForUnknown()},
	}
}

func (r *quotaResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Manages the Neutron (networking) quotas for a project. Use `-1` for unlimited. Only the " +
			"quota fields you set are managed; fields you omit keep their server value. Destroying this resource stops " +
			"managing the quotas but does not reset them to their defaults.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{Computed: true, MarkdownDescription: "The composite `<project_id>/<region>` ID.", PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()}},
			"project_id": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "The project (tenant) the quotas apply to. Changing this forces a new resource.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"region": schema.StringAttribute{
				Optional:            true,
				Computed:            true,
				MarkdownDescription: "The region. Defaults to the provider's region. Changing this forces a new resource.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.RequiresReplace(), stringplanmodifier.UseStateForUnknown()},
			},
			"floatingip":          quotaIntAttr("Quota for the number of floating IPs."),
			"network":             quotaIntAttr("Quota for the number of networks."),
			"port":                quotaIntAttr("Quota for the number of ports."),
			"rbac_policy":         quotaIntAttr("Quota for the number of RBAC policies."),
			"router":              quotaIntAttr("Quota for the number of routers."),
			"security_group":      quotaIntAttr("Quota for the number of security groups."),
			"security_group_rule": quotaIntAttr("Quota for the number of security group rules."),
			"subnet":              quotaIntAttr("Quota for the number of subnets."),
			"subnetpool":          quotaIntAttr("Quota for the number of subnet pools."),
		},
	}
}

func (r *quotaResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.config = configureClient(req.ProviderData, &resp.Diagnostics)
}

// quotaPtrIfSet returns a *int for a known, non-null value, or nil so the field
// is omitted from the request (leaving the server value unchanged).
func quotaPtrIfSet(v types.Int64) *int {
	if v.IsNull() || v.IsUnknown() {
		return nil
	}
	i := int(v.ValueInt64())
	return &i
}

// quotaPtrIfChanged returns a *int only when the planned value differs from
// state (and is known), so an in-place update sends only changed fields.
func quotaPtrIfChanged(plan, state types.Int64) *int {
	if plan.IsUnknown() || plan.Equal(state) {
		return nil
	}
	i := int(plan.ValueInt64())
	return &i
}

func (r *quotaResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan quotaModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := r.config.NetworkV2Client()
	if err != nil {
		resp.Diagnostics.AddError("networking: building v2 client", err.Error())
		return
	}

	region := plan.Region.ValueString()
	if plan.Region.IsNull() || plan.Region.IsUnknown() {
		region = r.config.Region
	}
	projectID := plan.ProjectID.ValueString()

	opts := quotas.UpdateOpts{
		FloatingIP:        quotaPtrIfSet(plan.FloatingIP),
		Network:           quotaPtrIfSet(plan.Network),
		Port:              quotaPtrIfSet(plan.Port),
		RBACPolicy:        quotaPtrIfSet(plan.RBACPolicy),
		Router:            quotaPtrIfSet(plan.Router),
		SecurityGroup:     quotaPtrIfSet(plan.SecurityGroup),
		SecurityGroupRule: quotaPtrIfSet(plan.SecurityGroupRule),
		Subnet:            quotaPtrIfSet(plan.Subnet),
		SubnetPool:        quotaPtrIfSet(plan.SubnetPool),
	}

	if _, err := quotas.Update(ctx, client, projectID, opts).Extract(); err != nil {
		resp.Diagnostics.AddError("networking: setting quotas", err.Error())
		return
	}

	resp.Diagnostics.Append(r.readInto(ctx, client, projectID, region, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *quotaResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state quotaModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := r.config.NetworkV2Client()
	if err != nil {
		resp.Diagnostics.AddError("networking: building v2 client", err.Error())
		return
	}

	region := state.Region.ValueString()
	if state.Region.IsNull() || state.Region.IsUnknown() {
		region = r.config.Region
	}
	q, err := quotas.Get(ctx, client, state.ProjectID.ValueString()).Extract()
	if err != nil {
		if gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
			resp.State.RemoveResource(ctx)
			return
		}
		resp.Diagnostics.AddError("networking: reading quotas", err.Error())
		return
	}

	setNetworkQuotaState(&state, region, q)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *quotaResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state quotaModel
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

	region := state.Region.ValueString()
	if state.Region.IsNull() || state.Region.IsUnknown() {
		region = r.config.Region
	}
	projectID := state.ProjectID.ValueString()

	opts := quotas.UpdateOpts{
		FloatingIP:        quotaPtrIfChanged(plan.FloatingIP, state.FloatingIP),
		Network:           quotaPtrIfChanged(plan.Network, state.Network),
		Port:              quotaPtrIfChanged(plan.Port, state.Port),
		RBACPolicy:        quotaPtrIfChanged(plan.RBACPolicy, state.RBACPolicy),
		Router:            quotaPtrIfChanged(plan.Router, state.Router),
		SecurityGroup:     quotaPtrIfChanged(plan.SecurityGroup, state.SecurityGroup),
		SecurityGroupRule: quotaPtrIfChanged(plan.SecurityGroupRule, state.SecurityGroupRule),
		Subnet:            quotaPtrIfChanged(plan.Subnet, state.Subnet),
		SubnetPool:        quotaPtrIfChanged(plan.SubnetPool, state.SubnetPool),
	}

	if _, err := quotas.Update(ctx, client, projectID, opts).Extract(); err != nil {
		resp.Diagnostics.AddError("networking: updating quotas", err.Error())
		return
	}

	resp.Diagnostics.Append(r.readInto(ctx, client, projectID, region, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

// Delete intentionally makes no API call. Following the upstream provider, the
// resource is removed from state without resetting the project's quotas to
// their default values.
func (r *quotaResource) Delete(_ context.Context, _ resource.DeleteRequest, _ *resource.DeleteResponse) {
}

func (r *quotaResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	parts := strings.SplitN(req.ID, "/", 2)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("project_id"), parts[0])...)
	if len(parts) == 2 && parts[1] != "" {
		resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("region"), parts[1])...)
	}
}

func (r *quotaResource) readInto(ctx context.Context, client *gophercloud.ServiceClient, projectID, region string, m *quotaModel) diag.Diagnostics {
	var diags diag.Diagnostics
	q, err := quotas.Get(ctx, client, projectID).Extract()
	if err != nil {
		diags.AddError("networking: reading quotas", err.Error())
		return diags
	}
	setNetworkQuotaState(m, region, q)
	return diags
}

func setNetworkQuotaState(m *quotaModel, region string, q *quotas.Quota) {
	m.ID = types.StringValue(fmt.Sprintf("%s/%s", m.ProjectID.ValueString(), region))
	m.Region = types.StringValue(region)
	m.FloatingIP = types.Int64Value(int64(q.FloatingIP))
	m.Network = types.Int64Value(int64(q.Network))
	m.Port = types.Int64Value(int64(q.Port))
	m.RBACPolicy = types.Int64Value(int64(q.RBACPolicy))
	m.Router = types.Int64Value(int64(q.Router))
	m.SecurityGroup = types.Int64Value(int64(q.SecurityGroup))
	m.SecurityGroupRule = types.Int64Value(int64(q.SecurityGroupRule))
	m.Subnet = types.Int64Value(int64(q.Subnet))
	m.SubnetPool = types.Int64Value(int64(q.SubnetPool))
}
