// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0
//
// Ported from terraform-provider-openstack v3.4.0
// (openstack/data_source_openstack_images_image_ids_v2.go), adapted for the
// terraform-plugin-framework and PCD.

package images

import (
	"context"
	"fmt"

	"github.com/gophercloud/gophercloud/v2/openstack/image/v2/images"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/platform9/terraform-provider-pcd/internal/clients"
)

var (
	_ datasource.DataSource              = (*imageIDsDataSource)(nil)
	_ datasource.DataSourceWithConfigure = (*imageIDsDataSource)(nil)
)

// NewImageIDsDataSource is the factory registered with the provider.
func NewImageIDsDataSource() datasource.DataSource {
	return &imageIDsDataSource{}
}

type imageIDsDataSource struct {
	config *clients.Config
}

type imageIDsDataSourceModel struct {
	ID         types.String `tfsdk:"id"`
	Name       types.String `tfsdk:"name"`
	Owner      types.String `tfsdk:"owner"`
	Tag        types.String `tfsdk:"tag"`
	Visibility types.String `tfsdk:"visibility"`
	IDs        types.List   `tfsdk:"ids"`
	Region     types.String `tfsdk:"region"`
}

func (d *imageIDsDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_images_image_ids"
}

func (d *imageIDsDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Return the IDs of images in PCD's Glance service matching the given filters.",
		Attributes: map[string]schema.Attribute{
			"id":         schema.StringAttribute{Computed: true, MarkdownDescription: "Data source identifier."},
			"name":       schema.StringAttribute{Optional: true, MarkdownDescription: "Filter by exact image name."},
			"owner":      schema.StringAttribute{Optional: true, MarkdownDescription: "Filter by owning project."},
			"tag":        schema.StringAttribute{Optional: true, MarkdownDescription: "Filter by a required tag."},
			"visibility": schema.StringAttribute{Optional: true, MarkdownDescription: "Filter by visibility."},
			"ids":        schema.ListAttribute{Computed: true, ElementType: types.StringType, MarkdownDescription: "The matching image IDs."},
			"region":     schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "The region. Defaults to the provider's region."},
		},
	}
}

func (d *imageIDsDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
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

func (d *imageIDsDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var data imageIDsDataSourceModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := d.config.ImageV2Client()
	if err != nil {
		resp.Diagnostics.AddError("images: building v2 client", err.Error())
		return
	}

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

	ids := make([]string, 0, len(all))
	for _, img := range all {
		ids = append(ids, img.ID)
	}
	idList, diags := types.ListValueFrom(ctx, types.StringType, ids)
	resp.Diagnostics.Append(diags...)
	data.IDs = idList

	if data.Region.IsNull() || data.Region.IsUnknown() {
		data.Region = types.StringValue(d.config.Region)
	}
	data.ID = types.StringValue(d.config.Region + ":images")
	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}
