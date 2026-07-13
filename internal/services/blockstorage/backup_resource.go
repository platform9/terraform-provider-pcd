// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0
//
// Ported from terraform-provider-openstack v3.4.0
// (openstack/resource_openstack_blockstorage_volume_backup_v3.go), adapted for
// the terraform-plugin-framework and PCD.

package blockstorage

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/blockstorage/v3/backups"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/boolplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/platform9/terraform-provider-pcd/internal/clients"
)

var (
	_ resource.Resource                = (*backupResource)(nil)
	_ resource.ResourceWithConfigure   = (*backupResource)(nil)
	_ resource.ResourceWithImportState = (*backupResource)(nil)
)

// NewBackupResource is the factory registered with the provider.
func NewBackupResource() resource.Resource {
	return &backupResource{}
}

type backupResource struct {
	config *clients.Config
}

type backupModel struct {
	ID          types.String `tfsdk:"id"`
	VolumeID    types.String `tfsdk:"volume_id"`
	Name        types.String `tfsdk:"name"`
	Description types.String `tfsdk:"description"`
	Force       types.Bool   `tfsdk:"force"`
	Incremental types.Bool   `tfsdk:"incremental"`
	Container   types.String `tfsdk:"container"`
	Size        types.Int64  `tfsdk:"size"`
	Status      types.String `tfsdk:"status"`
	Region      types.String `tfsdk:"region"`
}

func (r *backupResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_blockstorage_volume_backup"
}

func (r *backupResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	useState := []planmodifier.String{stringplanmodifier.UseStateForUnknown()}
	forceNew := []planmodifier.String{stringplanmodifier.RequiresReplace(), stringplanmodifier.UseStateForUnknown()}
	resp.Schema = schema.Schema{
		MarkdownDescription: "Manages a Cinder volume backup in PCD.",
		Attributes: map[string]schema.Attribute{
			"id":          schema.StringAttribute{Computed: true, MarkdownDescription: "The backup ID.", PlanModifiers: useState},
			"volume_id":   schema.StringAttribute{Required: true, MarkdownDescription: "The volume to back up. Changing this forces a new resource.", PlanModifiers: forceNew},
			"name":        schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "The name of the backup.", PlanModifiers: useState},
			"description": schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "A description of the backup.", PlanModifiers: useState},
			"force":       schema.BoolAttribute{Optional: true, Computed: true, Default: booldefault.StaticBool(false), MarkdownDescription: "Back up the volume even if it is attached/in-use. Changing this forces a new resource.", PlanModifiers: []planmodifier.Bool{boolplanmodifier.RequiresReplace()}},
			"incremental": schema.BoolAttribute{Optional: true, Computed: true, Default: booldefault.StaticBool(false), MarkdownDescription: "Whether to create an incremental backup. Changing this forces a new resource.", PlanModifiers: []planmodifier.Bool{boolplanmodifier.RequiresReplace()}},
			"container":   schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "The backup store container. Changing this forces a new resource.", PlanModifiers: forceNew},
			"size":        schema.Int64Attribute{Computed: true, MarkdownDescription: "The size of the backup in GB."},
			"status":      schema.StringAttribute{Computed: true, MarkdownDescription: "The Cinder status (e.g. available)."},
			"region":      schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "The region. Defaults to the provider's region.", PlanModifiers: useState},
		},
	}
}

func (r *backupResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.config = configureClient(req.ProviderData, &resp.Diagnostics)
}

