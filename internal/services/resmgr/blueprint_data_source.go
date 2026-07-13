// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package resmgr

import (
	"context"
	"encoding/json"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/platform9/terraform-provider-pcd/internal/clients"
)

var (
	_ datasource.DataSource              = (*blueprintDataSource)(nil)
	_ datasource.DataSourceWithConfigure = (*blueprintDataSource)(nil)
)

// NewBlueprintDataSource is the factory registered with the provider.
func NewBlueprintDataSource() datasource.DataSource {
	return &blueprintDataSource{}
}

type blueprintDataSource struct {
	config *clients.Config
}

type blueprintDataSourceModel struct {
	Name                      types.String `tfsdk:"name"`
	NetworkingType            types.String `tfsdk:"networking_type"`
	EnableDistributedRouting  types.Bool   `tfsdk:"enable_distributed_routing"`
	DNSDomainName             types.String `tfsdk:"dns_domain_name"`
	VirtualNetworking         types.Object `tfsdk:"virtual_networking"`
	ImageLibraryStorage       types.String `tfsdk:"image_library_storage"`
	ImageLibrarySharedStorage types.Bool   `tfsdk:"image_library_shared_storage"`
	InstanceSharedStorage     types.Bool   `tfsdk:"instance_shared_storage"`
	VMStorage                 types.String `tfsdk:"vm_storage"`
	StorageBackendsJSON       types.String `tfsdk:"storage_backends_json"`
}

// blueprintAPI is the JSON shape of a resmgr/v2 blueprint.
type blueprintAPI struct {
	Name                      string                `json:"name"`
	NetworkingType            string                `json:"networkingType"`
	EnableDistributedRouting  bool                  `json:"enableDistributedRouting"`
	DNSDomainName             string                `json:"dnsDomainName"`
	VirtualNetworking         *virtualNetworkingAPI `json:"virtualNetworking"`
	ImageLibraryStorage       string                `json:"imageLibraryStorage"`
	ImageLibrarySharedStorage bool                  `json:"imageLibrarySharedStorage"`
	InstanceSharedStorage     bool                  `json:"instanceSharedStorage"`
	VMStorage                 string                `json:"vmStorage"`
	StorageBackends           json.RawMessage       `json:"storageBackends"`
}

type virtualNetworkingAPI struct {
	Enabled      bool   `json:"enabled"`
	UnderlayType string `json:"underlayType"`
	VnidRange    string `json:"vnidRange"`
}

var virtualNetworkingAttrTypes = map[string]attr.Type{
	"enabled":       types.BoolType,
	"underlay_type": types.StringType,
	"vnid_range":    types.StringType,
}

func (d *blueprintDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_cluster_blueprint"
}

func (d *blueprintDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Reads a PCD cluster blueprint by name.",
		Attributes: map[string]schema.Attribute{
			"name":                       schema.StringAttribute{Required: true, MarkdownDescription: "The blueprint (cluster) name."},
			"networking_type":            schema.StringAttribute{Computed: true, MarkdownDescription: "The networking type (`ovn` or `ovs`)."},
			"enable_distributed_routing": schema.BoolAttribute{Computed: true, MarkdownDescription: "Whether distributed routing is enabled."},
			"dns_domain_name":            schema.StringAttribute{Computed: true, MarkdownDescription: "The internal DNS domain name for VMs."},
			"virtual_networking": schema.SingleNestedAttribute{
				Computed:            true,
				MarkdownDescription: "Virtual (tenant) networking settings.",
				Attributes: map[string]schema.Attribute{
					"enabled":       schema.BoolAttribute{Computed: true},
					"underlay_type": schema.StringAttribute{Computed: true},
					"vnid_range":    schema.StringAttribute{Computed: true},
				},
			},
			"image_library_storage":        schema.StringAttribute{Computed: true, MarkdownDescription: "The image library storage location."},
			"image_library_shared_storage": schema.BoolAttribute{Computed: true, MarkdownDescription: "Whether the image library uses shared storage."},
			"instance_shared_storage":      schema.BoolAttribute{Computed: true, MarkdownDescription: "Whether instance storage is shared."},
			"vm_storage":                   schema.StringAttribute{Computed: true, MarkdownDescription: "The VM ephemeral storage path."},
			"storage_backends_json":        schema.StringAttribute{Computed: true, Sensitive: true, MarkdownDescription: "The Cinder storage backends as a JSON string (contains credentials)."},
		},
	}
}

func (d *blueprintDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	d.config = configureClient(req.ProviderData, &resp.Diagnostics)
}

func (d *blueprintDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var data blueprintDataSourceModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := d.config.ResmgrV2Client()
	if err != nil {
		resp.Diagnostics.AddError("resmgr: building client", err.Error())
		return
	}

	var bp blueprintAPI
	if err := getJSON(ctx, client, client.ServiceURL("blueprint", data.Name.ValueString()), &bp); err != nil {
		resp.Diagnostics.AddError("resmgr: reading blueprint", err.Error())
		return
	}

	data.Name = types.StringValue(bp.Name)
	data.NetworkingType = types.StringValue(bp.NetworkingType)
	data.EnableDistributedRouting = types.BoolValue(bp.EnableDistributedRouting)
	data.DNSDomainName = types.StringValue(bp.DNSDomainName)
	data.ImageLibraryStorage = types.StringValue(bp.ImageLibraryStorage)
	data.ImageLibrarySharedStorage = types.BoolValue(bp.ImageLibrarySharedStorage)
	data.InstanceSharedStorage = types.BoolValue(bp.InstanceSharedStorage)
	data.VMStorage = types.StringValue(bp.VMStorage)

	if bp.VirtualNetworking != nil {
		obj, diags := types.ObjectValue(virtualNetworkingAttrTypes, map[string]attr.Value{
			"enabled":       types.BoolValue(bp.VirtualNetworking.Enabled),
			"underlay_type": types.StringValue(bp.VirtualNetworking.UnderlayType),
			"vnid_range":    types.StringValue(bp.VirtualNetworking.VnidRange),
		})
		resp.Diagnostics.Append(diags...)
		data.VirtualNetworking = obj
	} else {
		data.VirtualNetworking = types.ObjectNull(virtualNetworkingAttrTypes)
	}

	if len(bp.StorageBackends) > 0 && string(bp.StorageBackends) != "null" {
		data.StorageBackendsJSON = types.StringValue(string(bp.StorageBackends))
	} else {
		data.StorageBackendsJSON = types.StringNull()
	}

	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}
