// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0
//
// Ported from terraform-provider-openstack v3.4.0
// (openstack/resource_openstack_keymanager_container_v1.go), adapted for the
// terraform-plugin-framework and PCD.

package keymanager

import (
	"context"
	"fmt"
	"net/http"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/keymanager/v1/containers"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/setplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/platform9/terraform-provider-pcd/internal/clients"
)

var (
	_ resource.Resource                = (*containerResource)(nil)
	_ resource.ResourceWithConfigure   = (*containerResource)(nil)
	_ resource.ResourceWithImportState = (*containerResource)(nil)
)

var (
	containerSecretRefObjType = types.ObjectType{AttrTypes: map[string]attr.Type{
		"secret_ref": types.StringType,
		"name":       types.StringType,
	}}
	containerConsumerObjType = types.ObjectType{AttrTypes: map[string]attr.Type{
		"name": types.StringType,
		"url":  types.StringType,
	}}
)

// NewContainerResource is the factory registered with the provider.
func NewContainerResource() resource.Resource {
	return &containerResource{}
}

type containerResource struct {
	config *clients.Config
}

type containerModel struct {
	ID           types.String `tfsdk:"id"`
	Name         types.String `tfsdk:"name"`
	Type         types.String `tfsdk:"type"`
	SecretRefs   types.Set    `tfsdk:"secret_refs"`
	ContainerRef types.String `tfsdk:"container_ref"`
	Status       types.String `tfsdk:"status"`
	Consumers    types.List   `tfsdk:"consumers"`
	CreatedAt    types.String `tfsdk:"created_at"`
	Region       types.String `tfsdk:"region"`
}

type containerSecretRefModel struct {
	SecretRef types.String `tfsdk:"secret_ref"`
	Name      types.String `tfsdk:"name"`
}

func (r *containerResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_keymanager_container"
}

func (r *containerResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	useState := []planmodifier.String{stringplanmodifier.UseStateForUnknown()}
	forceNew := []planmodifier.String{stringplanmodifier.RequiresReplace()}
	forceNewC := []planmodifier.String{stringplanmodifier.RequiresReplace(), stringplanmodifier.UseStateForUnknown()}
	resp.Schema = schema.Schema{
		MarkdownDescription: "Manages a container in PCD's Barbican key manager — a named grouping of secrets " +
			"(generic, RSA, or certificate). Containers are immutable; any change forces a new resource.",
		Attributes: map[string]schema.Attribute{
			"id":   schema.StringAttribute{Computed: true, MarkdownDescription: "The container UUID.", PlanModifiers: useState},
			"name": schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "The name of the container. Changing this forces a new resource.", PlanModifiers: forceNewC},
			"type": schema.StringAttribute{Required: true, MarkdownDescription: "The container type: generic, rsa, or certificate. Changing this forces a new resource.", PlanModifiers: forceNew},
			"secret_refs": schema.SetNestedAttribute{
				Optional:            true,
				MarkdownDescription: "The secrets in the container. Changing these forces a new resource.",
				NestedObject: schema.NestedAttributeObject{Attributes: map[string]schema.Attribute{
					"secret_ref": schema.StringAttribute{Required: true, MarkdownDescription: "The full secret reference URL."},
					"name":       schema.StringAttribute{Optional: true, MarkdownDescription: "A label for the secret within the container (e.g. private_key)."},
				}},
				PlanModifiers: []planmodifier.Set{setplanmodifier.RequiresReplace()},
			},
			"container_ref": schema.StringAttribute{Computed: true, MarkdownDescription: "The full Barbican container reference URL.", PlanModifiers: useState},
			"status":        schema.StringAttribute{Computed: true, MarkdownDescription: "The container status (e.g. ACTIVE).", PlanModifiers: useState},
			"consumers": schema.ListNestedAttribute{
				Computed:            true,
				MarkdownDescription: "Services consuming this container.",
				NestedObject: schema.NestedAttributeObject{Attributes: map[string]schema.Attribute{
					"name": schema.StringAttribute{Computed: true, MarkdownDescription: "The consumer name."},
					"url":  schema.StringAttribute{Computed: true, MarkdownDescription: "The consumer URL."},
				}},
			},
			"created_at": schema.StringAttribute{Computed: true, MarkdownDescription: "Creation timestamp (RFC3339).", PlanModifiers: useState},
			"region":     schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "The region. Defaults to the provider's region.", PlanModifiers: useState},
		},
	}
}

