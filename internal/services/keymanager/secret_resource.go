// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0
//
// Ported from terraform-provider-openstack v3.4.0
// (openstack/resource_openstack_keymanager_secret_v1.go), adapted for the
// terraform-plugin-framework and PCD.

package keymanager

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/keymanager/v1/secrets"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/int64planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/mapplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/platform9/terraform-provider-pcd/internal/clients"
)

var (
	_ resource.Resource                = (*secretResource)(nil)
	_ resource.ResourceWithConfigure   = (*secretResource)(nil)
	_ resource.ResourceWithImportState = (*secretResource)(nil)
)

// NewSecretResource is the factory registered with the provider.
func NewSecretResource() resource.Resource {
	return &secretResource{}
}

type secretResource struct {
	config *clients.Config
}

type secretModel struct {
	ID                     types.String `tfsdk:"id"`
	Name                   types.String `tfsdk:"name"`
	Algorithm              types.String `tfsdk:"algorithm"`
	BitLength              types.Int64  `tfsdk:"bit_length"`
	Mode                   types.String `tfsdk:"mode"`
	SecretType             types.String `tfsdk:"secret_type"`
	Expiration             types.String `tfsdk:"expiration"`
	Payload                types.String `tfsdk:"payload"`
	PayloadContentType     types.String `tfsdk:"payload_content_type"`
	PayloadContentEncoding types.String `tfsdk:"payload_content_encoding"`
	SecretRef              types.String `tfsdk:"secret_ref"`
	Status                 types.String `tfsdk:"status"`
	CreatorID              types.String `tfsdk:"creator_id"`
	ContentTypes           types.Map    `tfsdk:"content_types"`
	CreatedAt              types.String `tfsdk:"created_at"`
	UpdatedAt              types.String `tfsdk:"updated_at"`
	Region                 types.String `tfsdk:"region"`
}

func (r *secretResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_keymanager_secret"
}

func (r *secretResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	useState := []planmodifier.String{stringplanmodifier.UseStateForUnknown()}
	// ForceNew inputs are Optional+Computed so an omitted value is populated from
	// the server (and captured on import) without tripping apply-consistency.
	forceNewC := []planmodifier.String{stringplanmodifier.RequiresReplace(), stringplanmodifier.UseStateForUnknown()}
	forceNew := []planmodifier.String{stringplanmodifier.RequiresReplace()}
	resp.Schema = schema.Schema{
		MarkdownDescription: "Manages a secret in PCD's Barbican key manager. Secrets are immutable — any change " +
			"forces a new resource.",
		Attributes: map[string]schema.Attribute{
			"id":                       schema.StringAttribute{Computed: true, MarkdownDescription: "The secret UUID.", PlanModifiers: useState},
			"name":                     schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "The name of the secret. Changing this forces a new resource.", PlanModifiers: forceNewC},
			"algorithm":                schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "The encryption algorithm (e.g. aes). Changing this forces a new resource.", PlanModifiers: forceNewC},
			"bit_length":               schema.Int64Attribute{Optional: true, Computed: true, MarkdownDescription: "The bit length of the secret. Changing this forces a new resource.", PlanModifiers: []planmodifier.Int64{int64planmodifier.RequiresReplace(), int64planmodifier.UseStateForUnknown()}},
			"mode":                     schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "The cipher mode (e.g. cbc). Changing this forces a new resource.", PlanModifiers: forceNewC},
			"secret_type":              schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "The secret type: symmetric, public, private, passphrase, certificate, or opaque (default). Changing this forces a new resource.", PlanModifiers: forceNewC},
			"expiration":               schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "Expiration timestamp (RFC3339). Changing this forces a new resource.", PlanModifiers: forceNewC},
			"payload":                  schema.StringAttribute{Optional: true, Sensitive: true, MarkdownDescription: "The secret data. Write-only: never read back from the server. Changing this forces a new resource.", PlanModifiers: forceNew},
			"payload_content_type":     schema.StringAttribute{Optional: true, MarkdownDescription: "The payload content type (required when payload is set, e.g. text/plain). Changing this forces a new resource.", PlanModifiers: forceNew},
			"payload_content_encoding": schema.StringAttribute{Optional: true, MarkdownDescription: "The payload content encoding (e.g. base64 for binary payloads). Changing this forces a new resource.", PlanModifiers: forceNew},
			"secret_ref":               schema.StringAttribute{Computed: true, MarkdownDescription: "The full Barbican secret reference URL.", PlanModifiers: useState},
			"status":                   schema.StringAttribute{Computed: true, MarkdownDescription: "The secret status (e.g. ACTIVE).", PlanModifiers: useState},
			"creator_id":               schema.StringAttribute{Computed: true, MarkdownDescription: "The ID of the user that created the secret.", PlanModifiers: useState},
			"content_types":            schema.MapAttribute{Computed: true, ElementType: types.StringType, MarkdownDescription: "The content types the secret is available in.", PlanModifiers: []planmodifier.Map{mapplanmodifier.UseStateForUnknown()}},
			"created_at":               schema.StringAttribute{Computed: true, MarkdownDescription: "Creation timestamp (RFC3339).", PlanModifiers: useState},
			"updated_at":               schema.StringAttribute{Computed: true, MarkdownDescription: "Last-update timestamp (RFC3339).", PlanModifiers: useState},
			"region":                   schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "The region. Defaults to the provider's region.", PlanModifiers: useState},
		},
	}
}

func (r *secretResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.config = configureClient(req.ProviderData, &resp.Diagnostics)
}