func (r *backupResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan backupModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := r.config.BlockStorageV3Client()
	if err != nil {
		resp.Diagnostics.AddError("blockstorage: building v3 client", err.Error())
		return
	}

	backup, err := backups.Create(ctx, client, backups.CreateOpts{
		VolumeID:    plan.VolumeID.ValueString(),
		Name:        plan.Name.ValueString(),
		Description: plan.Description.ValueString(),
		Force:       plan.Force.ValueBool(),
		Incremental: plan.Incremental.ValueBool(),
		Container:   plan.Container.ValueString(),
	}).Extract()
	if err != nil {
		resp.Diagnostics.AddError("blockstorage: creating volume backup", err.Error())
		return
	}

	final, err := waitForBackupStatus(ctx, client, backup.ID, "available", 30*time.Minute)
	if err != nil {
		resp.Diagnostics.AddError("blockstorage: waiting for backup to become available", err.Error())
		return
	}

	// Build state from the object the waiter already fetched rather than a second
	// Get, so a transient read failure can't orphan the created backup.
	resp.Diagnostics.Append(r.setState(&plan, final)...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *backupResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state backupModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := r.config.BlockStorageV3Client()
	if err != nil {
		resp.Diagnostics.AddError("blockstorage: building v3 client", err.Error())
		return
	}

	notFound, diags := r.readIntoChecked(ctx, client, state.ID.ValueString(), &state)
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

func (r *backupResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state backupModel
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

	id := plan.ID.ValueString()
	if !plan.Name.Equal(state.Name) || !plan.Description.Equal(state.Description) {
		name := plan.Name.ValueString()
		desc := plan.Description.ValueString()
		if _, err := backups.Update(ctx, client, id, backups.UpdateOpts{Name: &name, Description: &desc}).Extract(); err != nil {
			resp.Diagnostics.AddError("blockstorage: updating volume backup", err.Error())
			return
		}
	}

	resp.Diagnostics.Append(r.readInto(ctx, client, id, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *backupResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state backupModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := r.config.BlockStorageV3Client()
	if err != nil {
		resp.Diagnostics.AddError("blockstorage: building v3 client", err.Error())
		return
	}

	if err := backups.Delete(ctx, client, state.ID.ValueString()).ExtractErr(); err != nil {
		if gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
			return
		}
		resp.Diagnostics.AddError("blockstorage: deleting volume backup", err.Error())
		return
	}
	if err := waitForBackupDeleted(ctx, client, state.ID.ValueString(), 20*time.Minute); err != nil {
		resp.Diagnostics.AddError("blockstorage: waiting for backup to delete", err.Error())
	}
}

func (r *backupResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, path.Root("id"), req, resp)
}

func (r *backupResource) readInto(ctx context.Context, client *gophercloud.ServiceClient, id string, m *backupModel) diag.Diagnostics {
	notFound, diags := r.readIntoChecked(ctx, client, id, m)
	if notFound {
		diags.AddError("blockstorage: reading volume backup", fmt.Sprintf("backup %s not found immediately after write", id))
	}
	return diags
}

func (r *backupResource) readIntoChecked(ctx context.Context, client *gophercloud.ServiceClient, id string, m *backupModel) (notFound bool, diags diag.Diagnostics) {
	backup, err := backups.Get(ctx, client, id).Extract()
	if err != nil {
		if gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
			return true, diags
		}
		diags.AddError("blockstorage: reading volume backup", err.Error())
		return false, diags
	}
	return false, r.setState(m, backup)
}

// setState populates the model from a backup. force is create-only (never
// returned by the API), so it is intentionally left untouched.
func (r *backupResource) setState(m *backupModel, backup *backups.Backup) diag.Diagnostics {
	m.ID = types.StringValue(backup.ID)
	m.VolumeID = types.StringValue(backup.VolumeID)
	m.Name = types.StringValue(backup.Name)
	m.Description = types.StringValue(backup.Description)
	m.Container = types.StringValue(backup.Container)
	m.Incremental = types.BoolValue(backup.IsIncremental)
	m.Size = types.Int64Value(int64(backup.Size))
	m.Status = types.StringValue(backup.Status)
	if m.Region.IsNull() || m.Region.IsUnknown() {
		m.Region = types.StringValue(r.config.Region)
	}
	return nil
}

func waitForBackupStatus(ctx context.Context, client *gophercloud.ServiceClient, id, target string, timeout time.Duration) (*backups.Backup, error) {
	deadline := time.Now().Add(timeout)
	for {
		backup, err := backups.Get(ctx, client, id).Extract()
		if err != nil {
			return nil, err
		}
		switch backup.Status {
		case target:
			return backup, nil
		case "error":
			return nil, fmt.Errorf("backup %s entered error state: %s", id, backup.FailReason)
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("timed out waiting for backup %s to reach %q (last status %q)", id, target, backup.Status)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(3 * time.Second):
		}
	}
}

func waitForBackupDeleted(ctx context.Context, client *gophercloud.ServiceClient, id string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		backup, err := backups.Get(ctx, client, id).Extract()
		if err != nil {
			if gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
				return nil
			}
			return err
		}
		if backup.Status == "error_deleting" {
			return fmt.Errorf("backup %s entered error_deleting state", id)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out waiting for backup %s to delete", id)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(3 * time.Second):
		}
	}
}
