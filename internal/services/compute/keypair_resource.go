// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0
//
// Ported from terraform-provider-openstack v3.4.0
// (openstack/resource_openstack_compute_keypair_v2.go), adapted for the
// terraform-plugin-framework and PCD.

package compute

import (
	"context"
	"fmt"
	"net/http"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/keypairs"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/platform9/terraform-provider-pcd/internal/clients"
)

var (
	_ resource.Resource                = (*keypairResource)(nil)
	_ resource.ResourceWithConfigure   = (*keypairResource)(nil)
	_ resource.ResourceWithImportState = (*keypairResource)(nil)
)

// NewKeypairResource is the factory registered with the provider.
func NewKeypairResource() resource.Resource {
	return &keypairResource{}
}

type keypairResource struct {
	config *clients.Config
}

type keypairModel struct {
	ID          types.String `tfsdk:"id"`
	Name        types.String `tfsdk:"name"`
	PublicKey   types.String `tfsdk:"public_key"`
	PrivateKey  types.String `tfsdk:"private_key"`
	Fingerprint types.String `tfsdk:"fingerprint"`
	UserID      types.String `tfsdk:"user_id"`
	Region      types.String `tfsdk:"region"`
}

func (r *keypairResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_compute_keypair"
}

func (r *keypairResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	fnC := []planmodifier.String{stringplanmodifier.RequiresReplace(), stringplanmodifier.UseStateForUnknown()}
	resp.Schema = schema.Schema{
		MarkdownDescription: "Manages an SSH keypair in PCD's compute (Nova) service. If public_key is omitted, " +
			"Nova generates a keypair and returns the private key once (on create).",
		Attributes: map[string]schema.Attribute{
			"id":          schema.StringAttribute{Computed: true, MarkdownDescription: "Mirrors name.", PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()}},
			"name":        schema.StringAttribute{Required: true, MarkdownDescription: "The name of the keypair. Changing this forces a new resource.", PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()}},
			"public_key":  schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "The public key. If omitted, one is generated. Changing this forces a new resource.", PlanModifiers: fnC},
			"private_key": schema.StringAttribute{Computed: true, Sensitive: true, MarkdownDescription: "The generated private key (only set when public_key was not supplied).", PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()}},
			"fingerprint": schema.StringAttribute{Computed: true, MarkdownDescription: "The keypair fingerprint.", PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()}},
			"user_id":     schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "The user that owns the keypair. Changing this forces a new resource.", PlanModifiers: fnC},
			"region":      schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "The region. Defaults to the provider's region.", PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()}},
		},
	}
}

func (r *keypairResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.config = configureClient(req.ProviderData, &resp.Diagnostics)
}

func (r *keypairResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan keypairModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := r.config.ComputeV2Client()
	if err != nil {
		resp.Diagnostics.AddError("compute: building v2 client", err.Error())
		return
	}

	kp, err := keypairs.Create(ctx, client, keypairs.CreateOpts{
		Name:      plan.Name.ValueString(),
		PublicKey: plan.PublicKey.ValueString(),
		UserID:    plan.UserID.ValueString(),
	}).Extract()
	if err != nil {
		resp.Diagnostics.AddError("compute: creating keypair", err.Error())
		return
	}

	plan.ID = types.StringValue(kp.Name)
	plan.PublicKey = types.StringValue(kp.PublicKey)
	plan.Fingerprint = types.StringValue(kp.Fingerprint)
	plan.UserID = types.StringValue(kp.UserID)
	plan.PrivateKey = types.StringValue(kp.PrivateKey) // empty unless Nova generated the pair
	if plan.Region.IsNull() || plan.Region.IsUnknown() {
		plan.Region = types.StringValue(r.config.Region)
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *keypairResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state keypairModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := r.config.ComputeV2Client()
	if err != nil {
		resp.Diagnostics.AddError("compute: building v2 client", err.Error())
		return
	}

	kp, err := keypairs.Get(ctx, client, state.Name.ValueString(), keypairs.GetOpts{UserID: state.UserID.ValueString()}).Extract()
	if err != nil {
		if gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
			resp.Diagnostics.AddWarning("Keypair not found",
				fmt.Sprintf("Keypair %s no longer exists and was removed from state.", state.Name.ValueString()))
			resp.State.RemoveResource(ctx)
			return
		}
		resp.Diagnostics.AddError("compute: reading keypair", err.Error())
		return
	}

	state.ID = types.StringValue(kp.Name)
	state.PublicKey = types.StringValue(kp.PublicKey)
	state.Fingerprint = types.StringValue(kp.Fingerprint)
	state.UserID = types.StringValue(kp.UserID)
	if state.Region.IsNull() || state.Region.IsUnknown() {
		state.Region = types.StringValue(r.config.Region)
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

// Update is required by the interface but never invoked (every attribute forces replacement).
func (r *keypairResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan keypairModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *keypairResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state keypairModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := r.config.ComputeV2Client()
	if err != nil {
		resp.Diagnostics.AddError("compute: building v2 client", err.Error())
		return
	}

	if err := keypairs.Delete(ctx, client, state.Name.ValueString(), keypairs.DeleteOpts{UserID: state.UserID.ValueString()}).ExtractErr(); err != nil {
		if gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
			return
		}
		resp.Diagnostics.AddError("compute: deleting keypair", err.Error())
	}
}

func (r *keypairResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, path.Root("name"), req, resp)
}
