// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0
//
// Ported from terraform-provider-openstack v3.4.0
// (openstack/resource_openstack_vpnaas_ipsec_policy_v2.go), adapted for the
// terraform-plugin-framework and PCD.

package vpnaas

import (
	"context"
	"fmt"
	"net/http"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/extensions/vpnaas/ipsecpolicies"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringdefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/platform9/terraform-provider-pcd/internal/clients"
)

var (
	_ resource.Resource                = (*ipsecPolicyResource)(nil)
	_ resource.ResourceWithConfigure   = (*ipsecPolicyResource)(nil)
	_ resource.ResourceWithImportState = (*ipsecPolicyResource)(nil)
)

// NewIPSecPolicyResource is the factory registered with the provider.
func NewIPSecPolicyResource() resource.Resource {
	return &ipsecPolicyResource{}
}

type ipsecPolicyResource struct {
	config *clients.Config
}

type ipsecPolicyModel struct {
	ID                  types.String `tfsdk:"id"`
	Region              types.String `tfsdk:"region"`
	Name                types.String `tfsdk:"name"`
	Description         types.String `tfsdk:"description"`
	AuthAlgorithm       types.String `tfsdk:"auth_algorithm"`
	EncapsulationMode   types.String `tfsdk:"encapsulation_mode"`
	EncryptionAlgorithm types.String `tfsdk:"encryption_algorithm"`
	PFS                 types.String `tfsdk:"pfs"`
	TransformProtocol   types.String `tfsdk:"transform_protocol"`
	Lifetime            types.Object `tfsdk:"lifetime"`
	TenantID            types.String `tfsdk:"tenant_id"`
}

func (r *ipsecPolicyResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_vpnaas_ipsec_policy"
}

func (r *ipsecPolicyResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	useState := []planmodifier.String{stringplanmodifier.UseStateForUnknown()}
	resp.Schema = schema.Schema{
		MarkdownDescription: "Manages a Neutron IPsec policy for VPNaaS in PCD.",
		Attributes: map[string]schema.Attribute{
			"id":                   schema.StringAttribute{Computed: true, MarkdownDescription: "The IPsec policy ID.", PlanModifiers: useState},
			"region":               schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "The region. Defaults to the provider's region.", PlanModifiers: useState},
			"name":                 schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "The name of the policy.", PlanModifiers: useState},
			"description":          schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "A description of the policy.", PlanModifiers: useState},
			"auth_algorithm":       schema.StringAttribute{Optional: true, Computed: true, Default: stringdefault.StaticString("sha256"), MarkdownDescription: "The authentication hash algorithm (sha1, sha256, sha384, sha512)."},
			"encapsulation_mode":   schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "The encapsulation mode (tunnel or transport).", PlanModifiers: useState},
			"encryption_algorithm": schema.StringAttribute{Optional: true, Computed: true, Default: stringdefault.StaticString("aes-128"), MarkdownDescription: "The encryption algorithm (e.g. 3des, aes-128, aes-192, aes-256)."},
			"pfs":                  schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "The perfect forward secrecy mode (group2, group5, group14, ...).", PlanModifiers: useState},
			"transform_protocol":   schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "The transform protocol (esp, ah, or ah-esp).", PlanModifiers: useState},
			"lifetime":             lifetimeSchema(),
			"tenant_id":            schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "The owning project. Changing this forces a new resource.", PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace(), stringplanmodifier.UseStateForUnknown()}},
		},
	}
}

func (r *ipsecPolicyResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.config = configureClient(req.ProviderData, &resp.Diagnostics)
}

