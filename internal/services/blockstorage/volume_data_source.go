// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0
//
// Ported from terraform-provider-openstack v3.4.0
// (openstack/data_source_openstack_blockstorage_volume_v3.go), adapted for the
// terraform-plugin-framework and PCD.

package blockstorage

import (
	"context"
	"fmt"

	"github.com/gophercloud/gophercloud/v2/openstack/blockstorage/v3/volumes"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/platform9/terraform-provider-pcd/internal/clients"
)

var (
	_ datasource.DataSource              = (*volumeDataSource)(nil)
	_ datasource.DataSourceWithConfigure = (*volumeDataSource)(nil)
)

// NewVolumeDataSource is the factory registered with the provider.
func NewVolumeDataSource() datasource.DataSource {
	return &volumeDataSource{}
}

type volumeDataSource struct {
	config *clients.Config
}

type volumeDataSourceModel struct {
	ID               types.String `tfsdk:"id"`
	VolumeID         types.String `tfsdk:"volume_id"`
	Name             types.String `tfsdk:"name"`
	Size             types.Int64  `tfsdk:"size"`
	Status           types.String `tfsdk:"status"`
	VolumeType       types.String `tfsdk:"volume_type"`
	AvailabilityZone types.String `tfsdk:"availability_zone"`
	Bootable         types.Bool   `tfsdk:"bootable"`
	Region           types.String `tfsdk:"region"`
}

func (d *volumeDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_blockstorage_volume"
}

func (d *volumeDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Look up a Cinder volume by ID or name.",
		Attributes: map[string]schema.Attribute{
			"id":                schema.StringAttribute{Computed: true, MarkdownDescription: "The volume ID."},
			"volume_id":         schema.StringAttribute{Optional: true, MarkdownDescription: "Look up by volume ID (takes precedence over name)."},
			"name":              schema.StringAttribute{Optional: true, MarkdownDescription: "Look up by name."},
			"size":              schema.Int64Attribute{Computed: true, MarkdownDescription: "Size in GB."},
			"status":            schema.StringAttribute{Computed: true, MarkdownDescription: "The Cinder status."},
			"volume_type":       schema.StringAttribute{Computed: true, MarkdownDescription: "The volume type."},
			"availability_zone": schema.StringAttribute{Computed: true, MarkdownDescription: "The availability zone."},
			"bootable":          schema.BoolAttribute{Computed: true, MarkdownDescription: "Whether the volume is bootable."},
			"region":            schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "The region. Defaults to the provider's region."},
		},
	}
}

func (d *volumeDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	d.config = configureClient(req.ProviderData, &resp.Diagnostics)
}

func (d *volumeDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var data volumeDataSourceModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := d.config.BlockStorageV3Client()
	if err != nil {
		resp.Diagnostics.AddError("blockstorage: building v3 client", err.Error())
		return
	}

	var vol *volumes.Volume
	if v := data.VolumeID.ValueString(); v != "" {
		vol, err = volumes.Get(ctx, client, v).Extract()
		if err != nil {
			resp.Diagnostics.AddError("blockstorage: getting volume", err.Error())
			return
		}
	} else {
		pages, err := volumes.List(client, volumes.ListOpts{Name: data.Name.ValueString()}).AllPages(ctx)
		if err != nil {
			resp.Diagnostics.AddError("blockstorage: listing volumes", err.Error())
			return
		}
		all, err := volumes.ExtractVolumes(pages)
		if err != nil {
			resp.Diagnostics.AddError("blockstorage: extracting volumes", err.Error())
			return
		}
		switch len(all) {
		case 0:
			resp.Diagnostics.AddError("No volume found", "No volume matched the given criteria.")
			return
		case 1:
			vol = &all[0]
		default:
			resp.Diagnostics.AddError("Multiple volumes found", fmt.Sprintf("%d volumes matched; refine the criteria.", len(all)))
			return
		}
	}

	data.ID = types.StringValue(vol.ID)
	data.VolumeID = types.StringValue(vol.ID)
	data.Name = types.StringValue(vol.Name)
	data.Size = types.Int64Value(int64(vol.Size))
	data.Status = types.StringValue(vol.Status)
	data.VolumeType = types.StringValue(vol.VolumeType)
	data.AvailabilityZone = types.StringValue(vol.AvailabilityZone)
	data.Bootable = types.BoolValue(vol.Bootable == "true")
	if data.Region.IsNull() || data.Region.IsUnknown() {
		data.Region = types.StringValue(d.config.Region)
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}
