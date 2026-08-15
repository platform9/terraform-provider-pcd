// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package resmgr

import (
	"context"
	"time"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/objectplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-framework/types/basetypes"

	"github.com/platform9/terraform-provider-pcd/internal/clients"
)

var (
	_ resource.Resource                = (*clusterResource)(nil)
	_ resource.ResourceWithConfigure   = (*clusterResource)(nil)
	_ resource.ResourceWithImportState = (*clusterResource)(nil)
)

// NewClusterResource is the factory registered with the provider.
func NewClusterResource() resource.Resource {
	return &clusterResource{}
}

type clusterResource struct {
	config *clients.Config
}

type clusterModel struct {
	Name                    types.String `tfsdk:"name"`
	VMHighAvailability      types.Object `tfsdk:"vm_high_availability"`
	AutoResourceRebalancing types.Object `tfsdk:"auto_resource_rebalancing"`
	GPU                     types.Object `tfsdk:"gpu"`
	CPU                     types.Object `tfsdk:"cpu"`
}

var clusterGPUAttrTypes = map[string]attr.Type{
	"enabled": types.BoolType,
	"mode":    types.StringType,
}

var clusterCPUAttrTypes = map[string]attr.Type{
	"mode":  types.StringType,
	"model": types.StringType,
}

// clusterAPI is the JSON body/response for /v2/clusters.
type clusterAPI struct {
	Name                    string         `json:"name"`
	VMHighAvailability      *haAPI         `json:"vmHighAvailability,omitempty"`
	AutoResourceRebalancing *rebalanceAPI  `json:"autoResourceRebalancing,omitempty"`
	GPU                     *clusterGPUAPI `json:"gpu,omitempty"`
	CPU                     *clusterCPUAPI `json:"cpu,omitempty"`
}

type clusterGPUAPI struct {
	Enabled bool   `json:"enabled"`
	Mode    string `json:"mode"`
}

type clusterCPUAPI struct {
	// Pointers: the API distinguishes "default" (null) from an explicit mode.
	Mode  *string `json:"mode"`
	Model *string `json:"model"`
}

func (r *clusterResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_cluster"
}

func (r *clusterResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	objUseState := []planmodifier.Object{objectplanmodifier.UseStateForUnknown()}
	resp.Schema = schema.Schema{
		MarkdownDescription: "Manages a PCD **cluster** (host cluster / host group): the unit hypervisors join. " +
			"Assigning a host the `hypervisor` cluster role requires one — `pcd_host_cluster_role.host_cluster` " +
			"names it. Carries the cluster-scoped VM high-availability, auto-rebalancing, GPU, and CPU-model settings.",
		Attributes: map[string]schema.Attribute{
			"name": schema.StringAttribute{Required: true, MarkdownDescription: "The cluster name. Changing this forces a new resource.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()}},
			"vm_high_availability": schema.SingleNestedAttribute{
				Optional: true, Computed: true, MarkdownDescription: "VM high-availability settings.",
				PlanModifiers: objUseState,
				Attributes: map[string]schema.Attribute{
					"enabled": schema.BoolAttribute{Optional: true, Computed: true, MarkdownDescription: "Auto-detect host failure and recover VMs."},
				},
			},
			"auto_resource_rebalancing": schema.SingleNestedAttribute{
				Optional: true, Computed: true, MarkdownDescription: "Automatic workload-rebalancing settings.",
				PlanModifiers: objUseState,
				Attributes: map[string]schema.Attribute{
					"enabled":                    schema.BoolAttribute{Optional: true, Computed: true, MarkdownDescription: "Whether auto-rebalancing is enabled."},
					"rebalancing_strategy":       schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "`vm_workload_consolidation` or `node_resource_consolidation`."},
					"rebalancing_frequency_mins": schema.Int64Attribute{Optional: true, Computed: true, MarkdownDescription: "Rebalancing frequency in minutes."},
				},
			},
			"gpu": schema.SingleNestedAttribute{
				Optional: true, Computed: true, MarkdownDescription: "GPU passthrough/virtualization settings.",
				PlanModifiers: objUseState,
				Attributes: map[string]schema.Attribute{
					"enabled": schema.BoolAttribute{Optional: true, Computed: true, MarkdownDescription: "Whether GPU support is enabled."},
					"mode":    schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "The GPU mode."},
				},
			},
			"cpu": schema.SingleNestedAttribute{
				Optional: true, Computed: true, MarkdownDescription: "Cluster CPU-model settings. Omit for PCD's default.",
				PlanModifiers: objUseState,
				Attributes: map[string]schema.Attribute{
					"mode":  schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "The CPU mode (e.g. `custom`); null means default."},
					"model": schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "The CPU model when `mode = \"custom\"`."},
				},
			},
		},
	}
}

