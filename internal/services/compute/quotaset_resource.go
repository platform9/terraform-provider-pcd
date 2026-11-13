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
	"net/url"
	"strings"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/quotasets"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/int64planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/platform9/terraform-provider-pcd/internal/clients"
)

var (
	_ resource.Resource                = (*quotasetResource)(nil)
	_ resource.ResourceWithConfigure   = (*quotasetResource)(nil)
	_ resource.ResourceWithImportState = (*quotasetResource)(nil)
	_ resource.ResourceWithModifyPlan  = (*quotasetResource)(nil)
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
	UserID                   types.String `tfsdk:"user_id"`
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
		MarkdownDescription: "Manages the Nova (compute) quotas for a project, or with `user_id` for one user in " +
			"a project (the PCD UI's per-user quotas on the Users page). Only the quota fields you set are " +
			"managed; fields you omit keep their server value.\n\n" +
			"Destroying a project quota set stops managing the quotas but does not reset them to their " +
			"defaults. Destroying a user quota set removes that user's overrides in the project, so the user " +
			"falls back to the project's quotas: Nova removes **all** of the user's overrides there, including " +
			"fields this resource does not set and overrides made in the PCD UI. Replacing a user quota set " +
			"create-before-destroy without changing `project_id`, `region` or `user_id` (for example " +
			"`terraform apply -replace` under that lifecycle setting) leaves the user without overrides: the " +
			"old object's destroy removes all of them, including the ones the new object just set, and the next " +
			"apply sets again those that differ from the project's quotas.\n\n" +
			"A user's quota must lie between what the user already uses and what the project has left, and a " +
			"project's quota cannot drop below the sum of its users' quotas. So when one configuration raises " +
			"both, make the user quota set `depends_on` the project one; when it lowers both, lower the user " +
			"quota first, in its own apply. Nova accepts any `user_id` without checking that the user exists " +
			"or belongs to the project, and leaves a deleted user's overrides behind. In user mode, fields you " +
			"omit read back the project's values unless the user already has an override for them (set in the " +
			"PCD UI, or by an earlier configuration), so only the fields without an override follow later " +
			"project changes. Every field you omit shows as \"(known after apply)\" whenever the user quota set changes. " +
			"A region whose Nova " +
			"uses unified limits ignores per-user quotas: the values you set then read back as the project's, " +
			"and the apply fails with an inconsistent-result error. A project deleted outside Terraform makes " +
			"Nova answer 400, not 404, so refresh reports an error instead of dropping the resource; remove it " +
			"from state.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{Computed: true, MarkdownDescription: "The composite ID: `<project_id>/<region>`, or `<project_id>/<region>/<user_id>` with `user_id`.", PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()}},
			"project_id": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "The project (tenant) the quotas apply to. Changing this forces a new resource.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"user_id": schema.StringAttribute{
				Optional: true,
				MarkdownDescription: "Manage the quotas of this user within `project_id` instead of the project's own. " +
					"Changing this forces a new resource. The PCD UI does not offer per-user quotas for a user with a " +
					"system or domain role, or with only the `reader` role in the project; Nova itself does not check.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
				Validators:    []validator.String{stringvalidator.LengthAtLeast(1)},
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

	client, err := r.config.ForRegion(plan.Region.ValueString()).ComputeV2Client()
	if err != nil {
		resp.Diagnostics.AddError("compute: building v2 client", err.Error())
		return
	}

	region := plan.Region.ValueString()
	if plan.Region.IsNull() || plan.Region.IsUnknown() {
		region = r.config.Region
	}
	projectID := plan.ProjectID.ValueString()
	userID := plan.UserID.ValueString()

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

	if err := updateQuotaSet(ctx, client, projectID, userID, opts); err != nil {
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

	client, err := r.config.ForRegion(state.Region.ValueString()).ComputeV2Client()
	if err != nil {
		resp.Diagnostics.AddError("compute: building v2 client", err.Error())
		return
	}

	region := state.Region.ValueString()
	if state.Region.IsNull() || state.Region.IsUnknown() {
		region = r.config.Region
	}
	qs, err := getQuotaSet(ctx, client, state.ProjectID.ValueString(), state.UserID.ValueString())
	if err != nil {
		if gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
			resp.State.RemoveResource(ctx)
			return
		}
		resp.Diagnostics.AddError("compute: reading quotas", err.Error())
		return
	}
	// A 200 without the object decodes to nil. It is not a not-found: that
	// would drop the row from state.
	if qs == nil {
		resp.Diagnostics.AddError("compute: reading quotas",
			fmt.Sprintf("The Compute API answered without a quota_set object for project ID %q.", state.ProjectID.ValueString()))
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

	client, err := r.config.ForRegion(plan.Region.ValueString()).ComputeV2Client()
	if err != nil {
		resp.Diagnostics.AddError("compute: building v2 client", err.Error())
		return
	}

	region := state.Region.ValueString()
	if state.Region.IsNull() || state.Region.IsUnknown() {
		region = r.config.Region
	}
	projectID := state.ProjectID.ValueString()
	userID := state.UserID.ValueString()

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

	if err := updateQuotaSet(ctx, client, projectID, userID, opts); err != nil {
		resp.Diagnostics.AddError("compute: updating quotas", err.Error())
		return
	}

	resp.Diagnostics.Append(r.readInto(ctx, client, projectID, region, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

// Delete of a project quota set intentionally makes no API call. Following the
// upstream provider, the resource is removed from state without resetting the
// project's quotas to their default values. A user quota set is different: its
// overrides exist only because this resource created them, and left behind
// they keep capping the user and stop the project's quota from going below
// their sum, so Delete removes them (DELETE ?user_id=, which removes every
// override the user has in the project).
func (r *quotasetResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state quotasetModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() || state.UserID.ValueString() == "" {
		return
	}

	client, err := r.config.ForRegion(state.Region.ValueString()).ComputeV2Client()
	if err != nil {
		resp.Diagnostics.AddError("compute: building v2 client", err.Error())
		return
	}
	if err := deleteUserQuotaSet(ctx, client, state.ProjectID.ValueString(), state.UserID.ValueString()); err != nil {
		resp.Diagnostics.AddError("compute: removing the user's quotas", err.Error())
	}
}

// ModifyPlan plans the quota fields a user quota set does not configure as
// unknown whenever the resource changes. Nova reports the project's value for
// every field the user has no override for, so such a field can change with
// the project's quota set in the same apply; kept at its prior value by
// UseStateForUnknown, it would make the apply fail with an inconsistent
// result. Update sends only known values, so nothing extra is written.
func (r *quotasetResource) ModifyPlan(ctx context.Context, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	if req.State.Raw.IsNull() || req.Plan.Raw.IsNull() || req.Plan.Raw.Equal(req.State.Raw) {
		return
	}
	var userID types.String
	resp.Diagnostics.Append(req.Plan.GetAttribute(ctx, path.Root("user_id"), &userID)...)
	if resp.Diagnostics.HasError() || userID.ValueString() == "" {
		return
	}
	for name, attr := range req.Plan.Schema.GetAttributes() {
		if _, ok := attr.(schema.Int64Attribute); !ok {
			continue // every Int64 attribute is a quota field
		}
		var configured types.Int64
		resp.Diagnostics.Append(req.Config.GetAttribute(ctx, path.Root(name), &configured)...)
		if configured.IsNull() {
			resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, path.Root(name), types.Int64Unknown())...)
		}
	}
}

func (r *quotasetResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	projectID, region, userID := splitQuotaImportID(req.ID)
	// An empty project ID names no project: the refresh after the import would
	// read .../os-quota-sets/ instead.
	if projectID == "" {
		resp.Diagnostics.AddError("Invalid import ID",
			fmt.Sprintf("expected <project_id>/<region> or <project_id>/<region>/<user_id>, got %q", req.ID))
		return
	}
	// A third part that is empty or holds another slash names no user: the
	// refresh would read the project's quotas, or a user Nova does not check.
	if strings.Count(req.ID, "/") >= 2 && (userID == "" || strings.Contains(userID, "/")) {
		resp.Diagnostics.AddError("Invalid import ID",
			fmt.Sprintf("expected <project_id>/<region> or <project_id>/<region>/<user_id>, got %q", req.ID))
		return
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("project_id"), projectID)...)
	if region != "" {
		resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("region"), region)...)
	}
	if userID != "" {
		resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("user_id"), userID)...)
	}
}

