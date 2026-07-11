// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0
//
// Ported from terraform-provider-openstack v3.4.0
// (openstack/data_source_openstack_compute_flavor_v2.go), adapted for the
// terraform-plugin-framework and PCD.

package compute

import (
	"context"
	"fmt"

	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/flavors"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/platform9/terraform-provider-pcd/internal/clients"
)

var (
	_ datasource.DataSource              = (*flavorDataSource)(nil)
	_ datasource.DataSourceWithConfigure = (*flavorDataSource)(nil)
)

// NewFlavorDataSource is the factory registered with the provider.
func NewFlavorDataSource() datasource.DataSource {
	return &flavorDataSource{}
}

type flavorDataSource struct {
	config *clients.Config
}

type flavorDataSourceModel struct {
	ID         types.String  `tfsdk:"id"`
	FlavorID   types.String  `tfsdk:"flavor_id"`
	Name       types.String  `tfsdk:"name"`
	RAM        types.Int64   `tfsdk:"ram"`
	VCPUs      types.Int64   `tfsdk:"vcpus"`
	Disk       types.Int64   `tfsdk:"disk"`
	Swap       types.Int64   `tfsdk:"swap"`
	RxTxFactor types.Float64 `tfsdk:"rx_tx_factor"`
	IsPublic   types.Bool    `tfsdk:"is_public"`
	Region     types.String  `tfsdk:"region"`
}

func (d *flavorDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_compute_flavor"
}

func (d *flavorDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Look up a compute flavor by name or ID.",
		Attributes: map[string]schema.Attribute{
			"id":           schema.StringAttribute{Computed: true, MarkdownDescription: "The flavor ID."},
			"flavor_id":    schema.StringAttribute{Optional: true, MarkdownDescription: "Look up by flavor ID (takes precedence over name)."},
			"name":         schema.StringAttribute{Optional: true, MarkdownDescription: "Look up by exact name."},
			"ram":          schema.Int64Attribute{Computed: true, MarkdownDescription: "Memory in MB."},
			"vcpus":        schema.Int64Attribute{Computed: true, MarkdownDescription: "Number of vCPUs."},
			"disk":         schema.Int64Attribute{Computed: true, MarkdownDescription: "Root disk in GB."},
			"swap":         schema.Int64Attribute{Computed: true, MarkdownDescription: "Swap in MB."},
			"rx_tx_factor": schema.Float64Attribute{Computed: true, MarkdownDescription: "RX/TX factor."},
			"is_public":    schema.BoolAttribute{Computed: true, MarkdownDescription: "Whether the flavor is public."},
			"region":       schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "The region. Defaults to the provider's region."},
		},
	}
}

func (d *flavorDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	d.config = configureClient(req.ProviderData, &resp.Diagnostics)
}

func (d *flavorDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var data flavorDataSourceModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := d.config.ComputeV2Client()
	if err != nil {
		resp.Diagnostics.AddError("compute: building v2 client", err.Error())
		return
	}

	var flavor *flavors.Flavor
	if v := data.FlavorID.ValueString(); v != "" {
		flavor, err = flavors.Get(ctx, client, v).Extract()
		if err != nil {
			resp.Diagnostics.AddError("compute: getting flavor by id", err.Error())
			return
		}
	} else {
		pages, err := flavors.ListDetail(client, flavors.ListOpts{}).AllPages(ctx)
		if err != nil {
			resp.Diagnostics.AddError("compute: listing flavors", err.Error())
			return
		}
		all, err := flavors.ExtractFlavors(pages)
		if err != nil {
			resp.Diagnostics.AddError("compute: extracting flavors", err.Error())
			return
		}
		name := data.Name.ValueString()
		var matches []flavors.Flavor
		for _, f := range all {
			if f.Name == name {
				matches = append(matches, f)
			}
		}
		switch len(matches) {
		case 0:
			resp.Diagnostics.AddError("No flavor found", fmt.Sprintf("No flavor named %q.", name))
			return
		case 1:
			flavor = &matches[0]
		default:
			resp.Diagnostics.AddError("Multiple flavors found", fmt.Sprintf("%d flavors named %q.", len(matches), name))
			return
		}
	}

	data.ID = types.StringValue(flavor.ID)
	data.FlavorID = types.StringValue(flavor.ID)
	data.Name = types.StringValue(flavor.Name)
	data.RAM = types.Int64Value(int64(flavor.RAM))
	data.VCPUs = types.Int64Value(int64(flavor.VCPUs))
	data.Disk = types.Int64Value(int64(flavor.Disk))
	data.Swap = types.Int64Value(int64(flavor.Swap))
	data.RxTxFactor = types.Float64Value(flavor.RxTxFactor)
	data.IsPublic = types.BoolValue(flavor.IsPublic)
	if data.Region.IsNull() || data.Region.IsUnknown() {
		data.Region = types.StringValue(d.config.Region)
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}