func (r *ipsecPolicyResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan ipsecPolicyModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := r.config.NetworkV2Client()
	if err != nil {
		resp.Diagnostics.AddError("vpnaas: building network client", err.Error())
		return
	}

	opts := ipsecpolicies.CreateOpts{
		Name:                plan.Name.ValueString(),
		Description:         plan.Description.ValueString(),
		TenantID:            plan.TenantID.ValueString(),
		AuthAlgorithm:       ipsecpolicies.AuthAlgorithm(plan.AuthAlgorithm.ValueString()),
		EncapsulationMode:   ipsecpolicies.EncapsulationMode(plan.EncapsulationMode.ValueString()),
		EncryptionAlgorithm: ipsecpolicies.EncryptionAlgorithm(plan.EncryptionAlgorithm.ValueString()),
		PFS:                 ipsecpolicies.PFS(plan.PFS.ValueString()),
		TransformProtocol:   ipsecpolicies.TransformProtocol(plan.TransformProtocol.ValueString()),
	}
	if units, value, ok := lifetimeFromObject(ctx, plan.Lifetime, &resp.Diagnostics); ok {
		opts.Lifetime = &ipsecpolicies.LifetimeCreateOpts{Units: ipsecpolicies.Unit(units), Value: value}
	}
	if resp.Diagnostics.HasError() {
		return
	}

	policy, err := ipsecpolicies.Create(ctx, client, opts).Extract()
	if err != nil {
		resp.Diagnostics.AddError("vpnaas: creating IPsec policy", err.Error())
		return
	}

	resp.Diagnostics.Append(r.readInto(ctx, client, policy.ID, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *ipsecPolicyResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state ipsecPolicyModel
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

func (r *ipsecPolicyResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state ipsecPolicyModel
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

	opts := ipsecpolicies.UpdateOpts{}
	if !plan.Name.Equal(state.Name) {
		v := plan.Name.ValueString()
		opts.Name = &v
	}
	if !plan.Description.Equal(state.Description) {
		v := plan.Description.ValueString()
		opts.Description = &v
	}
	if !plan.AuthAlgorithm.Equal(state.AuthAlgorithm) {
		opts.AuthAlgorithm = ipsecpolicies.AuthAlgorithm(plan.AuthAlgorithm.ValueString())
	}
	if !plan.EncapsulationMode.Equal(state.EncapsulationMode) {
		opts.EncapsulationMode = ipsecpolicies.EncapsulationMode(plan.EncapsulationMode.ValueString())
	}
	if !plan.EncryptionAlgorithm.Equal(state.EncryptionAlgorithm) {
		opts.EncryptionAlgorithm = ipsecpolicies.EncryptionAlgorithm(plan.EncryptionAlgorithm.ValueString())
	}
	if !plan.PFS.Equal(state.PFS) {
		opts.PFS = ipsecpolicies.PFS(plan.PFS.ValueString())
	}
	if !plan.TransformProtocol.Equal(state.TransformProtocol) {
		opts.TransformProtocol = ipsecpolicies.TransformProtocol(plan.TransformProtocol.ValueString())
	}
	if !plan.Lifetime.Equal(state.Lifetime) {
		if units, value, ok := lifetimeFromObject(ctx, plan.Lifetime, &resp.Diagnostics); ok {
			opts.Lifetime = &ipsecpolicies.LifetimeUpdateOpts{Units: ipsecpolicies.Unit(units), Value: value}
		}
	}
	if resp.Diagnostics.HasError() {
		return
	}

	id := plan.ID.ValueString()
	if _, err := ipsecpolicies.Update(ctx, client, id, opts).Extract(); err != nil {
		resp.Diagnostics.AddError("vpnaas: updating IPsec policy", err.Error())
		return
	}

	resp.Diagnostics.Append(r.readInto(ctx, client, id, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *ipsecPolicyResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state ipsecPolicyModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := r.config.NetworkV2Client()
	if err != nil {
		resp.Diagnostics.AddError("vpnaas: building network client", err.Error())
		return
	}

	if err := ipsecpolicies.Delete(ctx, client, state.ID.ValueString()).ExtractErr(); err != nil {
		if gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
			return
		}
		resp.Diagnostics.AddError("vpnaas: deleting IPsec policy", err.Error())
	}
}

func (r *ipsecPolicyResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, path.Root("id"), req, resp)
}

func (r *ipsecPolicyResource) readInto(ctx context.Context, client *gophercloud.ServiceClient, id string, m *ipsecPolicyModel) diag.Diagnostics {
	notFound, diags := r.get(ctx, client, id, m)
	if notFound {
		diags.AddError("vpnaas: IPsec policy not found after write",
			fmt.Sprintf("IPsec policy %s was not found immediately after a create/update.", id))
	}
	return diags
}

func (r *ipsecPolicyResource) get(ctx context.Context, client *gophercloud.ServiceClient, id string, m *ipsecPolicyModel) (notFound bool, diags diag.Diagnostics) {
	policy, err := ipsecpolicies.Get(ctx, client, id).Extract()
	if err != nil {
		if gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
			return true, diags
		}
		diags.AddError("vpnaas: reading IPsec policy", err.Error())
		return false, diags
	}

	m.ID = types.StringValue(policy.ID)
	m.Name = types.StringValue(policy.Name)
	m.Description = types.StringValue(policy.Description)
	m.AuthAlgorithm = types.StringValue(policy.AuthAlgorithm)
	m.EncapsulationMode = types.StringValue(policy.EncapsulationMode)
	m.EncryptionAlgorithm = types.StringValue(policy.EncryptionAlgorithm)
	m.PFS = types.StringValue(policy.PFS)
	m.TransformProtocol = types.StringValue(policy.TransformProtocol)
	m.TenantID = types.StringValue(policy.TenantID)
	m.Lifetime = flattenLifetime(policy.Lifetime.Units, policy.Lifetime.Value)
	if m.Region.IsNull() || m.Region.IsUnknown() {
		m.Region = types.StringValue(r.config.Region)
	}
	return false, diags
}