// readInto reads the project's quotas into m, or, when m has a user_id, that
// user's quotas within the project.
func (r *quotasetResource) readInto(ctx context.Context, client *gophercloud.ServiceClient, projectID, region string, m *quotasetModel) diag.Diagnostics {
	var diags diag.Diagnostics
	qs, err := getQuotaSet(ctx, client, projectID, m.UserID.ValueString())
	if err != nil {
		diags.AddError("compute: reading quotas", err.Error())
		return diags
	}
	// A 200 without the object decodes to nil.
	if qs == nil {
		diags.AddError("compute: reading quotas",
			fmt.Sprintf("The Compute API answered without a quota_set object for project ID %q.", projectID))
		return diags
	}
	setQuotaState(m, region, qs)
	return diags
}

// quotaSetURL is /os-quota-sets/<project_id>, plus ?user_id=<user_id> for a
// user's quotas. gophercloud's quotasets package has no user variant, so the
// requests below reuse its types and build the URL here; for a project the URL
// is the one gophercloud builds.
func quotaSetURL(client *gophercloud.ServiceClient, projectID, userID string) string {
	u := client.ServiceURL("os-quota-sets", projectID)
	if userID != "" {
		u += "?user_id=" + url.QueryEscape(userID)
	}
	return u
}

// getQuotaSet reads a project's quotas, or a user's within it. Nova answers a
// user's GET with the project's value for every field the user has no
// override for.
func getQuotaSet(ctx context.Context, client *gophercloud.ServiceClient, projectID, userID string) (*quotasets.QuotaSet, error) {
	var r quotasets.GetResult
	resp, err := client.Get(ctx, quotaSetURL(client, projectID, userID), &r.Body, nil)
	_, r.Header, r.Err = gophercloud.ParseResponse(resp, err)
	return r.Extract()
}