func (r *clusterResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.config = configureClient(req.ProviderData, &resp.Diagnostics)
}

func (r *clusterResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan clusterModel
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
	// Cluster creation depends on the compute control plane being warm: on a
	// freshly deployed region the identical POST answers `500 Request Failed`
	// for the first several minutes and succeeds afterwards (the PCD UI
	// health-checks Nova before even offering the dialog). Retry 500s for a
	// bounded window so one-shot region bring-up works; a genuinely invalid
	// request still fails, just after the window.
	const window = 8 * time.Minute
	deadline := time.Now().Add(window)
	for {
		err := postJSON(ctx, client, client.ServiceURL("clusters"), body, nil)
		if err == nil {
			break
		}
		if !gophercloud.ResponseCodeIs(err, 500) || time.Now().After(deadline) {
			resp.Diagnostics.AddError("resmgr: creating cluster", err.Error())
			return
		}
		select {
		case <-ctx.Done():
			resp.Diagnostics.AddError("resmgr: creating cluster", ctx.Err().Error())
			return
		case <-time.After(15 * time.Second):
		}
	}

	if readDiags := r.readInto(ctx, client, plan.Name.ValueString(), &plan); readDiags.HasError() {
		resp.Diagnostics.AddWarning("resmgr: cluster created but read-back failed",
			"The cluster was created; state reconciles on the next refresh.")
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *clusterResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state clusterModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := r.config.ResmgrV2Client()
	if err != nil {
		resp.Diagnostics.AddError("resmgr: building client", err.Error())
		return
	}

	var c clusterAPI
	if err := getJSON(ctx, client, client.ServiceURL("clusters", state.Name.ValueString()), &c); err != nil {
		if isNotFound(err) {
			resp.State.RemoveResource(ctx)
			return
		}
		resp.Diagnostics.AddError("resmgr: reading cluster", err.Error())
		return
	}
	resp.Diagnostics.Append(r.setState(&state, &c)...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *clusterResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan clusterModel
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
	if err := putJSON(ctx, client, client.ServiceURL("clusters", plan.Name.ValueString()), body, nil); err != nil {
		resp.Diagnostics.AddError("resmgr: updating cluster", err.Error())
		return
	}

	if readDiags := r.readInto(ctx, client, plan.Name.ValueString(), &plan); readDiags.HasError() {
		resp.Diagnostics.AddWarning("resmgr: cluster updated but read-back failed",
			"The update was applied; state reconciles on the next refresh.")
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *clusterResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state clusterModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := r.config.ResmgrV2Client()
	if err != nil {
		resp.Diagnostics.AddError("resmgr: building client", err.Error())
		return
	}

	if _, err := client.Delete(ctx, client.ServiceURL("clusters", state.Name.ValueString()), &gophercloud.RequestOpts{OkCodes: []int{200, 202, 204}}); err != nil {
		if isNotFound(err) {
			return
		}
		resp.Diagnostics.AddError("resmgr: deleting cluster", err.Error())
	}
}

func (r *clusterResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, path.Root("name"), req, resp)
}

func (r *clusterResource) readInto(ctx context.Context, client *gophercloud.ServiceClient, name string, m *clusterModel) diag.Diagnostics {
	var c clusterAPI
	if err := getJSON(ctx, client, client.ServiceURL("clusters", name), &c); err != nil {
		var d diag.Diagnostics
		d.AddError("resmgr: reading cluster", err.Error())
		return d
	}
	return r.setState(m, &c)
}

func (r *clusterResource) setState(m *clusterModel, c *clusterAPI) diag.Diagnostics {
	var diags diag.Diagnostics
	m.Name = types.StringValue(c.Name)

	if c.VMHighAvailability != nil {
		obj, d := types.ObjectValue(haAttrTypes, map[string]attr.Value{
			"enabled": types.BoolValue(c.VMHighAvailability.Enabled),
		})
		diags.Append(d...)
		m.VMHighAvailability = obj
	} else {
		m.VMHighAvailability = types.ObjectNull(haAttrTypes)
	}

	if c.AutoResourceRebalancing != nil {
		obj, d := types.ObjectValue(rebalanceAttrTypes, map[string]attr.Value{
			"enabled":                    types.BoolValue(c.AutoResourceRebalancing.Enabled),
			"rebalancing_strategy":       types.StringValue(c.AutoResourceRebalancing.RebalancingStrategy),
			"rebalancing_frequency_mins": types.Int64Value(c.AutoResourceRebalancing.RebalancingFrequencyMins),
		})
		diags.Append(d...)
		m.AutoResourceRebalancing = obj
	} else {
		m.AutoResourceRebalancing = types.ObjectNull(rebalanceAttrTypes)
	}

	if c.GPU != nil {
		obj, d := types.ObjectValue(clusterGPUAttrTypes, map[string]attr.Value{
			"enabled": types.BoolValue(c.GPU.Enabled),
			"mode":    types.StringValue(c.GPU.Mode),
		})
		diags.Append(d...)
		m.GPU = obj
	} else {
		m.GPU = types.ObjectNull(clusterGPUAttrTypes)
	}

	if c.CPU != nil {
		mode := types.StringNull()
		if c.CPU.Mode != nil {
			mode = types.StringValue(*c.CPU.Mode)
		}
		model := types.StringNull()
		if c.CPU.Model != nil {
			model = types.StringValue(*c.CPU.Model)
		}
		obj, d := types.ObjectValue(clusterCPUAttrTypes, map[string]attr.Value{
			"mode":  mode,
			"model": model,
		})
		diags.Append(d...)
		m.CPU = obj
	} else {
		m.CPU = types.ObjectNull(clusterCPUAttrTypes)
	}
	return diags
}

// toAPI builds the request body. Every group is always sent: the create API
// requires the full object, and absent groups default to disabled.
func (r *clusterResource) toAPI(ctx context.Context, m *clusterModel, diags *diag.Diagnostics) *clusterAPI {
	c := &clusterAPI{
		Name:                    m.Name.ValueString(),
		VMHighAvailability:      &haAPI{},
		AutoResourceRebalancing: &rebalanceAPI{},
		GPU:                     &clusterGPUAPI{},
		CPU:                     &clusterCPUAPI{},
	}

	if !m.VMHighAvailability.IsNull() && !m.VMHighAvailability.IsUnknown() {
		var ha struct {
			Enabled types.Bool `tfsdk:"enabled"`
		}
		diags.Append(m.VMHighAvailability.As(ctx, &ha, basetypes.ObjectAsOptions{})...)
		c.VMHighAvailability = &haAPI{Enabled: ha.Enabled.ValueBool()}
	}

	if !m.AutoResourceRebalancing.IsNull() && !m.AutoResourceRebalancing.IsUnknown() {
		var rb struct {
			Enabled                  types.Bool   `tfsdk:"enabled"`
			RebalancingStrategy      types.String `tfsdk:"rebalancing_strategy"`
			RebalancingFrequencyMins types.Int64  `tfsdk:"rebalancing_frequency_mins"`
		}
		diags.Append(m.AutoResourceRebalancing.As(ctx, &rb, basetypes.ObjectAsOptions{})...)
		c.AutoResourceRebalancing = &rebalanceAPI{
			Enabled:                  rb.Enabled.ValueBool(),
			RebalancingStrategy:      rb.RebalancingStrategy.ValueString(),
			RebalancingFrequencyMins: rb.RebalancingFrequencyMins.ValueInt64(),
		}
	}

	if !m.GPU.IsNull() && !m.GPU.IsUnknown() {
		var g struct {
			Enabled types.Bool   `tfsdk:"enabled"`
			Mode    types.String `tfsdk:"mode"`
		}
		diags.Append(m.GPU.As(ctx, &g, basetypes.ObjectAsOptions{})...)
		c.GPU = &clusterGPUAPI{Enabled: g.Enabled.ValueBool(), Mode: g.Mode.ValueString()}
	}

	if !m.CPU.IsNull() && !m.CPU.IsUnknown() {
		var cp struct {
			Mode  types.String `tfsdk:"mode"`
			Model types.String `tfsdk:"model"`
		}
		diags.Append(m.CPU.As(ctx, &cp, basetypes.ObjectAsOptions{})...)
		cpu := &clusterCPUAPI{}
		if !cp.Mode.IsNull() {
			v := cp.Mode.ValueString()
			cpu.Mode = &v
		}
		if !cp.Model.IsNull() {
			v := cp.Model.ValueString()
			cpu.Model = &v
		}
		c.CPU = cpu
	}
	return c
}
