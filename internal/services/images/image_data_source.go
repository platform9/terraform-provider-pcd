// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0
//
// Ported from terraform-provider-openstack v3.4.0
// (openstack/data_source_openstack_images_image_v2.go), adapted for the
// terraform-plugin-framework and PCD.

package images

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/gophercloud/gophercloud/v2/openstack/image/v2/images"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/platform9/terraform-provider-pcd/internal/clients"
)

var (
	_ datasource.DataSource              = (*imageDataSource)(nil)
	_ datasource.DataSourceWithConfigure = (*imageDataSource)(nil)
)

// NewImageDataSource is the factory registered with the provider.
func NewImageDataSource() datasource.DataSource {
	return &imageDataSource{}
}

type imageDataSource struct {
	config *clients.Config
}

type imageDataSourceModel struct {
	ID              types.String `tfsdk:"id"`
	ImageID         types.String `tfsdk:"image_id"`
	Name            types.String `tfsdk:"name"`
	Owner           types.String `tfsdk:"owner"`
	Tag             types.String `tfsdk:"tag"`
	Visibility      types.String `tfsdk:"visibility"`
	MostRecent      types.Bool   `tfsdk:"most_recent"`
	ContainerFormat types.String `tfsdk:"container_format"`
	DiskFormat      types.String `tfsdk:"disk_format"`
	MinDiskGB       types.Int64  `tfsdk:"min_disk_gb"`
	MinRAMMB        types.Int64  `tfsdk:"min_ram_mb"`
	Protected       types.Bool   `tfsdk:"protected"`
	Checksum        types.String `tfsdk:"checksum"`
	SizeBytes       types.Int64  `tfsdk:"size_bytes"`
	CreatedAt       types.String `tfsdk:"created_at"`
	UpdatedAt       types.String `tfsdk:"updated_at"`
	Tags            types.Set    `tfsdk:"tags"`
	Region          types.String `tfsdk:"region"`
}

func (d *imageDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_images_image"
}

func (d *imageDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Look up a single image in PCD's Glance service by id or by filters.",
		Attributes: map[string]schema.Attribute{
			"id":               schema.StringAttribute{Computed: true, MarkdownDescription: "The image ID."},
			"image_id":         schema.StringAttribute{Optional: true, MarkdownDescription: "Look up by image ID (takes precedence over filters)."},
			"name":             schema.StringAttribute{Optional: true, MarkdownDescription: "Filter by exact image name."},
			"owner":            schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "Filter by (and report) the owning project."},
			"tag":              schema.StringAttribute{Optional: true, MarkdownDescription: "Filter by a required tag."},
			"visibility":       schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "Filter by (and report) visibility."},
			"most_recent":      schema.BoolAttribute{Optional: true, MarkdownDescription: "If multiple images match, select the most recently created."},
			"container_format": schema.StringAttribute{Computed: true, MarkdownDescription: "The container format."},
			"disk_format":      schema.StringAttribute{Computed: true, MarkdownDescription: "The disk format."},
			"min_disk_gb":      schema.Int64Attribute{Computed: true, MarkdownDescription: "Minimum disk (GB)."},
			"min_ram_mb":       schema.Int64Attribute{Computed: true, MarkdownDescription: "Minimum RAM (MB)."},
			"protected":        schema.BoolAttribute{Computed: true, MarkdownDescription: "Whether the image is protected."},
			"checksum":         schema.StringAttribute{Computed: true, MarkdownDescription: "md5 checksum."},
			"size_bytes":       schema.Int64Attribute{Computed: true, MarkdownDescription: "Size in bytes."},
			"created_at":       schema.StringAttribute{Computed: true, MarkdownDescription: "Creation timestamp (RFC3339)."},
			"updated_at":       schema.StringAttribute{Computed: true, MarkdownDescription: "Last-update timestamp (RFC3339)."},
			"tags":             schema.SetAttribute{Computed: true, ElementType: types.StringType, MarkdownDescription: "Image tags."},
			"region":           schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "The region. Defaults to the provider's region."},
		},
	}
}

func (d *imageDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	if req.ProviderData == nil {
		return
	}
	config, ok := req.ProviderData.(*clients.Config)
	if !ok {
		resp.Diagnostics.AddError("Unexpected provider data type",
			fmt.Sprintf("Expected *clients.Config, got %T.", req.ProviderData))
		return
	}
	d.config = config
}

func (d *imageDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var data imageDataSourceModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := d.config.ImageV2Client()
	if err != nil {
		resp.Diagnostics.AddError("images: building v2 client", err.Error())
		return
	}

	var img *images.Image
	if v := data.ImageID.ValueString(); v != "" {
		img, err = images.Get(ctx, client, v).Extract()
		if err != nil {
			resp.Diagnostics.AddError("images: getting image by id", err.Error())
			return
		}
	} else {
		listOpts := images.ListOpts{
			Name:  data.Name.ValueString(),
			Owner: data.Owner.ValueString(),
		}
		if v := data.Visibility.ValueString(); v != "" {
			listOpts.Visibility = images.ImageVisibility(v)
		}
		if v := data.Tag.ValueString(); v != "" {
			listOpts.Tags = []string{v}
		}
		pages, err := images.List(client, listOpts).AllPages(ctx)
		if err != nil {
			resp.Diagnostics.AddError("images: listing images", err.Error())
			return
		}
		all, err := images.ExtractImages(pages)
		if err != nil {
			resp.Diagnostics.AddError("images: extracting images", err.Error())
			return
		}
		switch {
		case len(all) == 0:
			resp.Diagnostics.AddError("No image found", "No image matched the given criteria.")
			return
		case len(all) == 1:
			img = &all[0]
		case data.MostRecent.ValueBool():
			sort.Slice(all, func(i, j int) bool { return all[i].CreatedAt.After(all[j].CreatedAt) })
			img = &all[0]
		default:
			resp.Diagnostics.AddError("Multiple images found",
				fmt.Sprintf("%d images matched; set most_recent or refine the filters.", len(all)))
			return
		}
	}

	data.ID = types.StringValue(img.ID)
	data.Name = types.StringValue(img.Name)
	data.Owner = types.StringValue(img.Owner)
	data.Visibility = types.StringValue(string(img.Visibility))
	data.ContainerFormat = types.StringValue(img.ContainerFormat)
	data.DiskFormat = types.StringValue(img.DiskFormat)
	data.MinDiskGB = types.Int64Value(int64(img.MinDiskGigabytes))
	data.MinRAMMB = types.Int64Value(int64(img.MinRAMMegabytes))
	data.Protected = types.BoolValue(img.Protected)
	data.Checksum = types.StringValue(img.Checksum)
	data.SizeBytes = types.Int64Value(img.SizeBytes)
	data.CreatedAt = types.StringValue(img.CreatedAt.Format(time.RFC3339))
	data.UpdatedAt = types.StringValue(img.UpdatedAt.Format(time.RFC3339))

	tagVals := img.Tags
	if tagVals == nil {
		tagVals = []string{}
	}
	tags, diags := types.SetValueFrom(ctx, types.StringType, tagVals)
	resp.Diagnostics.Append(diags...)
	data.Tags = tags

	if data.Region.IsNull() || data.Region.IsUnknown() {
		data.Region = types.StringValue(d.config.Region)
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}