// updateQuotaSet sends the fields opts sets to a project's or a user's quotas.
func updateQuotaSet(ctx context.Context, client *gophercloud.ServiceClient, projectID, userID string, opts quotasets.UpdateOpts) error {
	body, err := opts.ToComputeQuotaUpdateMap()
	if err != nil {
		return err
	}
	var r quotasets.UpdateResult
	resp, err := client.Put(ctx, quotaSetURL(client, projectID, userID), body, &r.Body, &gophercloud.RequestOpts{OkCodes: []int{200}})
	_, r.Header, r.Err = gophercloud.ParseResponse(resp, err)
	return r.Err
}

// deleteUserQuotaSet removes every override the user has in the project. A
// 404 means there is nothing left to remove.
func deleteUserQuotaSet(ctx context.Context, client *gophercloud.ServiceClient, projectID, userID string) error {
	resp, err := client.Delete(ctx, quotaSetURL(client, projectID, userID), nil)
	_, _, err = gophercloud.ParseResponse(resp, err)
	if err != nil && !gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
		return err
	}
	return nil
}

// quotaSetID is <project_id>/<region>, with /<user_id> appended for a user's
// quotas, so the IDs of existing project quota sets do not change.
func quotaSetID(projectID, region, userID string) string {
	if userID == "" {
		return fmt.Sprintf("%s/%s", projectID, region)
	}
	return fmt.Sprintf("%s/%s/%s", projectID, region, userID)
}

func setQuotaState(m *quotasetModel, region string, qs *quotasets.QuotaSet) {
	m.ID = types.StringValue(quotaSetID(m.ProjectID.ValueString(), region, m.UserID.ValueString()))
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

// splitQuotaImportID parses "<project_id>/<region>/<user_id>" and
// "<project_id>/<region>" import IDs, tolerating the legacy bare
// "<project_id>" form; the parts not given are returned empty.
func splitQuotaImportID(id string) (projectID, region, userID string) {
	parts := strings.SplitN(id, "/", 3)
	projectID = parts[0]
	if len(parts) > 1 {
		region = parts[1]
	}
	if len(parts) > 2 {
		userID = parts[2]
	}
	return projectID, region, userID
}
