// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0
//
// Ported from terraform-provider-openstack v3.4.0
// (openstack/data_source_openstack_compute_keypair_v2.go), adapted for the
// terraform-plugin-framework and PCD.

package compute

import (
	"context"

	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/keypairs"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/platform9/terraform-provider-pcd/internal/clients"
)

var (
	_ datasource.DataSource              = (*keypairDataSource)(nil)
	_ datasource.DataSourceWithConfigure = (*keypairDataSource)(nil)
)

// NewKeypairDataSource is the factory registered with the provider.
func NewKeypairDataSource() datasource.DataSource {
	return &keypairDataSource{}
}

type keypairDataSource struct {
	config *clients.Config
}

type keypairDataSourceModel struct {
	ID          types.String `tfsdk:"id"`
	Name        types.String `tfsdk:"name"`
	PublicKey   types.String `tfsdk:"public_key"`
	Fingerprint types.String `tfsdk:"fingerprint"`
	UserID      types.String `tfsdk:"user_id"`
	Region      types.String `tfsdk:"region"`
}

func (d *keypairDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_compute_keypair"
}

func (d *keypairDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Look up an SSH keypair by name.",
		Attributes: map[string]schema.Attribute{
			"id":          schema.StringAttribute{Computed: true, MarkdownDescription: "Mirrors name."},
			"name":        schema.StringAttribute{Required: true, MarkdownDescription: "The keypair name."},
			"public_key":  schema.StringAttribute{Computed: true, MarkdownDescription: "The public key."},
			"fingerprint": schema.StringAttribute{Computed: true, MarkdownDescription: "The keypair fingerprint."},
			"user_id":     schema.StringAttribute{Computed: true, MarkdownDescription: "The owning user."},
			"region":      schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "The region. Defaults to the provider's region."},
		},
	}
}

func (d *keypairDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	d.config = configureClient(req.ProviderData, &resp.Diagnostics)
}

func (d *keypairDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var data keypairDataSourceModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := d.config.ComputeV2Client()
	if err != nil {
		resp.Diagnostics.AddError("compute: building v2 client", err.Error())
		return
	}

	kp, err := keypairs.Get(ctx, client, data.Name.ValueString(), keypairs.GetOpts{}).Extract()
	if err != nil {
		resp.Diagnostics.AddError("compute: getting keypair", err.Error())
		return
	}

	data.ID = types.StringValue(kp.Name)
	data.PublicKey = types.StringValue(kp.PublicKey)
	data.Fingerprint = types.StringValue(kp.Fingerprint)
	data.UserID = types.StringValue(kp.UserID)
	if data.Region.IsNull() || data.Region.IsUnknown() {
		data.Region = types.StringValue(d.config.Region)
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}
