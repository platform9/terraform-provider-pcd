// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0
//
// Ported from terraform-provider-openstack v3.4.0
// (openstack/data_source_openstack_keymanager_secret_v1.go), adapted for the
// terraform-plugin-framework and PCD.

package keymanager

import (
	"context"
	"fmt"

	"github.com/gophercloud/gophercloud/v2/openstack/keymanager/v1/secrets"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/platform9/terraform-provider-pcd/internal/clients"
)

var (
	_ datasource.DataSource              = (*secretDataSource)(nil)
	_ datasource.DataSourceWithConfigure = (*secretDataSource)(nil)
)

// NewSecretDataSource is the factory registered with the provider.
func NewSecretDataSource() datasource.DataSource {
	return &secretDataSource{}
}

type secretDataSource struct {
	config *clients.Config
}

type secretDataSourceModel struct {
	ID                 types.String `tfsdk:"id"`
	SecretRef          types.String `tfsdk:"secret_ref"`
	Name               types.String `tfsdk:"name"`
	Status             types.String `tfsdk:"status"`
	SecretType         types.String `tfsdk:"secret_type"`
	Algorithm          types.String `tfsdk:"algorithm"`
	BitLength          types.Int64  `tfsdk:"bit_length"`
	Mode               types.String `tfsdk:"mode"`
	Expiration         types.String `tfsdk:"expiration"`
	CreatedAt          types.String `tfsdk:"created_at"`
	CreatorID          types.String `tfsdk:"creator_id"`
	ContentTypes       types.Map    `tfsdk:"content_types"`
	PayloadContentType types.String `tfsdk:"payload_content_type"`
	Payload            types.String `tfsdk:"payload"`
	Region             types.String `tfsdk:"region"`
}

func (d *secretDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_keymanager_secret"
}

func (d *secretDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Look up a Barbican secret by name or reference. Exactly one secret must match. The " +
			"secret's `payload` is fetched only when `payload_content_type` is set.",
		Attributes: map[string]schema.Attribute{
			"id":                   schema.StringAttribute{Computed: true, MarkdownDescription: "The secret UUID."},
			"secret_ref":           schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "Look up by secret reference (URL or UUID); takes precedence over name."},
			"name":                 schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "Look up by (and report) the secret name."},
			"status":               schema.StringAttribute{Computed: true, MarkdownDescription: "The secret status."},
			"secret_type":          schema.StringAttribute{Computed: true, MarkdownDescription: "The secret type."},
			"algorithm":            schema.StringAttribute{Computed: true, MarkdownDescription: "The encryption algorithm."},
			"bit_length":           schema.Int64Attribute{Computed: true, MarkdownDescription: "The bit length."},
			"mode":                 schema.StringAttribute{Computed: true, MarkdownDescription: "The cipher mode."},
			"expiration":           schema.StringAttribute{Computed: true, MarkdownDescription: "The expiration timestamp (RFC3339)."},
			"created_at":           schema.StringAttribute{Computed: true, MarkdownDescription: "The creation timestamp (RFC3339)."},
			"creator_id":           schema.StringAttribute{Computed: true, MarkdownDescription: "The ID of the user that created the secret."},
			"content_types":        schema.MapAttribute{Computed: true, ElementType: types.StringType, MarkdownDescription: "The content types the secret is available in."},
			"payload_content_type": schema.StringAttribute{Optional: true, MarkdownDescription: "Set to fetch the payload with this content type (Accept header)."},
			"payload":              schema.StringAttribute{Computed: true, Sensitive: true, MarkdownDescription: "The secret payload, populated only when payload_content_type is set."},
			"region":               schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "The region. Defaults to the provider's region."},
		},
	}
}

func (d *secretDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	d.config = configureClient(req.ProviderData, &resp.Diagnostics)
}

func (d *secretDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var data secretDataSourceModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := d.config.KeyManagerV1Client()
	if err != nil {
		resp.Diagnostics.AddError("keymanager: building v1 client", err.Error())
		return
	}

	var secret *secrets.Secret
	switch {
	case data.SecretRef.ValueString() != "":
		secret, err = secrets.Get(ctx, client, refToID(data.SecretRef.ValueString())).Extract()
		if err != nil {
			resp.Diagnostics.AddError("keymanager: getting secret", err.Error())
			return
		}
	case data.Name.ValueString() != "":
		pages, err := secrets.List(client, secrets.ListOpts{Name: data.Name.ValueString()}).AllPages(ctx)
		if err != nil {
			resp.Diagnostics.AddError("keymanager: listing secrets", err.Error())
			return
		}
		all, err := secrets.ExtractSecrets(pages)
		if err != nil {
			resp.Diagnostics.AddError("keymanager: extracting secrets", err.Error())
			return
		}
		switch len(all) {
		case 0:
			resp.Diagnostics.AddError("No secret found", "No secret matched the given name.")
			return
		case 1:
			secret = &all[0]
		default:
			resp.Diagnostics.AddError("Multiple secrets found",
				fmt.Sprintf("%d secrets matched; refine the criteria.", len(all)))
			return
		}
	default:
		resp.Diagnostics.AddError("Missing lookup key", "Set one of secret_ref or name.")
		return
	}

	id := refToID(secret.SecretRef)
	data.ID = types.StringValue(id)
	data.SecretRef = types.StringValue(secret.SecretRef)
	data.Name = types.StringValue(secret.Name)
	data.Status = types.StringValue(secret.Status)
	data.SecretType = types.StringValue(secret.SecretType)
	data.Algorithm = types.StringValue(secret.Algorithm)
	data.BitLength = types.Int64Value(int64(secret.BitLength))
	data.Mode = types.StringValue(secret.Mode)
	data.Expiration = types.StringValue(formatTime(secret.Expiration))
	data.CreatedAt = types.StringValue(formatTime(secret.Created))
	data.CreatorID = types.StringValue(secret.CreatorID)

	ct := secret.ContentTypes
	if ct == nil {
		ct = map[string]string{}
	}
	contentTypes, diags := types.MapValueFrom(ctx, types.StringType, ct)
	resp.Diagnostics.Append(diags...)
	data.ContentTypes = contentTypes

	if pct := data.PayloadContentType.ValueString(); pct != "" {
		payload, err := secrets.GetPayload(ctx, client, id, secrets.GetPayloadOpts{PayloadContentType: pct}).Extract()
		if err != nil {
			resp.Diagnostics.AddError("keymanager: fetching secret payload", err.Error())
			return
		}
		data.Payload = types.StringValue(string(payload))
	} else {
		data.Payload = types.StringNull()
	}

	if data.Region.IsNull() || data.Region.IsUnknown() {
		data.Region = types.StringValue(d.config.Region)
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}
