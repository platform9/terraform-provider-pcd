// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0
//
// Ported from terraform-provider-openstack v3.4.0
// (openstack/resource_openstack_vpnaas_ike_policy_v2.go), adapted for the
// terraform-plugin-framework and PCD.

package vpnaas

import (
	"context"
	"fmt"
	"net/http"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/extensions/vpnaas/ikepolicies"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/int64planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/objectplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringdefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/platform9/terraform-provider-pcd/internal/clients"
)

var (
	_ resource.Resource                = (*ikePolicyResource)(nil)
	_ resource.ResourceWithConfigure   = (*ikePolicyResource)(nil)
	_ resource.ResourceWithImportState = (*ikePolicyResource)(nil)
)

// NewIKEPolicyResource is the factory registered with the provider.
func NewIKEPolicyResource() resource.Resource {
	return &ikePolicyResource{}
}

type ikePolicyResource struct {
	config *clients.Config
}

type ikePolicyModel struct {
	ID                    types.String `tfsdk:"id"`
	Region                types.String `tfsdk:"region"`
	Name                  types.String `tfsdk:"name"`
	Description           types.String `tfsdk:"description"`
	AuthAlgorithm         types.String `tfsdk:"auth_algorithm"`
	EncryptionAlgorithm   types.String `tfsdk:"encryption_algorithm"`
	PFS                   types.String `tfsdk:"pfs"`
	Phase1NegotiationMode types.String `tfsdk:"phase1_negotiation_mode"`
	IKEVersion            types.String `tfsdk:"ike_version"`
	Lifetime              types.Object `tfsdk:"lifetime"`
	TenantID              types.String `tfsdk:"tenant_id"`
}

func (r *ikePolicyResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_vpnaas_ike_policy"
}

// lifetimeSchema is the shared {units, value} nested attribute for the IKE and
// IPsec policy resources.
func lifetimeSchema() schema.SingleNestedAttribute {
	return schema.SingleNestedAttribute{
		Optional:            true,
		Computed:            true,
		MarkdownDescription: "The lifetime of the security association.",
		PlanModifiers:       []planmodifier.Object{objectplanmodifier.UseStateForUnknown()},
		Attributes: map[string]schema.Attribute{
			"units": schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "The lifetime units: `seconds` (default) or `kilobytes`.", PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()}},
			"value": schema.Int64Attribute{Optional: true, Computed: true, MarkdownDescription: "The lifetime value (default 3600).", PlanModifiers: []planmodifier.Int64{int64planmodifier.UseStateForUnknown()}},
		},
	}
}

func (r *ikePolicyResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	useState := []planmodifier.String{stringplanmodifier.UseStateForUnknown()}
	resp.Schema = schema.Schema{
		MarkdownDescription: "Manages a Neutron IKE policy for VPNaaS in PCD.",
		Attributes: map[string]schema.Attribute{
			"id":                      schema.StringAttribute{Computed: true, MarkdownDescription: "The IKE policy ID.", PlanModifiers: useState},
			"region":                  schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "The region. Defaults to the provider's region.", PlanModifiers: useState},
			"name":                    schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "The name of the policy.", PlanModifiers: useState},
			"description":             schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "A description of the policy.", PlanModifiers: useState},
			"auth_algorithm":          schema.StringAttribute{Optional: true, Computed: true, Default: stringdefault.StaticString("sha256"), MarkdownDescription: "The authentication hash algorithm (sha1, sha256, sha384, sha512)."},
			"encryption_algorithm":    schema.StringAttribute{Optional: true, Computed: true, Default: stringdefault.StaticString("aes-128"), MarkdownDescription: "The encryption algorithm (e.g. 3des, aes-128, aes-192, aes-256)."},
			"pfs":                     schema.StringAttribute{Optional: true, Computed: true, Default: stringdefault.StaticString("group5"), MarkdownDescription: "The perfect forward secrecy mode (group2, group5, group14, ...)."},
			"phase1_negotiation_mode": schema.StringAttribute{Optional: true, Computed: true, Default: stringdefault.StaticString("main"), MarkdownDescription: "The IKE phase-1 negotiation mode (only `main`)."},
			"ike_version":             schema.StringAttribute{Optional: true, Computed: true, Default: stringdefault.StaticString("v1"), MarkdownDescription: "The IKE version (v1 or v2)."},
			"lifetime":                lifetimeSchema(),
			"tenant_id":               schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "The owning project. Changing this forces a new resource.", PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace(), stringplanmodifier.UseStateForUnknown()}},
		},
	}
}

func (r *ikePolicyResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.config = configureClient(req.ProviderData, &resp.Diagnostics)
}

