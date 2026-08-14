// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package resmgr

import (
	"context"
	"encoding/json"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/boolplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/objectplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-framework/types/basetypes"

	"github.com/platform9/terraform-provider-pcd/internal/clients"
)

var (
	_ resource.Resource                = (*blueprintResource)(nil)
	_ resource.ResourceWithConfigure   = (*blueprintResource)(nil)
	_ resource.ResourceWithImportState = (*blueprintResource)(nil)
)

// NewBlueprintResource is the factory registered with the provider.
func NewBlueprintResource() resource.Resource {
	return &blueprintResource{}
}

type blueprintResource struct {
	config *clients.Config
}

type blueprintResourceModel struct {
	Name                      types.String `tfsdk:"name"`
	NetworkingType            types.String `tfsdk:"networking_type"`
	EnableDistributedRouting  types.Bool   `tfsdk:"enable_distributed_routing"`
	DNSDomainName             types.String `tfsdk:"dns_domain_name"`
	VirtualNetworking         types.Object `tfsdk:"virtual_networking"`
	ImageLibraryStorage       types.String `tfsdk:"image_library_storage"`
	ImageLibrarySharedStorage types.Bool   `tfsdk:"image_library_shared_storage"`
	InstanceSharedStorage     types.Bool   `tfsdk:"instance_shared_storage"`
	VMStorage                 types.String `tfsdk:"vm_storage"`
	VMHighAvailability        types.Object `tfsdk:"vm_high_availability"`
	AutoResourceRebalancing   types.Object `tfsdk:"auto_resource_rebalancing"`
	StorageBackendsJSON       types.String `tfsdk:"storage_backends_json"`
}

var haAttrTypes = map[string]attr.Type{
	"enabled": types.BoolType,
}

var rebalanceAttrTypes = map[string]attr.Type{
	"enabled":                    types.BoolType,
	"rebalancing_strategy":       types.StringType,
	"rebalancing_frequency_mins": types.Int64Type,
}

func (r *blueprintResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_cluster_blueprint"
}

func (r *blueprintResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	useState := []planmodifier.String{stringplanmodifier.UseStateForUnknown()}
	boolUseState := []planmodifier.Bool{boolplanmodifier.UseStateForUnknown()}
	objUseState := []planmodifier.Object{objectplanmodifier.UseStateForUnknown()}
	resp.Schema = schema.Schema{
		MarkdownDescription: "Manages a PCD cluster blueprint — the shared, declarative configuration that virtualized " +
			"clusters inherit (networking, image library, VM storage, and Cinder backends). PCD supports a single " +
			"blueprint per region, so the common workflow is to `terraform import` the existing blueprint and then " +
			"manage it. Changing `name` forces a new resource. Destroying this resource only removes it from " +
			"Terraform state — it does not delete the region's blueprint (use the PCD UI for that).",
		Attributes: map[string]schema.Attribute{
			"name":                       schema.StringAttribute{Required: true, MarkdownDescription: "The blueprint (cluster) name. Changing this forces a new resource.", PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()}},
			"networking_type":            schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "The networking type: `ovn` (default) or `ovs`.", PlanModifiers: useState},
			"enable_distributed_routing": schema.BoolAttribute{Optional: true, Computed: true, MarkdownDescription: "Whether cluster-wide distributed routing is enabled.", PlanModifiers: boolUseState},
			"dns_domain_name":            schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "The internal DNS domain name suffix for VMs (not Designate).", PlanModifiers: useState},
			"virtual_networking": schema.SingleNestedAttribute{
				Optional:            true,
				Computed:            true,
				MarkdownDescription: "Virtual (tenant) networking settings.",
				PlanModifiers:       objUseState,
				Attributes: map[string]schema.Attribute{
					"enabled":       schema.BoolAttribute{Optional: true, Computed: true, MarkdownDescription: "Whether virtual networking is enabled."},
					"underlay_type": schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "The underlay type: `vlan` or `other`."},
					"vnid_range":    schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "The VLAN/VNI segmentation ID range (e.g. `1000:2000`)."},
				},
			},
			"image_library_storage":        schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "The image library storage location.", PlanModifiers: useState},
			"image_library_shared_storage": schema.BoolAttribute{Optional: true, Computed: true, MarkdownDescription: "Whether the image library uses shared storage.", PlanModifiers: boolUseState},
			"instance_shared_storage":      schema.BoolAttribute{Optional: true, Computed: true, MarkdownDescription: "Whether instance ephemeral storage is shared.", PlanModifiers: boolUseState},
			"vm_storage":                   schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "The VM ephemeral storage path.", PlanModifiers: useState},
			"vm_high_availability": schema.SingleNestedAttribute{
				Optional:            true,
				Computed:            true,
				MarkdownDescription: "VM high-availability settings.",
				PlanModifiers:       objUseState,
				Attributes: map[string]schema.Attribute{
					"enabled": schema.BoolAttribute{Optional: true, Computed: true, MarkdownDescription: "Auto-detect host failure and recover VMs."},
				},
			},
			"auto_resource_rebalancing": schema.SingleNestedAttribute{
				Optional:            true,
				Computed:            true,
				MarkdownDescription: "Automatic workload-rebalancing settings.",
				PlanModifiers:       objUseState,
				Attributes: map[string]schema.Attribute{
					"enabled":                    schema.BoolAttribute{Optional: true, Computed: true, MarkdownDescription: "Whether auto-rebalancing is enabled."},
					"rebalancing_strategy":       schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "`vm_workload_consolidation` or `node_resource_consolidation`."},
					"rebalancing_frequency_mins": schema.Int64Attribute{Optional: true, Computed: true, MarkdownDescription: "Rebalancing frequency in minutes (1-60)."},
				},
			},
			// storage_backends_json carries plaintext driver credentials, so it is
			// Sensitive. Leave it unset to preserve the existing backends (the value
			// is read back from the server and re-sent in the required complete-object
			// PUT); set it — using the server's canonical nested JSON shape — only to
			// change the Cinder backends. Required to CREATE a new blueprint.
			"storage_backends_json": schema.StringAttribute{Optional: true, Computed: true, Sensitive: true, MarkdownDescription: "The Cinder storage backends as a JSON string (contains credentials). Read back from the server; set it only to change backends.", PlanModifiers: useState},
		},
	}
}

