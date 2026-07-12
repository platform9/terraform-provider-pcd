// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0
//
// Ported from terraform-provider-openstack v3.4.0
// (openstack/resource_openstack_compute_quotaset_v2.go), adapted for the
// terraform-plugin-framework and PCD.

package compute

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/quotasets"
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
	_ resource.Resource                = (*quotasetResource)(nil)
	_ resource.ResourceWithConfigure   = (*quotasetResource)(nil)
	_ resource.ResourceWithImportState = (*quotasetResource)(nil)
)

// NewQuotasetResource is the factory registered with the provider.
func NewQuotasetResource() resource.Resource {
	return &quotasetResource{}
}

type quotasetResource struct {
	config *clients.Config
}

type quotasetModel struct {
	ID                       types.String `tfsdk:"id"`
	ProjectID                types.String `tfsdk:"project_id"`
	Region                   types.String `tfsdk:"region"`
	FixedIPs                 types.Int64  `tfsdk:"fixed_ips"`
	FloatingIPs              types.Int64  `tfsdk:"floating_ips"`
	InjectedFileContentBytes types.Int64  `tfsdk:"injected_file_content_bytes"`
	InjectedFilePathBytes    types.Int64  `tfsdk:"injected_file_path_bytes"`
	InjectedFiles            types.Int64  `tfsdk:"injected_files"`
	KeyPairs                 types.Int64  `tfsdk:"key_pairs"`
	MetadataItems            types.Int64  `tfsdk:"metadata_items"`
	RAM                      types.Int64  `tfsdk:"ram"`
	SecurityGroupRules       types.Int64  `tfsdk:"security_group_rules"`
	SecurityGroups           types.Int64  `tfsdk:"security_groups"`
	Cores                    types.Int64  `tfsdk:"cores"`
	Instances                types.Int64  `tfsdk:"instances"`
	ServerGroups             types.Int64  `tfsdk:"server_groups"`
	ServerGroupMembers       types.Int64  `tfsdk:"server_group_members"`
}

func (r *quotasetResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_compute_quotaset"
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

func (r *quotasetResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Manages the Nova (compute) quotas for a project. Only the quota fields you set are " +
			"managed; fields you omit keep their server value. Destroying this resource stops managing the quotas " +
			"but does not reset them to their defaults.",
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
			"fixed_ips":                   quotaIntAttr("Quota for fixed IPs (legacy nova-network)."),
			"floating_ips":                quotaIntAttr("Quota for floating IPs (legacy nova-network)."),
			"injected_file_content_bytes": quotaIntAttr("Quota for injected file content in bytes."),
			"injected_file_path_bytes":    quotaIntAttr("Quota for injected file path length in bytes."),
			"injected_files":              quotaIntAttr("Quota for the number of injected files."),
			"key_pairs":                   quotaIntAttr("Quota for the number of key pairs."),
			"metadata_items":              quotaIntAttr("Quota for metadata items per instance."),
			"ram":                         quotaIntAttr("Quota for RAM in megabytes."),
			"security_group_rules":        quotaIntAttr("Quota for security group rules (legacy nova-network)."),
			"security_groups":             quotaIntAttr("Quota for security groups (legacy nova-network)."),
			"cores":                       quotaIntAttr("Quota for the number of instance vCPU cores."),
			"instances":                   quotaIntAttr("Quota for the number of instances."),
			"server_groups":               quotaIntAttr("Quota for the number of server groups."),
			"server_group_members":        quotaIntAttr("Quota for the number of members per server group."),
		},
	}
}