func (r *secretResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan secretModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	payloadSet := !plan.Payload.IsNull() && plan.Payload.ValueString() != ""
	if payloadSet && plan.PayloadContentType.ValueString() == "" {
		resp.Diagnostics.AddError("Invalid secret", "payload_content_type is required when payload is set.")
		return
	}

	client, err := r.config.KeyManagerV1Client()
	if err != nil {
		resp.Diagnostics.AddError("keymanager: building v1 client", err.Error())
		return
	}

	createOpts := secrets.CreateOpts{
		Name:                   plan.Name.ValueString(),
		Algorithm:              plan.Algorithm.ValueString(),
		Mode:                   plan.Mode.ValueString(),
		Payload:                plan.Payload.ValueString(),
		PayloadContentType:     plan.PayloadContentType.ValueString(),
		PayloadContentEncoding: plan.PayloadContentEncoding.ValueString(),
	}
	if v := plan.SecretType.ValueString(); v != "" {
		createOpts.SecretType = secrets.SecretType(v)
	}
	if plan.BitLength.ValueInt64() > 0 {
		createOpts.BitLength = int(plan.BitLength.ValueInt64())
	}
	if v := plan.Expiration.ValueString(); v != "" {
		exp, perr := time.Parse(time.RFC3339, v)
		if perr != nil {
			resp.Diagnostics.AddError("Invalid expiration", fmt.Sprintf("expiration must be RFC3339: %s", perr))
			return
		}
		createOpts.Expiration = &exp
	}

	secret, err := secrets.Create(ctx, client, createOpts).Extract()
	if err != nil {
		resp.Diagnostics.AddError("keymanager: creating secret", err.Error())
		return
	}
	id := refToID(secret.SecretRef)

	// A secret created with a payload is briefly PENDING; wait for ACTIVE. A
	// secret created without a payload stays PENDING, so do not wait in that case.
	if payloadSet {
		if err := waitForSecretActive(ctx, client, id, defaultKeyManagerTimeout); err != nil {
			resp.Diagnostics.AddError("keymanager: waiting for secret to become active", err.Error())
			return
		}
	}

	_, readDiags := r.readInto(ctx, client, id, &plan)
	resp.Diagnostics.Append(readDiags...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *secretResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state secretModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := r.config.KeyManagerV1Client()
	if err != nil {
		resp.Diagnostics.AddError("keymanager: building v1 client", err.Error())
		return
	}

	notFound, diags := r.readInto(ctx, client, state.ID.ValueString(), &state)
	if notFound {
		resp.Diagnostics.AddWarning("Secret not found",
			fmt.Sprintf("Secret %s no longer exists and was removed from state.", state.ID.ValueString()))
		resp.State.RemoveResource(ctx)
		return
	}
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

// Update is required by the interface but never invoked (every attribute forces replacement).
func (r *secretResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan secretModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *secretResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state secretModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := r.config.KeyManagerV1Client()
	if err != nil {
		resp.Diagnostics.AddError("keymanager: building v1 client", err.Error())
		return
	}

	if err := secrets.Delete(ctx, client, state.ID.ValueString()).ExtractErr(); err != nil {
		if gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
			return
		}
		resp.Diagnostics.AddError("keymanager: deleting secret", err.Error())
	}
}

func (r *secretResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, path.Root("id"), req, resp)
}

// readInto refreshes the server-managed secret attributes. The write-only payload
// fields are never read back (Barbican does not return them), and the ForceNew
// inputs are populated from the server only when unset (import/omitted) so a
// configured value round-trips exactly.
func (r *secretResource) readInto(ctx context.Context, client *gophercloud.ServiceClient, id string, m *secretModel) (notFound bool, diags diag.Diagnostics) {
	secret, err := secrets.Get(ctx, client, id).Extract()
	if err != nil {
		if gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
			return true, diags
		}
		diags.AddError("keymanager: reading secret", err.Error())
		return false, diags
	}

	m.ID = types.StringValue(id)
	m.SecretRef = types.StringValue(secret.SecretRef)
	m.Status = types.StringValue(secret.Status)
	m.CreatorID = types.StringValue(secret.CreatorID)
	m.CreatedAt = types.StringValue(formatTime(secret.Created))
	m.UpdatedAt = types.StringValue(formatTime(secret.Updated))

	if unset(m.Name) {
		m.Name = types.StringValue(secret.Name)
	}
	if unset(m.Algorithm) {
		m.Algorithm = types.StringValue(secret.Algorithm)
	}
	if unset(m.Mode) {
		m.Mode = types.StringValue(secret.Mode)
	}
	if unset(m.SecretType) {
		m.SecretType = types.StringValue(secret.SecretType)
	}
	if m.BitLength.IsNull() || m.BitLength.IsUnknown() {
		m.BitLength = types.Int64Value(int64(secret.BitLength))
	}
	if unset(m.Expiration) {
		m.Expiration = types.StringValue(formatTime(secret.Expiration))
	}

	ct := secret.ContentTypes
	if ct == nil {
		ct = map[string]string{}
	}
	contentTypes, d := types.MapValueFrom(ctx, types.StringType, ct)
	diags = append(diags, d...)
	m.ContentTypes = contentTypes

	if m.Region.IsNull() || m.Region.IsUnknown() {
		m.Region = types.StringValue(r.config.Region)
	}
	return false, diags
}

// unset reports whether a string attribute is null or unknown (needs populating
// from the server rather than echoing a configured value).
func unset(v types.String) bool {
	return v.IsNull() || v.IsUnknown()
}

// formatTime renders a timestamp as RFC3339, or "" if zero.
func formatTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format(time.RFC3339)
}
