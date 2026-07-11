// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0
//
// Ported from terraform-provider-openstack v3.4.0
// (openstack/data_source_openstack_compute_availability_zones_v2.go), adapted
// for the terraform-plugin-framework and PCD.

package compute

import (
	"context"

	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/availabilityzones"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/platform9/terraform-provider-pcd/internal/clients"
)

var (
	_ datasource.DataSource              = (*azDataSource)(nil)
	_ datasource.DataSourceWithConfigure = (*azDataSource)(nil)
)

// NewAvailabilityZonesDataSource is the factory registered with the provider.
func NewAvailabilityZonesDataSource() datasource.DataSource {
	return &azDataSource{}
}

type azDataSource struct {
	config *clients.Config
}

type azDataSourceModel struct {
	ID     types.String `tfsdk:"id"`
	State  types.String `tfsdk:"state"`
	Names  types.List   `tfsdk:"names"`
	Region types.String `tfsdk:"region"`
}

func (d *azDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_compute_availability_zones"
}

func (d *azDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Return the compute availability zones.",
		Attributes: map[string]schema.Attribute{
			"id":     schema.StringAttribute{Computed: true, MarkdownDescription: "Data source identifier."},
			"state":  schema.StringAttribute{Optional: true, MarkdownDescription: "Filter by state: available (default) or unavailable."},
			"names":  schema.ListAttribute{Computed: true, ElementType: types.StringType, MarkdownDescription: "The matching availability zone names."},
			"region": schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "The region. Defaults to the provider's region."},
		},
	}
}

func (d *azDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	d.config = configureClient(req.ProviderData, &resp.Diagnostics)
}

func (d *azDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var data azDataSourceModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := d.config.ComputeV2Client()
	if err != nil {
		resp.Diagnostics.AddError("compute: building v2 client", err.Error())
		return
	}

	pages, err := availabilityzones.List(client).AllPages(ctx)
	if err != nil {
		resp.Diagnostics.AddError("compute: listing availability zones", err.Error())
		return
	}
	zones, err := availabilityzones.ExtractAvailabilityZones(pages)
	if err != nil {
		resp.Diagnostics.AddError("compute: extracting availability zones", err.Error())
		return
	}

	wantAvailable := data.State.ValueString() != "unavailable"
	names := make([]string, 0, len(zones))
	for _, z := range zones {
		if z.ZoneState.Available == wantAvailable {
			names = append(names, z.ZoneName)
		}
	}

	nameList, diags := types.ListValueFrom(ctx, types.StringType, names)
	resp.Diagnostics.Append(diags...)
	data.Names = nameList
	if data.Region.IsNull() || data.Region.IsUnknown() {
		data.Region = types.StringValue(d.config.Region)
	}
	data.ID = types.StringValue(d.config.Region + ":azs")
	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}