func (r *ikePolicyResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan ikePolicyModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := r.config.NetworkV2Client()
	if err != nil {
		resp.Diagnostics.AddError("vpnaas: building network client", err.Error())
		return
	}

	opts := ikepolicies.CreateOpts{
		Name:                  plan.Name.ValueString(),
		Description:           plan.Description.ValueString(),
		TenantID:              plan.TenantID.ValueString(),
		AuthAlgorithm:         ikepolicies.AuthAlgorithm(plan.AuthAlgorithm.ValueString()),
		EncryptionAlgorithm:   ikepolicies.EncryptionAlgorithm(plan.EncryptionAlgorithm.ValueString()),
		PFS:                   ikepolicies.PFS(plan.PFS.ValueString()),
		Phase1NegotiationMode: ikepolicies.Phase1NegotiationMode(plan.Phase1NegotiationMode.ValueString()),
		IKEVersion:            ikepolicies.IKEVersion(plan.IKEVersion.ValueString()),
	}
	if units, value, ok := lifetimeFromObject(ctx, plan.Lifetime, &resp.Diagnostics); ok {
		opts.Lifetime = &ikepolicies.LifetimeCreateOpts{Units: ikepolicies.Unit(units), Value: value}
	}
	if resp.Diagnostics.HasError() {
		return
	}

	policy, err := ikepolicies.Create(ctx, client, opts).Extract()
	if err != nil {
		resp.Diagnostics.AddError("vpnaas: creating IKE policy", err.Error())
		return
	}

	resp.Diagnostics.Append(r.readInto(ctx, client, policy.ID, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *ikePolicyResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state ikePolicyModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := r.config.NetworkV2Client()
	if err != nil {
		resp.Diagnostics.AddError("vpnaas: building network client", err.Error())
		return
	}

	notFound, diags := r.get(ctx, client, state.ID.ValueString(), &state)
	if notFound {
		resp.State.RemoveResource(ctx)
		return
	}
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *ikePolicyResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state ikePolicyModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := r.config.NetworkV2Client()
	if err != nil {
		resp.Diagnostics.AddError("vpnaas: building network client", err.Error())
		return
	}

	opts := ikepolicies.UpdateOpts{}
	if !plan.Name.Equal(state.Name) {
		v := plan.Name.ValueString()
		opts.Name = &v
	}
	if !plan.Description.Equal(state.Description) {
		v := plan.Description.ValueString()
		opts.Description = &v
	}
	if !plan.AuthAlgorithm.Equal(state.AuthAlgorithm) {
		opts.AuthAlgorithm = ikepolicies.AuthAlgorithm(plan.AuthAlgorithm.ValueString())
	}
	if !plan.EncryptionAlgorithm.Equal(state.EncryptionAlgorithm) {
		opts.EncryptionAlgorithm = ikepolicies.EncryptionAlgorithm(plan.EncryptionAlgorithm.ValueString())
	}
	if !plan.PFS.Equal(state.PFS) {
		opts.PFS = ikepolicies.PFS(plan.PFS.ValueString())
	}
	if !plan.Phase1NegotiationMode.Equal(state.Phase1NegotiationMode) {
		opts.Phase1NegotiationMode = ikepolicies.Phase1NegotiationMode(plan.Phase1NegotiationMode.ValueString())
	}
	if !plan.IKEVersion.Equal(state.IKEVersion) {
		opts.IKEVersion = ikepolicies.IKEVersion(plan.IKEVersion.ValueString())
	}
	if !plan.Lifetime.Equal(state.Lifetime) {
		if units, value, ok := lifetimeFromObject(ctx, plan.Lifetime, &resp.Diagnostics); ok {
			opts.Lifetime = &ikepolicies.LifetimeUpdateOpts{Units: ikepolicies.Unit(units), Value: value}
		}
	}
	if resp.Diagnostics.HasError() {
		return
	}

	id := plan.ID.ValueString()
	if _, err := ikepolicies.Update(ctx, client, id, opts).Extract(); err != nil {
		resp.Diagnostics.AddError("vpnaas: updating IKE policy", err.Error())
		return
	}

	resp.Diagnostics.Append(r.readInto(ctx, client, id, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *ikePolicyResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state ikePolicyModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := r.config.NetworkV2Client()
	if err != nil {
		resp.Diagnostics.AddError("vpnaas: building network client", err.Error())
		return
	}

	if err := ikepolicies.Delete(ctx, client, state.ID.ValueString()).ExtractErr(); err != nil {
		if gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
			return
		}
		resp.Diagnostics.AddError("vpnaas: deleting IKE policy", err.Error())
	}
}

func (r *ikePolicyResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, path.Root("id"), req, resp)
}

func (r *ikePolicyResource) readInto(ctx context.Context, client *gophercloud.ServiceClient, id string, m *ikePolicyModel) diag.Diagnostics {
	notFound, diags := r.get(ctx, client, id, m)
	if notFound {
		diags.AddError("vpnaas: IKE policy not found after write",
			fmt.Sprintf("IKE policy %s was not found immediately after a create/update.", id))
	}
	return diags
}

func (r *ikePolicyResource) get(ctx context.Context, client *gophercloud.ServiceClient, id string, m *ikePolicyModel) (notFound bool, diags diag.Diagnostics) {
	policy, err := ikepolicies.Get(ctx, client, id).Extract()
	if err != nil {
		if gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
			return true, diags
		}
		diags.AddError("vpnaas: reading IKE policy", err.Error())
		return false, diags
	}

	m.ID = types.StringValue(policy.ID)
	m.Name = types.StringValue(policy.Name)
	m.Description = types.StringValue(policy.Description)
	m.AuthAlgorithm = types.StringValue(policy.AuthAlgorithm)
	m.EncryptionAlgorithm = types.StringValue(policy.EncryptionAlgorithm)
	m.PFS = types.StringValue(policy.PFS)
	m.Phase1NegotiationMode = types.StringValue(policy.Phase1NegotiationMode)
	m.IKEVersion = types.StringValue(policy.IKEVersion)
	m.TenantID = types.StringValue(policy.TenantID)
	m.Lifetime = flattenLifetime(policy.Lifetime.Units, policy.Lifetime.Value)
	if m.Region.IsNull() || m.Region.IsUnknown() {
		m.Region = types.StringValue(r.config.Region)
	}
	return false, diags
}
