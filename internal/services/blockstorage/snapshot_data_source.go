// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0
//
// Ported from terraform-provider-openstack v3.4.0
// (openstack/data_source_openstack_blockstorage_snapshot_v3.go), adapted for the
// terraform-plugin-framework and PCD.

package blockstorage

import (
	"context"
	"fmt"

	"github.com/gophercloud/gophercloud/v2/openstack/blockstorage/v3/snapshots"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/platform9/terraform-provider-pcd/internal/clients"
)

var (
	_ datasource.DataSource              = (*snapshotDataSource)(nil)
	_ datasource.DataSourceWithConfigure = (*snapshotDataSource)(nil)
)

// NewSnapshotDataSource is the factory registered with the provider.
func NewSnapshotDataSource() datasource.DataSource {
	return &snapshotDataSource{}
}

type snapshotDataSource struct {
	config *clients.Config
}

type snapshotDataSourceModel struct {
	ID          types.String `tfsdk:"id"`
	SnapshotID  types.String `tfsdk:"snapshot_id"`
	Name        types.String `tfsdk:"name"`
	VolumeID    types.String `tfsdk:"volume_id"`
	Size        types.Int64  `tfsdk:"size"`
	Status      types.String `tfsdk:"status"`
	Description types.String `tfsdk:"description"`
	Region      types.String `tfsdk:"region"`
}

func (d *snapshotDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_blockstorage_snapshot"
}

func (d *snapshotDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Look up a Cinder volume snapshot by ID or filters.",
		Attributes: map[string]schema.Attribute{
			"id":          schema.StringAttribute{Computed: true, MarkdownDescription: "The snapshot ID."},
			"snapshot_id": schema.StringAttribute{Optional: true, MarkdownDescription: "Look up by snapshot ID (takes precedence over filters)."},
			"name":        schema.StringAttribute{Optional: true, MarkdownDescription: "Filter by name."},
			"volume_id":   schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "Filter by (and report) the source volume."},
			"size":        schema.Int64Attribute{Computed: true, MarkdownDescription: "Size in GB."},
			"status":      schema.StringAttribute{Computed: true, MarkdownDescription: "The Cinder status."},
			"description": schema.StringAttribute{Computed: true, MarkdownDescription: "The snapshot description."},
			"region":      schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "The region. Defaults to the provider's region."},
		},
	}
}

func (d *snapshotDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	d.config = configureClient(req.ProviderData, &resp.Diagnostics)
}

func (d *snapshotDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var data snapshotDataSourceModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := d.config.BlockStorageV3Client()
	if err != nil {
		resp.Diagnostics.AddError("blockstorage: building v3 client", err.Error())
		return
	}

	var snap *snapshots.Snapshot
	if v := data.SnapshotID.ValueString(); v != "" {
		snap, err = snapshots.Get(ctx, client, v).Extract()
		if err != nil {
			resp.Diagnostics.AddError("blockstorage: getting snapshot", err.Error())
			return
		}
	} else {
		pages, err := snapshots.List(client, snapshots.ListOpts{
			Name:     data.Name.ValueString(),
			VolumeID: data.VolumeID.ValueString(),
		}).AllPages(ctx)
		if err != nil {
			resp.Diagnostics.AddError("blockstorage: listing snapshots", err.Error())
			return
		}
		all, err := snapshots.ExtractSnapshots(pages)
		if err != nil {
			resp.Diagnostics.AddError("blockstorage: extracting snapshots", err.Error())
			return
		}
		switch len(all) {
		case 0:
			resp.Diagnostics.AddError("No snapshot found", "No snapshot matched the given criteria.")
			return
		case 1:
			snap = &all[0]
		default:
			resp.Diagnostics.AddError("Multiple snapshots found", fmt.Sprintf("%d snapshots matched; refine the criteria.", len(all)))
			return
		}
	}

	data.ID = types.StringValue(snap.ID)
	data.SnapshotID = types.StringValue(snap.ID)
	data.Name = types.StringValue(snap.Name)
	data.VolumeID = types.StringValue(snap.VolumeID)
	data.Size = types.Int64Value(int64(snap.Size))
	data.Status = types.StringValue(snap.Status)
	data.Description = types.StringValue(snap.Description)
	if data.Region.IsNull() || data.Region.IsUnknown() {
		data.Region = types.StringValue(d.config.Region)
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}
