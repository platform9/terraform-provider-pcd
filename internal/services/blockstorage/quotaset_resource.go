// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0
//
// Ported from terraform-provider-openstack v3.4.0
// (openstack/resource_openstack_blockstorage_quotaset_v3.go), adapted for the
// terraform-plugin-framework and PCD. The upstream resource also exposes a
// per-volume-type quota map (volume_type_quota); that is deliberately deferred
// (see DECISIONS.md) and this resource manages the scalar quotas only.

package blockstorage

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/blockstorage/v3/quotasets"
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
	ID                 types.String `tfsdk:"id"`
	ProjectID          types.String `tfsdk:"project_id"`
	Region             types.String `tfsdk:"region"`
	Volumes            types.Int64  `tfsdk:"volumes"`
	Snapshots          types.Int64  `tfsdk:"snapshots"`
	Gigabytes          types.Int64  `tfsdk:"gigabytes"`
	PerVolumeGigabytes types.Int64  `tfsdk:"per_volume_gigabytes"`
	Backups            types.Int64  `tfsdk:"backups"`
	BackupGigabytes    types.Int64  `tfsdk:"backup_gigabytes"`
	Groups             types.Int64  `tfsdk:"groups"`
}

func (r *quotasetResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_blockstorage_quotaset"
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
		MarkdownDescription: "Manages the Cinder (block storage) quotas for a project. Only the quota fields you set " +
			"are managed; fields you omit keep their server value. Destroying this resource stops managing the quotas " +
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
			"volumes":              quotaIntAttr("Quota for the number of volumes."),
			"snapshots":            quotaIntAttr("Quota for the number of snapshots."),
			"gigabytes":            quotaIntAttr("Quota for total volume storage in gigabytes."),
			"per_volume_gigabytes": quotaIntAttr("Quota for the size of a single volume in gigabytes."),
			"backups":              quotaIntAttr("Quota for the number of backups."),
			"backup_gigabytes":     quotaIntAttr("Quota for total backup storage in gigabytes."),
			"groups":               quotaIntAttr("Quota for the number of volume groups."),
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
// state (and is known), so an in-place update sends only changed fields.
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

	client, err := r.config.BlockStorageV3Client()
	if err != nil {
		resp.Diagnostics.AddError("blockstorage: building v3 client", err.Error())
		return
	}

	region := plan.Region.ValueString()
	if plan.Region.IsNull() || plan.Region.IsUnknown() {
		region = r.config.Region
	}
	projectID := plan.ProjectID.ValueString()

	opts := quotasets.UpdateOpts{
		Volumes:            quotaPtrIfSet(plan.Volumes),
		Snapshots:          quotaPtrIfSet(plan.Snapshots),
		Gigabytes:          quotaPtrIfSet(plan.Gigabytes),
		PerVolumeGigabytes: quotaPtrIfSet(plan.PerVolumeGigabytes),
		Backups:            quotaPtrIfSet(plan.Backups),
		BackupGigabytes:    quotaPtrIfSet(plan.BackupGigabytes),
		Groups:             quotaPtrIfSet(plan.Groups),
	}

	if _, err := quotasets.Update(ctx, client, projectID, opts).Extract(); err != nil {
		resp.Diagnostics.AddError("blockstorage: setting quotas", err.Error())
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

	client, err := r.config.BlockStorageV3Client()
	if err != nil {
		resp.Diagnostics.AddError("blockstorage: building v3 client", err.Error())
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
		resp.Diagnostics.AddError("blockstorage: reading quotas", err.Error())
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

	client, err := r.config.BlockStorageV3Client()
	if err != nil {
		resp.Diagnostics.AddError("blockstorage: building v3 client", err.Error())
		return
	}

	region := state.Region.ValueString()
	if state.Region.IsNull() || state.Region.IsUnknown() {
		region = r.config.Region
	}
	projectID := state.ProjectID.ValueString()

	opts := quotasets.UpdateOpts{
		Volumes:            quotaPtrIfChanged(plan.Volumes, state.Volumes),
		Snapshots:          quotaPtrIfChanged(plan.Snapshots, state.Snapshots),
		Gigabytes:          quotaPtrIfChanged(plan.Gigabytes, state.Gigabytes),
		PerVolumeGigabytes: quotaPtrIfChanged(plan.PerVolumeGigabytes, state.PerVolumeGigabytes),
		Backups:            quotaPtrIfChanged(plan.Backups, state.Backups),
		BackupGigabytes:    quotaPtrIfChanged(plan.BackupGigabytes, state.BackupGigabytes),
		Groups:             quotaPtrIfChanged(plan.Groups, state.Groups),
	}

	if _, err := quotasets.Update(ctx, client, projectID, opts).Extract(); err != nil {
		resp.Diagnostics.AddError("blockstorage: updating quotas", err.Error())
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
	parts := strings.SplitN(req.ID, "/", 2)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("project_id"), parts[0])...)
	if len(parts) == 2 && parts[1] != "" {
		resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("region"), parts[1])...)
	}
}

func (r *quotasetResource) readInto(ctx context.Context, client *gophercloud.ServiceClient, projectID, region string, m *quotasetModel) diag.Diagnostics {
	var diags diag.Diagnostics
	qs, err := quotasets.Get(ctx, client, projectID).Extract()
	if err != nil {
		diags.AddError("blockstorage: reading quotas", err.Error())
		return diags
	}
	setQuotaState(m, region, qs)
	return diags
}

func setQuotaState(m *quotasetModel, region string, qs *quotasets.QuotaSet) {
	m.ID = types.StringValue(fmt.Sprintf("%s/%s", m.ProjectID.ValueString(), region))
	m.Region = types.StringValue(region)
	m.Volumes = types.Int64Value(int64(qs.Volumes))
	m.Snapshots = types.Int64Value(int64(qs.Snapshots))
	m.Gigabytes = types.Int64Value(int64(qs.Gigabytes))
	m.PerVolumeGigabytes = types.Int64Value(int64(qs.PerVolumeGigabytes))
	m.Backups = types.Int64Value(int64(qs.Backups))
	m.BackupGigabytes = types.Int64Value(int64(qs.BackupGigabytes))
	m.Groups = types.Int64Value(int64(qs.Groups))
}