func (r *blueprintResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.config = configureClient(req.ProviderData, &resp.Diagnostics)
}

func (r *blueprintResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan blueprintResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := r.config.ResmgrV2Client()
	if err != nil {
		resp.Diagnostics.AddError("resmgr: building client", err.Error())
		return
	}

	// PCD rejects a blueprint create that lacks storage backends (opaque 500), so
	// require them explicitly rather than surfacing a server error.
	if plan.StorageBackendsJSON.IsNull() || plan.StorageBackendsJSON.IsUnknown() || plan.StorageBackendsJSON.ValueString() == "" {
		resp.Diagnostics.AddError("resmgr: creating blueprint",
			"storage_backends_json is required to create a blueprint (PCD rejects a blueprint with no storage backends).")
		return
	}

	body := r.toAPI(ctx, &plan, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}
	if err := postJSON(ctx, client, client.ServiceURL("blueprint"), body, nil); err != nil {
		resp.Diagnostics.AddError("resmgr: creating blueprint", err.Error())
		return
	}

	r.refresh(ctx, client, &plan, "blueprint created", &resp.Diagnostics)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *blueprintResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state blueprintResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := r.config.ResmgrV2Client()
	if err != nil {
		resp.Diagnostics.AddError("resmgr: building client", err.Error())
		return
	}

	var bp blueprintAPI
	if err := getJSON(ctx, client, client.ServiceURL("blueprint", state.Name.ValueString()), &bp); err != nil {
		if isNotFound(err) {
			resp.State.RemoveResource(ctx)
			return
		}
		resp.Diagnostics.AddError("resmgr: reading blueprint", err.Error())
		return
	}
	resp.Diagnostics.Append(r.setState(&state, &bp)...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *blueprintResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan blueprintResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := r.config.ResmgrV2Client()
	if err != nil {
		resp.Diagnostics.AddError("resmgr: building client", err.Error())
		return
	}

	body := r.toAPI(ctx, &plan, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}
	if err := putJSON(ctx, client, client.ServiceURL("blueprint", plan.Name.ValueString()), body, nil); err != nil {
		resp.Diagnostics.AddError("resmgr: updating blueprint", err.Error())
		return
	}

	r.refresh(ctx, client, &plan, "blueprint updated", &resp.Diagnostics)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

// refresh reconciles state after a write. It reads the server object but keeps
// the user-configured (known) plan values, so the write API's re-serialization
// or normalization of a configured attribute (notably the storage_backends_json
// JSON blob) cannot trip "inconsistent result after apply"; only attributes the
// user left unknown are taken from the server. A read-back failure is a warning,
// not an error, so a successful write is never reported as a failed apply.
func (r *blueprintResource) refresh(ctx context.Context, client *gophercloud.ServiceClient, plan *blueprintResourceModel, what string, diags *diag.Diagnostics) {
	saved := *plan
	if readDiags := r.readInto(ctx, client, plan.Name.ValueString(), plan); readDiags.HasError() {
		diags.AddWarning("resmgr: "+what+" but read-back failed",
			"The write was applied; state reflects the plan and reconciles on the next refresh.")
		*plan = saved
		return
	}
	restoreKnown(plan, &saved)
}

// restoreKnown puts the user-configured (known) values from saved back over the
// server read-back, leaving only the plan's unknown attributes server-sourced.
func restoreKnown(m, saved *blueprintResourceModel) {
	m.Name = saved.Name
	if knownStr(saved.NetworkingType) {
		m.NetworkingType = saved.NetworkingType
	}
	if knownBool(saved.EnableDistributedRouting) {
		m.EnableDistributedRouting = saved.EnableDistributedRouting
	}
	if knownStr(saved.DNSDomainName) {
		m.DNSDomainName = saved.DNSDomainName
	}
	if knownObj(saved.VirtualNetworking) {
		m.VirtualNetworking = saved.VirtualNetworking
	}
	if knownStr(saved.ImageLibraryStorage) {
		m.ImageLibraryStorage = saved.ImageLibraryStorage
	}
	if knownBool(saved.ImageLibrarySharedStorage) {
		m.ImageLibrarySharedStorage = saved.ImageLibrarySharedStorage
	}
	if knownBool(saved.InstanceSharedStorage) {
		m.InstanceSharedStorage = saved.InstanceSharedStorage
	}
	if knownStr(saved.VMStorage) {
		m.VMStorage = saved.VMStorage
	}
	if knownObj(saved.VMHighAvailability) {
		m.VMHighAvailability = saved.VMHighAvailability
	}
	if knownObj(saved.AutoResourceRebalancing) {
		m.AutoResourceRebalancing = saved.AutoResourceRebalancing
	}
	if knownStr(saved.StorageBackendsJSON) {
		m.StorageBackendsJSON = saved.StorageBackendsJSON
	}
}

func knownStr(v types.String) bool { return !v.IsNull() && !v.IsUnknown() }
func knownBool(v types.Bool) bool  { return !v.IsNull() && !v.IsUnknown() }
func knownObj(v types.Object) bool { return !v.IsNull() && !v.IsUnknown() }

// Delete removes the blueprint from Terraform state WITHOUT calling the API. PCD
// supports a single blueprint per region and it is normally imported, so a
// `terraform destroy` should stop managing it rather than physically delete the
// region's cluster-defining blueprint (which every host and cluster depends on).
// Remove it from the PCD UI if you truly need to delete it.
func (r *blueprintResource) Delete(_ context.Context, _ resource.DeleteRequest, _ *resource.DeleteResponse) {
}

func (r *blueprintResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, path.Root("name"), req, resp)
}

func (r *blueprintResource) readInto(ctx context.Context, client *gophercloud.ServiceClient, name string, m *blueprintResourceModel) diag.Diagnostics {
	var bp blueprintAPI
	if err := getJSON(ctx, client, client.ServiceURL("blueprint", name), &bp); err != nil {
		var diags diag.Diagnostics
		diags.AddError("resmgr: reading blueprint", err.Error())
		return diags
	}
	return r.setState(m, &bp)
}

func (r *blueprintResource) setState(m *blueprintResourceModel, bp *blueprintAPI) diag.Diagnostics {
	var diags diag.Diagnostics
	m.Name = types.StringValue(bp.Name)
	m.NetworkingType = types.StringValue(bp.NetworkingType)
	m.EnableDistributedRouting = types.BoolValue(bp.EnableDistributedRouting)
	m.DNSDomainName = types.StringValue(bp.DNSDomainName)
	m.ImageLibraryStorage = types.StringValue(bp.ImageLibraryStorage)
	m.ImageLibrarySharedStorage = types.BoolValue(bp.ImageLibrarySharedStorage)
	m.InstanceSharedStorage = types.BoolValue(bp.InstanceSharedStorage)
	m.VMStorage = types.StringValue(bp.VMStorage)

	if bp.VirtualNetworking != nil {
		obj, d := types.ObjectValue(virtualNetworkingAttrTypes, map[string]attr.Value{
			"enabled":       types.BoolValue(bp.VirtualNetworking.Enabled),
			"underlay_type": types.StringValue(bp.VirtualNetworking.UnderlayType),
			"vnid_range":    types.StringValue(bp.VirtualNetworking.VnidRange),
		})
		diags.Append(d...)
		m.VirtualNetworking = obj
	} else {
		m.VirtualNetworking = types.ObjectNull(virtualNetworkingAttrTypes)
	}

	if bp.VMHighAvailability != nil {
		obj, d := types.ObjectValue(haAttrTypes, map[string]attr.Value{
			"enabled": types.BoolValue(bp.VMHighAvailability.Enabled),
		})
		diags.Append(d...)
		m.VMHighAvailability = obj
	} else {
		m.VMHighAvailability = types.ObjectNull(haAttrTypes)
	}

	if bp.AutoResourceRebalancing != nil {
		obj, d := types.ObjectValue(rebalanceAttrTypes, map[string]attr.Value{
			"enabled":                    types.BoolValue(bp.AutoResourceRebalancing.Enabled),
			"rebalancing_strategy":       types.StringValue(bp.AutoResourceRebalancing.RebalancingStrategy),
			"rebalancing_frequency_mins": types.Int64Value(bp.AutoResourceRebalancing.RebalancingFrequencyMins),
		})
		diags.Append(d...)
		m.AutoResourceRebalancing = obj
	} else {
		m.AutoResourceRebalancing = types.ObjectNull(rebalanceAttrTypes)
	}

	if len(bp.StorageBackends) > 0 && string(bp.StorageBackends) != "null" {
		m.StorageBackendsJSON = types.StringValue(canonicalJSON(bp.StorageBackends))
	} else {
		m.StorageBackendsJSON = types.StringNull()
	}
	return diags
}

// toAPI builds the full blueprint request body from the plan (the write API
// requires a complete object).
func (r *blueprintResource) toAPI(ctx context.Context, m *blueprintResourceModel, diags *diag.Diagnostics) *blueprintAPI {
	bp := &blueprintAPI{
		Name:                      m.Name.ValueString(),
		NetworkingType:            m.NetworkingType.ValueString(),
		EnableDistributedRouting:  m.EnableDistributedRouting.ValueBool(),
		DNSDomainName:             m.DNSDomainName.ValueString(),
		ImageLibraryStorage:       m.ImageLibraryStorage.ValueString(),
		ImageLibrarySharedStorage: m.ImageLibrarySharedStorage.ValueBool(),
		InstanceSharedStorage:     m.InstanceSharedStorage.ValueBool(),
		VMStorage:                 m.VMStorage.ValueString(),
	}

	if !m.VirtualNetworking.IsNull() && !m.VirtualNetworking.IsUnknown() {
		var vn struct {
			Enabled      types.Bool   `tfsdk:"enabled"`
			UnderlayType types.String `tfsdk:"underlay_type"`
			VnidRange    types.String `tfsdk:"vnid_range"`
		}
		diags.Append(m.VirtualNetworking.As(ctx, &vn, basetypes.ObjectAsOptions{})...)
		bp.VirtualNetworking = &virtualNetworkingAPI{
			Enabled:      vn.Enabled.ValueBool(),
			UnderlayType: vn.UnderlayType.ValueString(),
			VnidRange:    vn.VnidRange.ValueString(),
		}
	}

	if !m.VMHighAvailability.IsNull() && !m.VMHighAvailability.IsUnknown() {
		var ha struct {
			Enabled types.Bool `tfsdk:"enabled"`
		}
		diags.Append(m.VMHighAvailability.As(ctx, &ha, basetypes.ObjectAsOptions{})...)
		bp.VMHighAvailability = &haAPI{Enabled: ha.Enabled.ValueBool()}
	}

	if !m.AutoResourceRebalancing.IsNull() && !m.AutoResourceRebalancing.IsUnknown() {
		var rb struct {
			Enabled                  types.Bool   `tfsdk:"enabled"`
			RebalancingStrategy      types.String `tfsdk:"rebalancing_strategy"`
			RebalancingFrequencyMins types.Int64  `tfsdk:"rebalancing_frequency_mins"`
		}
		diags.Append(m.AutoResourceRebalancing.As(ctx, &rb, basetypes.ObjectAsOptions{})...)
		bp.AutoResourceRebalancing = &rebalanceAPI{
			Enabled:                  rb.Enabled.ValueBool(),
			RebalancingStrategy:      rb.RebalancingStrategy.ValueString(),
			RebalancingFrequencyMins: rb.RebalancingFrequencyMins.ValueInt64(),
		}
	}

	if !m.StorageBackendsJSON.IsNull() && !m.StorageBackendsJSON.IsUnknown() && m.StorageBackendsJSON.ValueString() != "" {
		bp.StorageBackends = json.RawMessage(m.StorageBackendsJSON.ValueString())
	}

	return bp
}