func (r *quotasetResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
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
// state (and is known), so an in-place update sends only the fields the user
// actually changed.
func quotaPtrIfChanged(plan, state types.Int64) *int {
	if plan.IsUnknown() || plan.Equal(state) {
		return nil
	}
	i := int(plan.ValueInt64())
	return &i
}

func (r *quotasetResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan quotasetModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := r.config.ComputeV2Client()
	if err != nil {
		resp.Diagnostics.AddError("compute: building v2 client", err.Error())
		return
	}

	region := plan.Region.ValueString()
	if plan.Region.IsNull() || plan.Region.IsUnknown() {
		region = r.config.Region
	}
	projectID := plan.ProjectID.ValueString()

	opts := quotasets.UpdateOpts{
		FixedIPs:                 quotaPtrIfSet(plan.FixedIPs),
		FloatingIPs:              quotaPtrIfSet(plan.FloatingIPs),
		InjectedFileContentBytes: quotaPtrIfSet(plan.InjectedFileContentBytes),
		InjectedFilePathBytes:    quotaPtrIfSet(plan.InjectedFilePathBytes),
		InjectedFiles:            quotaPtrIfSet(plan.InjectedFiles),
		KeyPairs:                 quotaPtrIfSet(plan.KeyPairs),
		MetadataItems:            quotaPtrIfSet(plan.MetadataItems),
		RAM:                      quotaPtrIfSet(plan.RAM),
		SecurityGroupRules:       quotaPtrIfSet(plan.SecurityGroupRules),
		SecurityGroups:           quotaPtrIfSet(plan.SecurityGroups),
		Cores:                    quotaPtrIfSet(plan.Cores),
		Instances:                quotaPtrIfSet(plan.Instances),
		ServerGroups:             quotaPtrIfSet(plan.ServerGroups),
		ServerGroupMembers:       quotaPtrIfSet(plan.ServerGroupMembers),
	}

	if _, err := quotasets.Update(ctx, client, projectID, opts).Extract(); err != nil {
		resp.Diagnostics.AddError("compute: setting quotas", err.Error())
		return
	}

	resp.Diagnostics.Append(r.readInto(ctx, client, projectID, region, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *quotasetResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state quotasetModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := r.config.ComputeV2Client()
	if err != nil {
		resp.Diagnostics.AddError("compute: building v2 client", err.Error())
		return
	}

	region := state.Region.ValueString()
	if state.Region.IsNull() || state.Region.IsUnknown() {
		region = r.config.Region
	}
	qs, err := quotasets.Get(ctx, client, state.ProjectID.ValueString()).Extract()
	if err != nil {
		if gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
			resp.State.RemoveResource(ctx)
			return
		}
		resp.Diagnostics.AddError("compute: reading quotas", err.Error())
		return
	}

	setQuotaState(&state, region, qs)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *quotasetResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state quotasetModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := r.config.ComputeV2Client()
	if err != nil {
		resp.Diagnostics.AddError("compute: building v2 client", err.Error())
		return
	}

	region := state.Region.ValueString()
	if state.Region.IsNull() || state.Region.IsUnknown() {
		region = r.config.Region
	}
	projectID := state.ProjectID.ValueString()

	opts := quotasets.UpdateOpts{
		FixedIPs:                 quotaPtrIfChanged(plan.FixedIPs, state.FixedIPs),
		FloatingIPs:              quotaPtrIfChanged(plan.FloatingIPs, state.FloatingIPs),
		InjectedFileContentBytes: quotaPtrIfChanged(plan.InjectedFileContentBytes, state.InjectedFileContentBytes),
		InjectedFilePathBytes:    quotaPtrIfChanged(plan.InjectedFilePathBytes, state.InjectedFilePathBytes),
		InjectedFiles:            quotaPtrIfChanged(plan.InjectedFiles, state.InjectedFiles),
		KeyPairs:                 quotaPtrIfChanged(plan.KeyPairs, state.KeyPairs),
		MetadataItems:            quotaPtrIfChanged(plan.MetadataItems, state.MetadataItems),
		RAM:                      quotaPtrIfChanged(plan.RAM, state.RAM),
		SecurityGroupRules:       quotaPtrIfChanged(plan.SecurityGroupRules, state.SecurityGroupRules),
		SecurityGroups:           quotaPtrIfChanged(plan.SecurityGroups, state.SecurityGroups),
		Cores:                    quotaPtrIfChanged(plan.Cores, state.Cores),
		Instances:                quotaPtrIfChanged(plan.Instances, state.Instances),
		ServerGroups:             quotaPtrIfChanged(plan.ServerGroups, state.ServerGroups),
		ServerGroupMembers:       quotaPtrIfChanged(plan.ServerGroupMembers, state.ServerGroupMembers),
	}

	if _, err := quotasets.Update(ctx, client, projectID, opts).Extract(); err != nil {
		resp.Diagnostics.AddError("compute: updating quotas", err.Error())
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
func (r *quotasetResource) Delete(_ context.Context, _ resource.DeleteRequest, _ *resource.DeleteResponse) {
}

func (r *quotasetResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	projectID, region := splitQuotaImportID(req.ID)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("project_id"), projectID)...)
	if region != "" {
		resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("region"), region)...)
	}
}

func (r *quotasetResource) readInto(ctx context.Context, client *gophercloud.ServiceClient, projectID, region string, m *quotasetModel) diag.Diagnostics {
	var diags diag.Diagnostics
	qs, err := quotasets.Get(ctx, client, projectID).Extract()
	if err != nil {
		diags.AddError("compute: reading quotas", err.Error())
		return diags
	}
	setQuotaState(m, region, qs)
	return diags
}

func setQuotaState(m *quotasetModel, region string, qs *quotasets.QuotaSet) {
	m.ID = types.StringValue(fmt.Sprintf("%s/%s", m.ProjectID.ValueString(), region))
	m.Region = types.StringValue(region)
	m.FixedIPs = types.Int64Value(int64(qs.FixedIPs))
	m.FloatingIPs = types.Int64Value(int64(qs.FloatingIPs))
	m.InjectedFileContentBytes = types.Int64Value(int64(qs.InjectedFileContentBytes))
	m.InjectedFilePathBytes = types.Int64Value(int64(qs.InjectedFilePathBytes))
	m.InjectedFiles = types.Int64Value(int64(qs.InjectedFiles))
	m.KeyPairs = types.Int64Value(int64(qs.KeyPairs))
	m.MetadataItems = types.Int64Value(int64(qs.MetadataItems))
	m.RAM = types.Int64Value(int64(qs.RAM))
	m.SecurityGroupRules = types.Int64Value(int64(qs.SecurityGroupRules))
	m.SecurityGroups = types.Int64Value(int64(qs.SecurityGroups))
	m.Cores = types.Int64Value(int64(qs.Cores))
	m.Instances = types.Int64Value(int64(qs.Instances))
	m.ServerGroups = types.Int64Value(int64(qs.ServerGroups))
	m.ServerGroupMembers = types.Int64Value(int64(qs.ServerGroupMembers))
}

// splitQuotaImportID parses "<project_id>/<region>" import IDs, tolerating the
// legacy bare "<project_id>" form (region returned empty).
func splitQuotaImportID(id string) (projectID, region string) {
	parts := strings.SplitN(id, "/", 2)
	if len(parts) == 2 && parts[1] != "" {
		return parts[0], parts[1]
	}
	return parts[0], ""
}
