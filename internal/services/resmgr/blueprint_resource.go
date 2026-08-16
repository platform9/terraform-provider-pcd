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
	VNCFloatingIP             types.String `tfsdk:"vnc_floating_ip"`
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
			"name": schema.StringAttribute{Required: true, MarkdownDescription: "The blueprint (cluster) name. Changing this forces a new resource.", PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()}},
			// networking_type and enable_distributed_routing are not user-configurable
			// anywhere in the product: the PCD UI hardcodes ovn / true and pcdctl has
			// no blueprint capability. The API, however, REQUIRES both on create and
			// has no server-side default (omitted or "" networkingType is rejected;
			// omitted enableDistributedRouting is a 500). So the provider takes the
			// UI's role and always sends the product values. Computed-only keeps them
			// visible in state and rejects any attempt to set them.
			"networking_type":            schema.StringAttribute{Computed: true, MarkdownDescription: "The region's networking type. Always `ovn` — set by the provider to match the product; not user-configurable.", PlanModifiers: useState},
			"enable_distributed_routing": schema.BoolAttribute{Computed: true, MarkdownDescription: "Whether distributed routing is enabled. Always `true` — set by the provider to match the product; not user-configurable.", PlanModifiers: boolUseState},
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
			"vm_storage":                   schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "The path on each hypervisor where instance (ephemeral) storage lives, e.g. `/opt/data/instances`.", PlanModifiers: useState},
			"instance_shared_storage": schema.BoolAttribute{Optional: true, Computed: true, MarkdownDescription: "Set `true` when `vm_storage` is mounted as shared storage (e.g. NFS) across all hosts, so PCD can treat instance disks as shared. " +
				"Matches the UI's \"Enable if this path is mounted as shared storage across all hosts\" toggle. Defaults to `false` (local disk).", PlanModifiers: boolUseState},
			"vnc_floating_ip": schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "A floating IP through which VM VNC consoles are reached. Leave unset for none.", PlanModifiers: useState},
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

	// resmgr silently discards vncFloatingIp on POST and only persists it on
	// PUT (verified against 2026.4: identical body, POST stores null, PUT stores
	// the value). The PCD UI never trips this because it creates first and sets
	// the VNC IP on a later save. Compensate the same way: follow the create
	// with a PUT whenever the user configured one, so a single apply matches
	// the configuration.
	if body.VNCFloatingIP != nil && *body.VNCFloatingIP != "" {
		if err := putJSON(ctx, client, client.ServiceURL("blueprint", plan.Name.ValueString()), body, nil); err != nil {
			resp.Diagnostics.AddError("resmgr: setting vnc_floating_ip on new blueprint",
				"The blueprint was created but the follow-up PUT that persists vnc_floating_ip failed: "+err.Error())
			return
		}
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
	if knownStr(saved.VNCFloatingIP) {
		m.VNCFloatingIP = saved.VNCFloatingIP
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

	// The API reports "no floating IP" as null; the provider models it as ""
	// so a user can clear the value with vnc_floating_ip = "" and get a stable
	// plan (a nullable string cannot be cleared from config any other way).
	if bp.VNCFloatingIP != nil {
		m.VNCFloatingIP = types.StringValue(*bp.VNCFloatingIP)
	} else {
		m.VNCFloatingIP = types.StringValue("")
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
	// networkingType and enableDistributedRouting are required by the API and
	// have no server default; the product's fixed values are supplied here (the
	// same ones the PCD UI hardcodes). An existing blueprint's stored values are
	// preserved on update via the read-back in state.
	networkingType := m.NetworkingType.ValueString()
	if networkingType == "" {
		networkingType = "ovn"
	}
	dvr := true
	if knownBool(m.EnableDistributedRouting) {
		dvr = m.EnableDistributedRouting.ValueBool()
	}
	bp := &blueprintAPI{
		Name:                      m.Name.ValueString(),
		NetworkingType:            networkingType,
		EnableDistributedRouting:  dvr,
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

	// Send the value whenever configured, including "": the API's PUT is a
	// complete-object write, so omitting the key would silently keep the old IP
	// when the user's intent is to clear it. A pointer to "" clears server-side.
	if knownStr(m.VNCFloatingIP) {
		v := m.VNCFloatingIP.ValueString()
		bp.VNCFloatingIP = &v
	}

	if !m.StorageBackendsJSON.IsNull() && !m.StorageBackendsJSON.IsUnknown() && m.StorageBackendsJSON.ValueString() != "" {
		bp.StorageBackends = json.RawMessage(m.StorageBackendsJSON.ValueString())
	}

	return bp
}
