// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0
//
// Ported from terraform-provider-openstack v3.4.0
// (openstack/data_source_openstack_compute_aggregate_v2.go), adapted for the
// terraform-plugin-framework and PCD.

package compute

import (
	"context"
	"fmt"
	"slices"

	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/aggregates"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/platform9/terraform-provider-pcd/internal/clients"
)

var (
	_ datasource.DataSource              = (*aggregateDataSource)(nil)
	_ datasource.DataSourceWithConfigure = (*aggregateDataSource)(nil)
)

// NewAggregateDataSource is the factory registered with the provider.
func NewAggregateDataSource() datasource.DataSource {
	return &aggregateDataSource{}
}

type aggregateDataSource struct {
	config *clients.Config
}

func (d *aggregateDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_compute_aggregate"
}

func (d *aggregateDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Looks up a Nova host aggregate by name, including the aggregates PCD keeps for its " +
			"clusters (named after the cluster). Requires an admin token.",
		Attributes: map[string]schema.Attribute{
			"name": schema.StringAttribute{Required: true, MarkdownDescription: "The aggregate name, matched exactly. Nova keeps names unique."},
			"id":   schema.StringAttribute{Computed: true, MarkdownDescription: "The Nova aggregate ID, a decimal integer."},
			"zone": schema.StringAttribute{Computed: true, MarkdownDescription: "The aggregate's availability zone; null when it has none."},
			"hosts": schema.SetAttribute{Computed: true, ElementType: types.StringType,
				MarkdownDescription: "The hypervisors in the aggregate, by resmgr host UUID."},
			"metadata": schema.MapAttribute{Computed: true, ElementType: types.StringType,
				MarkdownDescription: "The aggregate metadata, without the `availability_zone` key Nova mirrors from `zone`."},
			"region": schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "The region. Defaults to the provider's region."},
		},
	}
}

func (d *aggregateDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	d.config = configureClient(req.ProviderData, &resp.Diagnostics)
}

func (d *aggregateDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var data aggregateModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}
	client, err := d.config.ForRegion(data.Region.ValueString()).ComputeV2Client()
	if err != nil {
		resp.Diagnostics.AddError("compute: building v2 client", err.Error())
		return
	}
	pages, err := aggregates.List(client).AllPages(ctx)
	if err != nil {
		resp.Diagnostics.AddError("compute: listing aggregates", err.Error())
		return
	}
	all, err := aggregates.ExtractAggregates(pages)
	if err != nil {
		resp.Diagnostics.AddError("compute: extracting aggregates", err.Error())
		return
	}
	agg, err := aggregateByName(all, data.Name.ValueString())
	if err != nil {
		resp.Diagnostics.AddAttributeError(path.Root("name"), "Aggregate not found", err.Error())
		return
	}
	resp.Diagnostics.Append(flattenAggregate(ctx, agg, &data, d.config.Region)...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}

// aggregateByName returns the aggregate with exactly this name. Nova keeps
// aggregate names unique (AggregateNameExists), so the first match is the
// only one.
func aggregateByName(all []aggregates.Aggregate, name string) (*aggregates.Aggregate, error) {
	names := make([]string, 0, len(all))
	for i := range all {
		if all[i].Name == name {
			return &all[i], nil
		}
		names = append(names, all[i].Name)
	}
	slices.Sort(names)
	return nil, fmt.Errorf("no aggregate is named %q; the region has %q", name, names)
}
