// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0
//
// Ported from terraform-provider-openstack v3.4.0
// (openstack/data_source_openstack_dns_zone_v2.go), adapted for the
// terraform-plugin-framework and PCD.

package dns

import (
	"context"
	"fmt"

	"github.com/gophercloud/gophercloud/v2/openstack/dns/v2/zones"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/platform9/terraform-provider-pcd/internal/clients"
)

var (
	_ datasource.DataSource              = (*zoneDataSource)(nil)
	_ datasource.DataSourceWithConfigure = (*zoneDataSource)(nil)
)

// NewZoneDataSource is the factory registered with the provider.
func NewZoneDataSource() datasource.DataSource {
	return &zoneDataSource{}
}

type zoneDataSource struct {
	config *clients.Config
}

type zoneDataSourceModel struct {
	ID          types.String `tfsdk:"id"`
	ZoneID      types.String `tfsdk:"zone_id"`
	Name        types.String `tfsdk:"name"`
	Email       types.String `tfsdk:"email"`
	TTL         types.Int64  `tfsdk:"ttl"`
	Serial      types.Int64  `tfsdk:"serial"`
	Status      types.String `tfsdk:"status"`
	Type        types.String `tfsdk:"type"`
	Description types.String `tfsdk:"description"`
	PoolID      types.String `tfsdk:"pool_id"`
	Region      types.String `tfsdk:"region"`
}

func (d *zoneDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_dns_zone"
}

func (d *zoneDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Look up a DNS zone by ID or name. Exactly one zone must match.",
		Attributes: map[string]schema.Attribute{
			"id":          schema.StringAttribute{Computed: true, MarkdownDescription: "The zone ID."},
			"zone_id":     schema.StringAttribute{Optional: true, MarkdownDescription: "Look up by zone ID (takes precedence over name)."},
			"name":        schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "Look up by (and report) the zone name."},
			"email":       schema.StringAttribute{Computed: true, MarkdownDescription: "The SOA contact email."},
			"ttl":         schema.Int64Attribute{Computed: true, MarkdownDescription: "The zone TTL."},
			"serial":      schema.Int64Attribute{Computed: true, MarkdownDescription: "The current SOA serial."},
			"status":      schema.StringAttribute{Computed: true, MarkdownDescription: "The zone status (e.g. ACTIVE)."},
			"type":        schema.StringAttribute{Computed: true, MarkdownDescription: "The zone type (PRIMARY or SECONDARY)."},
			"description": schema.StringAttribute{Computed: true, MarkdownDescription: "The zone description."},
			"pool_id":     schema.StringAttribute{Computed: true, MarkdownDescription: "The pool hosting the zone."},
			"region":      schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "The region. Defaults to the provider's region."},
		},
	}
}

func (d *zoneDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	d.config = configureClient(req.ProviderData, &resp.Diagnostics)
}

func (d *zoneDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var data zoneDataSourceModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := d.config.DNSV2Client()
	if err != nil {
		resp.Diagnostics.AddError("dns: building v2 client", err.Error())
		return
	}

	var zone *zones.Zone
	switch {
	case data.ZoneID.ValueString() != "":
		zone, err = zones.Get(ctx, client, data.ZoneID.ValueString()).Extract()
		if err != nil {
			resp.Diagnostics.AddError("dns: getting zone", err.Error())
			return
		}
	case data.Name.ValueString() != "":
		pages, err := zones.List(client, zones.ListOpts{Name: data.Name.ValueString()}).AllPages(ctx)
		if err != nil {
			resp.Diagnostics.AddError("dns: listing zones", err.Error())
			return
		}
		all, err := zones.ExtractZones(pages)
		if err != nil {
			resp.Diagnostics.AddError("dns: extracting zones", err.Error())
			return
		}
		switch len(all) {
		case 0:
			resp.Diagnostics.AddError("No zone found", "No zone matched the given name.")
			return
		case 1:
			zone = &all[0]
		default:
			resp.Diagnostics.AddError("Multiple zones found",
				fmt.Sprintf("%d zones matched; refine the criteria.", len(all)))
			return
		}
	default:
		resp.Diagnostics.AddError("Missing lookup key", "Set one of zone_id or name.")
		return
	}

	data.ID = types.StringValue(zone.ID)
	data.ZoneID = types.StringValue(zone.ID)
	data.Name = types.StringValue(zone.Name)
	data.Email = types.StringValue(zone.Email)
	data.TTL = types.Int64Value(int64(zone.TTL))
	data.Serial = types.Int64Value(int64(zone.Serial))
	data.Status = types.StringValue(zone.Status)
	data.Type = types.StringValue(zone.Type)
	data.Description = types.StringValue(zone.Description)
	data.PoolID = types.StringValue(zone.PoolID)
	if data.Region.IsNull() || data.Region.IsUnknown() {
		data.Region = types.StringValue(d.config.Region)
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}
