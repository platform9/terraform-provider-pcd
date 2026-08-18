// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package resmgr

import (
	"context"
	"fmt"
	"strings"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/platform9/terraform-provider-pcd/internal/clients"
)

var (
	_ resource.Resource                = (*hostConfigResource)(nil)
	_ resource.ResourceWithConfigure   = (*hostConfigResource)(nil)
	_ resource.ResourceWithImportState = (*hostConfigResource)(nil)
)

// NewHostConfigResource is the factory registered with the provider.
func NewHostConfigResource() resource.Resource {
	return &hostConfigResource{}
}

type hostConfigResource struct {
	config *clients.Config
}

type hostConfigModel struct {
	ID                     types.String `tfsdk:"id"`
	Name                   types.String `tfsdk:"name"`
	MgmtInterface          types.String `tfsdk:"mgmt_interface"`
	VMConsoleInterface     types.String `tfsdk:"vm_console_interface"`
	HostLivenessInterface  types.String `tfsdk:"host_liveness_interface"`
	TunnelingInterface     types.String `tfsdk:"tunneling_interface"`
	ImagelibInterface      types.String `tfsdk:"imagelib_interface"`
	LiveMigrationInterface types.String `tfsdk:"live_migration_interface"`
	NetworkLabels          types.Map    `tfsdk:"network_labels"`
	ClusterName            types.String `tfsdk:"cluster_name"`
	GPUPci                 types.List   `tfsdk:"gpu_pci"`
}

// hostConfigAPI is the JSON body/response for /hostconfigs.
type hostConfigAPI struct {
	ID                     string            `json:"id,omitempty"`
	Name                   string            `json:"name"`
	MgmtInterface          string            `json:"mgmtInterface,omitempty"`
	VMConsoleInterface     string            `json:"vmConsoleInterface,omitempty"`
	HostLivenessInterface  string            `json:"hostLivenessInterface,omitempty"`
	TunnelingInterface     string            `json:"tunnelingInterface,omitempty"`
	ImagelibInterface      string            `json:"imagelibInterface,omitempty"`
	LiveMigrationInterface string            `json:"liveMigrationInterface,omitempty"`
	NetworkLabels          map[string]string `json:"networkLabels,omitempty"`
	ClusterName            string            `json:"clusterName,omitempty"`
	GPUPci                 []string          `json:"gpuPci,omitempty"`
}

func (r *hostConfigResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_host_config"
}

func (r *hostConfigResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	useState := []planmodifier.String{stringplanmodifier.UseStateForUnknown()}
	iface := func(desc string) schema.StringAttribute {
		return schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: desc, PlanModifiers: useState}
	}
	resp.Schema = schema.Schema{
		MarkdownDescription: "Manages a PCD host configuration: the mapping of traffic types (management, VM console,  Destroying one is refused while any host is still assigned to it: PCD leaves such a host unable to be assigned a host configuration ever again, so remove the `pcd_host_config_assignment` first." +
			"tunnels, image library, live migration, host liveness) to network interfaces, plus physical-network labels.",
		Attributes: map[string]schema.Attribute{
			"id":                       schema.StringAttribute{Computed: true, MarkdownDescription: "The host configuration ID.", PlanModifiers: useState},
			"name":                     schema.StringAttribute{Required: true, MarkdownDescription: "The host configuration name."},
			"mgmt_interface":           iface("The management-traffic interface."),
			"vm_console_interface":     iface("The VM-console interface."),
			"host_liveness_interface":  iface("The host-liveness interface."),
			"tunneling_interface":      iface("The virtual-network tunnels interface."),
			"imagelib_interface":       iface("The image-library interface."),
			"live_migration_interface": iface("The live-migration interface."),
			"network_labels":           schema.MapAttribute{Optional: true, Computed: true, ElementType: types.StringType, MarkdownDescription: "Physical-network label → interface (e.g. `physnet1 = enp1s0`)."},
			"cluster_name":             schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "The cluster blueprint this config belongs to.", PlanModifiers: useState},
			"gpu_pci":                  schema.ListAttribute{Optional: true, Computed: true, ElementType: types.StringType, MarkdownDescription: "PCI addresses of GPUs to pass through."},
		},
	}
}