func (r *containerResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.config = configureClient(req.ProviderData, &resp.Diagnostics)
}

func (r *containerResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan containerModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := r.config.KeyManagerV1Client()
	if err != nil {
		resp.Diagnostics.AddError("keymanager: building v1 client", err.Error())
		return
	}

	createOpts := containers.CreateOpts{
		Type: containers.ContainerType(plan.Type.ValueString()),
		Name: plan.Name.ValueString(),
	}
	if !plan.SecretRefs.IsNull() && !plan.SecretRefs.IsUnknown() {
		var refs []containerSecretRefModel
		resp.Diagnostics.Append(plan.SecretRefs.ElementsAs(ctx, &refs, false)...)
		if resp.Diagnostics.HasError() {
			return
		}
		for _, ref := range refs {
			createOpts.SecretRefs = append(createOpts.SecretRefs, containers.SecretRef{
				SecretRef: ref.SecretRef.ValueString(),
				Name:      ref.Name.ValueString(),
			})
		}
	}

	container, err := containers.Create(ctx, client, createOpts).Extract()
	if err != nil {
		resp.Diagnostics.AddError("keymanager: creating container", err.Error())
		return
	}

	_, readDiags := r.readInto(ctx, client, refToID(container.ContainerRef), &plan)
	resp.Diagnostics.Append(readDiags...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *containerResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state containerModel
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
		resp.Diagnostics.AddWarning("Container not found",
			fmt.Sprintf("Container %s no longer exists and was removed from state.", state.ID.ValueString()))
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
func (r *containerResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan containerModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *containerResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state containerModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := r.config.KeyManagerV1Client()
	if err != nil {
		resp.Diagnostics.AddError("keymanager: building v1 client", err.Error())
		return
	}

	if err := containers.Delete(ctx, client, state.ID.ValueString()).ExtractErr(); err != nil {
		if gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
			return
		}
		resp.Diagnostics.AddError("keymanager: deleting container", err.Error())
	}
}

func (r *containerResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, path.Root("id"), req, resp)
}

// readInto refreshes a container. secret_refs is echo-only (it holds the user's
// requested set and is ForceNew); it is populated from the server only when unset
// (import). consumers is refreshed every read (it can change out-of-band).
func (r *containerResource) readInto(ctx context.Context, client *gophercloud.ServiceClient, id string, m *containerModel) (notFound bool, diags diag.Diagnostics) {
	container, err := containers.Get(ctx, client, id).Extract()
	if err != nil {
		if gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
			return true, diags
		}
		diags.AddError("keymanager: reading container", err.Error())
		return false, diags
	}

	m.ID = types.StringValue(id)
	m.ContainerRef = types.StringValue(container.ContainerRef)
	m.Status = types.StringValue(container.Status)
	m.CreatedAt = types.StringValue(formatTime(container.Created))
	if unset(m.Name) {
		m.Name = types.StringValue(container.Name)
	}
	if unset(m.Type) {
		m.Type = types.StringValue(container.Type)
	}

	if m.SecretRefs.IsNull() || m.SecretRefs.IsUnknown() {
		refs := make([]containerSecretRefModel, 0, len(container.SecretRefs))
		for _, sr := range container.SecretRefs {
			refs = append(refs, containerSecretRefModel{
				SecretRef: types.StringValue(sr.SecretRef),
				Name:      types.StringValue(sr.Name),
			})
		}
		refSet, d := types.SetValueFrom(ctx, containerSecretRefObjType, refs)
		diags = append(diags, d...)
		m.SecretRefs = refSet
	}

	consumers := make([]map[string]string, 0, len(container.Consumers))
	for _, c := range container.Consumers {
		consumers = append(consumers, map[string]string{"name": c.Name, "url": c.URL})
	}
	consList, d := types.ListValueFrom(ctx, containerConsumerObjType, consumers)
	diags = append(diags, d...)
	m.Consumers = consList

	if m.Region.IsNull() || m.Region.IsUnknown() {
		m.Region = types.StringValue(r.config.Region)
	}
	return false, diags
}