func (r *hostConfigResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.config = configureClient(req.ProviderData, &resp.Diagnostics)
}

func (r *hostConfigResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan hostConfigModel
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

	var created hostConfigAPI
	if _, err := client.Post(ctx, client.ServiceURL("hostconfigs"), body, &created, &gophercloud.RequestOpts{OkCodes: []int{200, 201, 202}}); err != nil {
		resp.Diagnostics.AddError("resmgr: creating host config", err.Error())
		return
	}
	if created.ID == "" {
		resp.Diagnostics.AddError("resmgr: creating host config", "the API did not return an id for the created host config")
		return
	}

	// Prefer the authoritative read-back, but never orphan a created config: if
	// the read fails, seed state from the create response so the resource is
	// still tracked and reconciles on the next refresh.
	if readDiags := r.readInto(ctx, client, created.ID, &plan); readDiags.HasError() {
		resp.Diagnostics.AddWarning("resmgr: host config created but read-back failed",
			"The host config was created; state is seeded from the create response and reconciles on the next refresh.")
		resp.Diagnostics.Append(r.fromAPI(ctx, &plan, &created)...)
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *hostConfigResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state hostConfigModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := r.config.ResmgrV2Client()
	if err != nil {
		resp.Diagnostics.AddError("resmgr: building client", err.Error())
		return
	}

	var hc hostConfigAPI
	if err := getJSON(ctx, client, client.ServiceURL("hostconfigs", state.ID.ValueString()), &hc); err != nil {
		if isNotFound(err) {
			resp.State.RemoveResource(ctx)
			return
		}
		resp.Diagnostics.AddError("resmgr: reading host config", err.Error())
		return
	}
	resp.Diagnostics.Append(r.fromAPI(ctx, &state, &hc)...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *hostConfigResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan hostConfigModel
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
	id := plan.ID.ValueString()
	if _, err := client.Put(ctx, client.ServiceURL("hostconfigs", id), body, nil, &gophercloud.RequestOpts{OkCodes: []int{200, 201, 202, 204}}); err != nil {
		resp.Diagnostics.AddError("resmgr: updating host config", err.Error())
		return
	}

	// On a read-back failure keep state consistent with the applied plan (which
	// is fully known) rather than reverting to the stale pre-update state.
	if readDiags := r.readInto(ctx, client, id, &plan); readDiags.HasError() {
		resp.Diagnostics.AddWarning("resmgr: host config updated but read-back failed",
			"The update was applied; state reflects the plan and reconciles on the next refresh.")
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *hostConfigResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state hostConfigModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := r.config.ResmgrV2Client()
	if err != nil {
		resp.Diagnostics.AddError("resmgr: building client", err.Error())
		return
	}

	// Deleting a host configuration a host is still assigned to cannot be undone, so it is
	// checked rather than attempted. resmgr accepts the delete, keeps the assignment, and
	// from then on refuses to remove it (404 — it validates that the target host config
	// exists before unbinding) and refuses every re-assignment (409
	// HostToHostconfigConflict). Deleting the host record does not clear it either, and a
	// host configuration cannot be re-created under the id that was removed: the host can
	// never be assigned one again, so it can never be onboarded again.
	//
	// The guard is deliberately fail-closed. If the check itself cannot be completed the
	// delete does not proceed — an unverified assumption here costs a hypervisor.
	id := state.ID.ValueString()
	assigned, err := hostsAssignedTo(ctx, client, id)
	if err != nil {
		resp.Diagnostics.AddError(
			"resmgr: cannot confirm the host configuration is unused",
			fmt.Sprintf("Host configuration %s was not deleted because the check for hosts still "+
				"assigned to it could not be completed: %s\n\nDeleting one while a host is still "+
				"assigned to it strands that host permanently, so Terraform will not do it on an "+
				"unverified answer. Retry when resmgr is reachable.", id, err),
		)
		return
	}
	if len(assigned) > 0 {
		resp.Diagnostics.AddError(
			"Host configuration is still assigned to a host",
			fmt.Sprintf("Host configuration %s is still assigned to %s, and deleting it now would "+
				"leave that host unable to be assigned any host configuration ever again: resmgr "+
				"would refuse both the unassign (404) and every re-assignment (409), with no way "+
				"back.\n\nRemove the assignment first, then delete the host configuration:\n"+
				"  - destroy the pcd_host_config_assignment resource that binds them, or\n"+
				"  - DELETE /resmgr/v2/hosts/<host_id>/hostconfig/%s if the assignment was made "+
				"outside Terraform, then confirm hostconfig_id has cleared in GET /resmgr/v2/hosts.\n\n"+
				"This guard is here because of a defect in PCD, not in your configuration, and will "+
				"be lifted once resmgr refuses the unsafe delete itself.",
				id, strings.Join(assigned, ", "), id),
		)
		return
	}

	if _, err := client.Delete(ctx, client.ServiceURL("hostconfigs", id), &gophercloud.RequestOpts{OkCodes: []int{200, 202, 204}}); err != nil {
		if isNotFound(err) {
			return
		}
		resp.Diagnostics.AddError("resmgr: deleting host config", err.Error())
	}
}

func (r *hostConfigResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, path.Root("id"), req, resp)
}

func (r *hostConfigResource) readInto(ctx context.Context, client *gophercloud.ServiceClient, id string, m *hostConfigModel) diag.Diagnostics {
	var hc hostConfigAPI
	if err := getJSON(ctx, client, client.ServiceURL("hostconfigs", id), &hc); err != nil {
		var d diag.Diagnostics
		d.AddError("resmgr: reading host config", err.Error())
		return d
	}
	return r.fromAPI(ctx, m, &hc)
}

func (r *hostConfigResource) toAPI(ctx context.Context, m *hostConfigModel, diags *diag.Diagnostics) hostConfigAPI {
	api := hostConfigAPI{
		Name:                   m.Name.ValueString(),
		MgmtInterface:          m.MgmtInterface.ValueString(),
		VMConsoleInterface:     m.VMConsoleInterface.ValueString(),
		HostLivenessInterface:  m.HostLivenessInterface.ValueString(),
		TunnelingInterface:     m.TunnelingInterface.ValueString(),
		ImagelibInterface:      m.ImagelibInterface.ValueString(),
		LiveMigrationInterface: m.LiveMigrationInterface.ValueString(),
		ClusterName:            m.ClusterName.ValueString(),
	}
	if !m.NetworkLabels.IsNull() && !m.NetworkLabels.IsUnknown() {
		labels := map[string]string{}
		diags.Append(m.NetworkLabels.ElementsAs(ctx, &labels, false)...)
		api.NetworkLabels = labels
	}
	if !m.GPUPci.IsNull() && !m.GPUPci.IsUnknown() {
		var gpus []string
		diags.Append(m.GPUPci.ElementsAs(ctx, &gpus, false)...)
		api.GPUPci = gpus
	}
	return api
}

func (r *hostConfigResource) fromAPI(ctx context.Context, m *hostConfigModel, hc *hostConfigAPI) diag.Diagnostics {
	var diags diag.Diagnostics
	m.ID = types.StringValue(hc.ID)
	m.Name = types.StringValue(hc.Name)
	m.MgmtInterface = types.StringValue(hc.MgmtInterface)
	m.VMConsoleInterface = types.StringValue(hc.VMConsoleInterface)
	m.HostLivenessInterface = types.StringValue(hc.HostLivenessInterface)
	m.TunnelingInterface = types.StringValue(hc.TunnelingInterface)
	m.ImagelibInterface = types.StringValue(hc.ImagelibInterface)
	m.LiveMigrationInterface = types.StringValue(hc.LiveMigrationInterface)
	m.ClusterName = types.StringValue(hc.ClusterName)

	labels := hc.NetworkLabels
	if labels == nil {
		labels = map[string]string{}
	}
	lv, d := types.MapValueFrom(ctx, types.StringType, labels)
	diags = append(diags, d...)
	m.NetworkLabels = lv

	gpus := hc.GPUPci
	if gpus == nil {
		gpus = []string{}
	}
	gv, d := types.ListValueFrom(ctx, types.StringType, gpus)
	diags = append(diags, d...)
	m.GPUPci = gv
	return diags
}
