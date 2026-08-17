// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0
//
// Ported from terraform-provider-openstack v3.4.0
// (openstack/resource_openstack_compute_instance_v2.go), adapted for the
// terraform-plugin-framework and PCD. This first cut covers boot-from-image with
// networks, security groups, keypair, metadata, user_data, and config drive.

package compute

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/flavors"
	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/keypairs"
	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/servers"
	"github.com/gophercloud/gophercloud/v2/openstack/image/v2/images"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/listplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/mapplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/setplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/platform9/terraform-provider-pcd/internal/clients"
)

var (
	_ resource.Resource                   = (*instanceResource)(nil)
	_ resource.ResourceWithConfigure      = (*instanceResource)(nil)
	_ resource.ResourceWithImportState    = (*instanceResource)(nil)
	_ resource.ResourceWithValidateConfig = (*instanceResource)(nil)
)

// migrationPriorityKey is the Nova server-metadata key PCD's Dynamic Resource
// Rebalancing (DRR) service reads to decide whether — and how eagerly — a VM
// may be live-migrated. The PCD UI's "Set Migration Priority" dialog writes
// exactly this key. The provider exposes it as a first-class attribute and
// keeps it out of the user-facing `metadata` map so the two never conflict.
const migrationPriorityKey = "migration-priority"

// migrationPriorities are the values the DRR service understands, as the PCD
// UI offers them: Normal, Low, High, Excluded ("never").
var migrationPriorities = map[string]bool{"normal": true, "low": true, "high": true, "never": true}

// splitMigrationPriority removes the reserved key from server metadata,
// returning the remaining user metadata and the priority ("" when unset).
func splitMigrationPriority(serverMeta map[string]string) (map[string]string, string) {
	meta := make(map[string]string, len(serverMeta))
	prio := ""
	for k, v := range serverMeta {
		if k == migrationPriorityKey {
			prio = v
			continue
		}
		meta[k] = v
	}
	return meta, prio
}

// blockDevicesFromList converts the block_device blocks into Nova
// block_device_mapping_v2 entries and reports whether one of them is the root
// disk (boot_index 0), in which case image_id/image_name are not required.
// Defaults follow openstack_compute_instance_v2 and the PCD UI: destination
// "volume", boot_index -1 (non-bootable), delete_on_termination false.
func blockDevicesFromList(ctx context.Context, l types.List, diags *diag.Diagnostics) ([]servers.BlockDevice, bool) {
	if l.IsNull() || l.IsUnknown() || len(l.Elements()) == 0 {
		return nil, false
	}
	var models []instanceBlockDeviceModel
	diags.Append(l.ElementsAs(ctx, &models, false)...)
	if diags.HasError() {
		return nil, false
	}
	out := make([]servers.BlockDevice, 0, len(models))
	hasRoot := false
	for i, m := range models {
		bd := servers.BlockDevice{
			SourceType:      servers.SourceType(m.SourceType.ValueString()),
			UUID:            m.UUID.ValueString(),
			BootIndex:       -1,
			DestinationType: servers.DestinationVolume,
			VolumeType:      m.VolumeType.ValueString(),
			GuestFormat:     m.GuestFormat.ValueString(),
			DeviceType:      m.DeviceType.ValueString(),
			DiskBus:         m.DiskBus.ValueString(),
		}
		if !m.BootIndex.IsNull() && !m.BootIndex.IsUnknown() {
			bd.BootIndex = int(m.BootIndex.ValueInt64())
		}
		if !m.DestinationType.IsNull() && m.DestinationType.ValueString() != "" {
			bd.DestinationType = servers.DestinationType(m.DestinationType.ValueString())
		}
		if !m.VolumeSize.IsNull() && !m.VolumeSize.IsUnknown() {
			bd.VolumeSize = int(m.VolumeSize.ValueInt64())
		}
		if !m.DeleteOnTermination.IsNull() && !m.DeleteOnTermination.IsUnknown() {
			bd.DeleteOnTermination = m.DeleteOnTermination.ValueBool()
		}
		switch bd.SourceType {
		case servers.SourceImage, servers.SourceVolume, servers.SourceSnapshot, servers.SourceBlank:
		default:
			diags.AddAttributeError(path.Root("block_device").AtListIndex(i).AtName("source_type"),
				"Invalid block_device source_type",
				fmt.Sprintf("%q is not valid; use image, volume, snapshot, or blank.", m.SourceType.ValueString()))
			return nil, false
		}
		if bd.SourceType != servers.SourceBlank && bd.UUID == "" {
			diags.AddAttributeError(path.Root("block_device").AtListIndex(i).AtName("uuid"),
				"block_device uuid is required",
				fmt.Sprintf("source_type %q needs the uuid of the source %s.", bd.SourceType, bd.SourceType))
			return nil, false
		}
		if bd.SourceType == servers.SourceBlank && bd.VolumeSize == 0 {
			diags.AddAttributeError(path.Root("block_device").AtListIndex(i).AtName("volume_size"),
				"block_device volume_size is required",
				"A blank block device needs a volume_size (GiB).")
			return nil, false
		}
		if bd.BootIndex == 0 {
			hasRoot = true
		}
		out = append(out, bd)
	}
	return out, hasRoot
}

// NewInstanceResource is the factory registered with the provider.
func NewInstanceResource() resource.Resource {
	return &instanceResource{}
}

type instanceResource struct {
	config *clients.Config
}

type instanceModel struct {
	ID                types.String `tfsdk:"id"`
	Name              types.String `tfsdk:"name"`
	ImageID           types.String `tfsdk:"image_id"`
	ImageName         types.String `tfsdk:"image_name"`
	FlavorID          types.String `tfsdk:"flavor_id"`
	FlavorName        types.String `tfsdk:"flavor_name"`
	KeyPair           types.String `tfsdk:"key_pair"`
	SecurityGroups    types.Set    `tfsdk:"security_groups"`
	Network           types.List   `tfsdk:"network"`
	BlockDevice       types.List   `tfsdk:"block_device"`
	Metadata          types.Map    `tfsdk:"metadata"`
	MigrationPriority types.String `tfsdk:"migration_priority"`
	UserData          types.String `tfsdk:"user_data"`
	AvailabilityZone  types.String `tfsdk:"availability_zone"`
	ConfigDrive       types.Bool   `tfsdk:"config_drive"`
	AccessIPv4        types.String `tfsdk:"access_ip_v4"`
	Status            types.String `tfsdk:"status"`
	Region            types.String `tfsdk:"region"`
}

type instanceNetworkModel struct {
	UUID types.String `tfsdk:"uuid"`
	Name types.String `tfsdk:"name"`
	Port types.String `tfsdk:"port"`
}

// instanceBlockDeviceModel mirrors openstack_compute_instance_v2's block_device
// block so configurations port across unchanged. Each entry is one
// block_device_mapping_v2 element on the Nova create request.
type instanceBlockDeviceModel struct {
	SourceType          types.String `tfsdk:"source_type"`
	UUID                types.String `tfsdk:"uuid"`
	VolumeSize          types.Int64  `tfsdk:"volume_size"`
	DestinationType     types.String `tfsdk:"destination_type"`
	BootIndex           types.Int64  `tfsdk:"boot_index"`
	DeleteOnTermination types.Bool   `tfsdk:"delete_on_termination"`
	VolumeType          types.String `tfsdk:"volume_type"`
	GuestFormat         types.String `tfsdk:"guest_format"`
	DeviceType          types.String `tfsdk:"device_type"`
	DiskBus             types.String `tfsdk:"disk_bus"`
}

func (r *instanceResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_compute_instance"
}

func (r *instanceResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	fn := []planmodifier.String{stringplanmodifier.RequiresReplace()}
	fnC := []planmodifier.String{stringplanmodifier.RequiresReplace(), stringplanmodifier.UseStateForUnknown()}
	stable := []planmodifier.String{stringplanmodifier.UseStateForUnknown()}
	resp.Schema = schema.Schema{
		MarkdownDescription: "Manages a compute instance (server) in PCD's Nova service.",
		Attributes: map[string]schema.Attribute{
			"id":         schema.StringAttribute{Computed: true, MarkdownDescription: "The instance ID.", PlanModifiers: stable},
			"name":       schema.StringAttribute{Required: true, MarkdownDescription: "The name of the instance."},
			"image_id":   schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "The image ID to boot from (alternative to image_name). Required unless a `block_device` with `boot_index = 0` supplies the boot disk. Changing this forces a new resource.", PlanModifiers: fnC},
			"image_name": schema.StringAttribute{Optional: true, MarkdownDescription: "The image name to boot from, resolved via Glance (alternative to image_id). Required unless a `block_device` with `boot_index = 0` supplies the boot disk. Changing this forces a new resource.", PlanModifiers: fn},
			// No UseStateForUnknown: it would pin the stale flavor_id into the plan
			// when a resize is driven by flavor_name, causing an "inconsistent result"
			// error. Update always sets the resolved flavor_id explicitly.
			"flavor_id":   schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "The flavor ID (alternative to flavor_name). Changing this triggers an in-place resize.", PlanModifiers: []planmodifier.String{}},
			"flavor_name": schema.StringAttribute{Optional: true, MarkdownDescription: "The flavor name (alternative to flavor_id). Changing this triggers an in-place resize.", PlanModifiers: []planmodifier.String{}},
			"key_pair":    schema.StringAttribute{Optional: true, MarkdownDescription: "The name of a keypair to inject. Changing this forces a new resource.", PlanModifiers: fn},
			"security_groups": schema.SetAttribute{
				Optional: true, Computed: true, ElementType: types.StringType,
				MarkdownDescription: "Names of security groups to associate. Changing this forces a new resource.",
				PlanModifiers:       []planmodifier.Set{setplanmodifier.RequiresReplace(), setplanmodifier.UseStateForUnknown()},
			},
			"metadata": schema.MapAttribute{Optional: true, Computed: true, ElementType: types.StringType, MarkdownDescription: "Key-value metadata attached to the instance. The key `migration-priority` is reserved — set it through `migration_priority` instead.", PlanModifiers: []planmodifier.Map{mapplanmodifier.UseStateForUnknown()}},
			"migration_priority": schema.StringAttribute{Optional: true, Computed: true,
				MarkdownDescription: "How PCD's Dynamic Resource Rebalancing (DRR) service treats this VM when balancing hosts: " +
					"`normal`, `low`, `high`, or `never` (excluded from automatic migration). Unset means DRR's default. " +
					"Stored as the `migration-priority` server metadata key, exactly as the PCD UI's Set Migration Priority " +
					"dialog does. Updatable in place; set `\"\"` to clear.",
				PlanModifiers: stable},
			"user_data":         schema.StringAttribute{Optional: true, MarkdownDescription: "User data (cloud-init) for the instance. Changing this forces a new resource.", PlanModifiers: fn},
			"availability_zone": schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "Availability zone to launch in. Changing this forces a new resource.", PlanModifiers: fnC},
			"config_drive":      schema.BoolAttribute{Optional: true, MarkdownDescription: "Whether to use a config drive. Changing this forces a new resource.", PlanModifiers: []planmodifier.Bool{}},
			"access_ip_v4":      schema.StringAttribute{Computed: true, MarkdownDescription: "The first IPv4 address of the instance.", PlanModifiers: stable},
			"status":            schema.StringAttribute{Computed: true, MarkdownDescription: "The Nova status (e.g. ACTIVE)."},
			"region":            schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "The region. Defaults to the provider's region.", PlanModifiers: stable},
		},
		Blocks: map[string]schema.Block{
			"network": schema.ListNestedBlock{
				MarkdownDescription: "Networks to attach. Changing this forces a new resource.",
				NestedObject: schema.NestedBlockObject{Attributes: map[string]schema.Attribute{
					"uuid": schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "Network UUID to attach to (required unless port is set).", PlanModifiers: stable},
					"name": schema.StringAttribute{Optional: true, MarkdownDescription: "Network name (informational)."},
					"port": schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "Existing port to attach (required unless uuid is set).", PlanModifiers: stable},
				}},
				PlanModifiers: []planmodifier.List{listplanmodifier.RequiresReplace(), listplanmodifier.UseStateForUnknown()},
			},
			"block_device": schema.ListNestedBlock{
				MarkdownDescription: "Block devices to create the instance with (Nova `block_device_mapping_v2`), one block per device. " +
					"Mirrors `openstack_compute_instance_v2`. Use it to boot from a new volume, an existing volume, or a volume " +
					"snapshot, to install from an ISO, or to attach extra disks at boot. The device with `boot_index = 0` is the " +
					"root disk; when one is present `image_id`/`image_name` may be omitted. Create-only: changing this forces a new " +
					"resource. Attach/detach volumes on a running instance with `pcd_compute_volume_attach` instead.",
				NestedObject: schema.NestedBlockObject{Attributes: map[string]schema.Attribute{
					"source_type":           schema.StringAttribute{Required: true, MarkdownDescription: "The source of the device: `image`, `volume`, `snapshot`, or `blank`."},
					"uuid":                  schema.StringAttribute{Optional: true, MarkdownDescription: "The ID of the source image, volume, or snapshot. Not used with `source_type = \"blank\"`."},
					"volume_size":           schema.Int64Attribute{Optional: true, MarkdownDescription: "Size in GiB of the volume to create. Required for `blank`; for `image`/`snapshot` sources it must be at least the source size."},
					"destination_type":      schema.StringAttribute{Optional: true, MarkdownDescription: "Where the device lives: `volume` (Cinder-backed, persistent) or `local` (ephemeral on the hypervisor). Defaults to `volume`."},
					"boot_index":            schema.Int64Attribute{Optional: true, MarkdownDescription: "Boot order. `0` is the root disk, `1` the next device (e.g. an installer ISO), `-1` for a non-bootable data disk. Defaults to `-1`."},
					"delete_on_termination": schema.BoolAttribute{Optional: true, MarkdownDescription: "Delete the created volume when the instance is deleted. Defaults to `false`."},
					"volume_type":           schema.StringAttribute{Optional: true, MarkdownDescription: "The Cinder volume type for a created volume."},
					"guest_format":          schema.StringAttribute{Optional: true, MarkdownDescription: "Filesystem format for a `blank` device (e.g. `ext4`, `swap`)."},
					"device_type":           schema.StringAttribute{Optional: true, MarkdownDescription: "The device type: `disk` (default) or `cdrom` (for an installer ISO)."},
					"disk_bus":              schema.StringAttribute{Optional: true, MarkdownDescription: "The bus to attach the device on (e.g. `virtio`, `scsi`, `ide`)."},
				}},
				PlanModifiers: []planmodifier.List{listplanmodifier.RequiresReplace()},
			},
		},
	}
}

func (r *instanceResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.config = configureClient(req.ProviderData, &resp.Diagnostics)
}

// ValidateConfig surfaces value errors at plan time rather than apply time.
func (r *instanceResource) ValidateConfig(ctx context.Context, req resource.ValidateConfigRequest, resp *resource.ValidateConfigResponse) {
	var cfg instanceModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &cfg)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if !cfg.MigrationPriority.IsNull() && !cfg.MigrationPriority.IsUnknown() {
		if p := cfg.MigrationPriority.ValueString(); p != "" && !migrationPriorities[p] {
			resp.Diagnostics.AddAttributeError(path.Root("migration_priority"), "Invalid migration_priority",
				fmt.Sprintf("%q is not valid; use normal, low, high, or never (or \"\" to clear).", p))
		}
	}
	if !cfg.Metadata.IsNull() && !cfg.Metadata.IsUnknown() {
		var meta map[string]string
		resp.Diagnostics.Append(cfg.Metadata.ElementsAs(ctx, &meta, false)...)
		if _, reserved := meta[migrationPriorityKey]; reserved {
			resp.Diagnostics.AddAttributeError(path.Root("metadata"), "Reserved metadata key",
				fmt.Sprintf("%q is managed by the migration_priority attribute; set it there instead.", migrationPriorityKey))
		}
	}
}

func (r *instanceResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan instanceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := r.config.ComputeV2Client()
	if err != nil {
		resp.Diagnostics.AddError("compute: building v2 client", err.Error())
		return
	}

	flavorID := plan.FlavorID.ValueString()
	if flavorID == "" {
		flavorID, err = flavorIDFromName(ctx, client, plan.FlavorName.ValueString())
		if err != nil {
			resp.Diagnostics.AddError("compute: resolving flavor", err.Error())
			return
		}
	}

	blockDevices, hasRootBlockDevice := blockDevicesFromList(ctx, plan.BlockDevice, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}

	imageNameSet := !plan.ImageName.IsNull() && plan.ImageName.ValueString() != ""
	imageIDSet := !plan.ImageID.IsNull() && !plan.ImageID.IsUnknown() && plan.ImageID.ValueString() != ""
	switch {
	case imageNameSet && imageIDSet:
		resp.Diagnostics.AddError("Invalid image", "Set only one of image_id or image_name.")
		return
	case !imageNameSet && !imageIDSet && !hasRootBlockDevice:
		resp.Diagnostics.AddError("Invalid image",
			"Set image_id or image_name, or supply the boot disk with a block_device that has boot_index = 0.")
		return
	}
	imageID := plan.ImageID.ValueString()
	if imageNameSet {
		imgClient, err := r.config.ImageV2Client()
		if err != nil {
			resp.Diagnostics.AddError("compute: building image v2 client", err.Error())
			return
		}
		imageID, err = imageIDFromName(ctx, imgClient, plan.ImageName.ValueString())
		if err != nil {
			resp.Diagnostics.AddError("compute: resolving image", err.Error())
			return
		}
	}

	var sgs []string
	if !plan.SecurityGroups.IsNull() && !plan.SecurityGroups.IsUnknown() {
		resp.Diagnostics.Append(plan.SecurityGroups.ElementsAs(ctx, &sgs, false)...)
	}
	var meta map[string]string
	if !plan.Metadata.IsNull() && !plan.Metadata.IsUnknown() {
		resp.Diagnostics.Append(plan.Metadata.ElementsAs(ctx, &meta, false)...)
	}
	if _, reserved := meta[migrationPriorityKey]; reserved {
		resp.Diagnostics.AddAttributeError(path.Root("metadata"), "Reserved metadata key",
			fmt.Sprintf("%q is managed by the migration_priority attribute; set it there instead.", migrationPriorityKey))
		return
	}
	if p := plan.MigrationPriority.ValueString(); !plan.MigrationPriority.IsNull() && !plan.MigrationPriority.IsUnknown() && p != "" {
		if !migrationPriorities[p] {
			resp.Diagnostics.AddAttributeError(path.Root("migration_priority"), "Invalid migration_priority",
				fmt.Sprintf("%q is not valid; use normal, low, high, or never.", p))
			return
		}
		if meta == nil {
			meta = map[string]string{}
		}
		meta[migrationPriorityKey] = p
	}
	nets := networksFromList(ctx, plan.Network, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}

	baseOpts := servers.CreateOpts{
		Name:             plan.Name.ValueString(),
		ImageRef:         imageID,
		FlavorRef:        flavorID,
		SecurityGroups:   sgs,
		Metadata:         meta,
		AvailabilityZone: plan.AvailabilityZone.ValueString(),
		Networks:         nets,
		BlockDevice:      blockDevices,
	}
	if v := plan.UserData.ValueString(); v != "" {
		baseOpts.UserData = []byte(v)
	}
	if !plan.ConfigDrive.IsNull() {
		cd := plan.ConfigDrive.ValueBool()
		baseOpts.ConfigDrive = &cd
	}

	var createOpts servers.CreateOptsBuilder = baseOpts
	if kp := plan.KeyPair.ValueString(); kp != "" {
		createOpts = keypairs.CreateOptsExt{CreateOptsBuilder: baseOpts, KeyName: kp}
	}

	// block_device_mapping_v2.volume_type is only accepted at compute API
	// microversion >= 2.67; the provider otherwise speaks 2.1. Negotiate the
	// higher version for the create call alone (a copy of the client, so read
	// paths keep the 2.1 response shapes they were written against — >= 2.47
	// embeds the full flavor in the server body, for instance). PCD 2026.4
	// advertises up to 2.100.
	createClient := *client
	if len(blockDevices) > 0 {
		createClient.Microversion = "2.67"
	}
	server, err := servers.Create(ctx, &createClient, createOpts, nil).Extract()
	if err != nil {
		resp.Diagnostics.AddError("compute: creating instance", err.Error())
		return
	}

	server, err = waitForServerActive(ctx, client, server.ID, 30*time.Minute)
	if err != nil {
		resp.Diagnostics.AddError("compute: waiting for instance to become active", err.Error())
		return
	}

	plan.FlavorID = types.StringValue(flavorID)
	// A boot-from-volume instance has no image; Nova reports image "" and so
	// does state, which is what an unset image_id must round-trip to.
	plan.ImageID = types.StringValue(imageID)
	resp.Diagnostics.Append(r.flatten(ctx, server, &plan)...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *instanceResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state instanceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := r.config.ComputeV2Client()
	if err != nil {
		resp.Diagnostics.AddError("compute: building v2 client", err.Error())
		return
	}

	server, err := servers.Get(ctx, client, state.ID.ValueString()).Extract()
	if err != nil {
		if gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
			resp.Diagnostics.AddWarning("Instance not found",
				fmt.Sprintf("Instance %s no longer exists and was removed from state.", state.ID.ValueString()))
			resp.State.RemoveResource(ctx)
			return
		}
		resp.Diagnostics.AddError("compute: reading instance", err.Error())
		return
	}

	resp.Diagnostics.Append(r.flatten(ctx, server, &state)...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *instanceResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state instanceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := r.config.ComputeV2Client()
	if err != nil {
		resp.Diagnostics.AddError("compute: building v2 client", err.Error())
		return
	}

	if !plan.Name.Equal(state.Name) {
		if _, err := servers.Update(ctx, client, plan.ID.ValueString(), servers.UpdateOpts{Name: plan.Name.ValueString()}).Extract(); err != nil {
			resp.Diagnostics.AddError("compute: renaming instance", err.Error())
			return
		}
	}

	// Only push metadata when the user actually set it and it changed. A null or
	// unknown plan value means "unmanaged" — Nova rejects a nil metadata map
	// (metadata: None) with a 400.
	metaChanged := !plan.Metadata.IsNull() && !plan.Metadata.IsUnknown() && !plan.Metadata.Equal(state.Metadata)
	prioChanged := !plan.MigrationPriority.IsUnknown() && !plan.MigrationPriority.Equal(state.MigrationPriority)
	if metaChanged || prioChanged {
		meta := map[string]string{}
		if !plan.Metadata.IsNull() && !plan.Metadata.IsUnknown() {
			resp.Diagnostics.Append(plan.Metadata.ElementsAs(ctx, &meta, false)...)
			if resp.Diagnostics.HasError() {
				return
			}
		}
		if _, reserved := meta[migrationPriorityKey]; reserved {
			resp.Diagnostics.AddAttributeError(path.Root("metadata"), "Reserved metadata key",
				fmt.Sprintf("%q is managed by the migration_priority attribute; set it there instead.", migrationPriorityKey))
			return
		}
		prio := ""
		if !plan.MigrationPriority.IsNull() && !plan.MigrationPriority.IsUnknown() {
			prio = plan.MigrationPriority.ValueString()
		}
		if prio != "" {
			if !migrationPriorities[prio] {
				resp.Diagnostics.AddAttributeError(path.Root("migration_priority"), "Invalid migration_priority",
					fmt.Sprintf("%q is not valid; use normal, low, high, or never.", prio))
				return
			}
			meta[migrationPriorityKey] = prio
		}
		// UpdateMetadata merges keys; it never removes one. Clearing the priority
		// (or a user key that vanished from the map) needs an explicit delete.
		if len(meta) > 0 {
			if _, err := servers.UpdateMetadata(ctx, client, plan.ID.ValueString(), servers.MetadataOpts(meta)).Extract(); err != nil {
				resp.Diagnostics.AddError("compute: updating instance metadata", err.Error())
				return
			}
		}
		if prio == "" && prioChanged && state.MigrationPriority.ValueString() != "" {
			if err := servers.DeleteMetadatum(ctx, client, plan.ID.ValueString(), migrationPriorityKey).ExtractErr(); err != nil && !gophercloud.ResponseCodeIs(err, 404) {
				resp.Diagnostics.AddError("compute: clearing migration_priority", err.Error())
				return
			}
		}
		if metaChanged && !state.Metadata.IsNull() {
			var prev map[string]string
			resp.Diagnostics.Append(state.Metadata.ElementsAs(ctx, &prev, false)...)
			for k := range prev {
				if _, still := meta[k]; !still && k != migrationPriorityKey {
					if err := servers.DeleteMetadatum(ctx, client, plan.ID.ValueString(), k).ExtractErr(); err != nil && !gophercloud.ResponseCodeIs(err, 404) {
						resp.Diagnostics.AddError("compute: removing instance metadata key "+k, err.Error())
						return
					}
				}
			}
		}
	}

	// Resize on flavor change. Resolve the target flavor from flavor_name when the
	// user configures a name (flavor_id is Computed and would carry the stale id via
	// UseStateForUnknown), otherwise from flavor_id.
	id := plan.ID.ValueString()
	// Resolve the intended flavor. Prefer flavor_name when configured: flavor_id is
	// Computed and may be unknown in the plan, so it can't be trusted here.
	targetFlavorID := plan.FlavorID.ValueString()
	if name := plan.FlavorName.ValueString(); name != "" {
		targetFlavorID, err = flavorIDFromName(ctx, client, name)
		if err != nil {
			resp.Diagnostics.AddError("compute: resolving flavor", err.Error())
			return
		}
	}
	if targetFlavorID == "" {
		targetFlavorID = state.FlavorID.ValueString()
	}
	if targetFlavorID != state.FlavorID.ValueString() {
		if err := servers.Resize(ctx, client, id, servers.ResizeOpts{FlavorRef: targetFlavorID}).ExtractErr(); err != nil {
			resp.Diagnostics.AddError("compute: resizing instance", err.Error())
			return
		}
		if _, err := waitForServerStatus(ctx, client, id, "VERIFY_RESIZE", 30*time.Minute); err != nil {
			// Best-effort revert so the instance returns to its original flavor.
			_ = servers.RevertResize(ctx, client, id).ExtractErr()
			_, _ = waitForServerActive(ctx, client, id, 30*time.Minute)
			resp.Diagnostics.AddError("compute: waiting for resize to verify", err.Error())
			return
		}
		if err := servers.ConfirmResize(ctx, client, id).ExtractErr(); err != nil {
			_ = servers.RevertResize(ctx, client, id).ExtractErr()
			_, _ = waitForServerActive(ctx, client, id, 30*time.Minute)
			resp.Diagnostics.AddError("compute: confirming resize", err.Error())
			return
		}
		if _, err := waitForServerActive(ctx, client, id, 30*time.Minute); err != nil {
			resp.Diagnostics.AddError("compute: waiting for instance active after resize", err.Error())
			return
		}
	}
	// Always pin the resolved flavor_id (it may be unknown in the plan when driven
	// by flavor_name), so the applied state matches what Terraform expects.
	plan.FlavorID = types.StringValue(targetFlavorID)

	server, err := servers.Get(ctx, client, plan.ID.ValueString()).Extract()
	if err != nil {
		resp.Diagnostics.AddError("compute: reading instance after update", err.Error())
		return
	}
	resp.Diagnostics.Append(r.flatten(ctx, server, &plan)...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *instanceResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state instanceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := r.config.ComputeV2Client()
	if err != nil {
		resp.Diagnostics.AddError("compute: building v2 client", err.Error())
		return
	}

	if err := servers.Delete(ctx, client, state.ID.ValueString()).ExtractErr(); err != nil {
		if gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
			return
		}
		resp.Diagnostics.AddError("compute: deleting instance", err.Error())
		return
	}
	if err := waitForServerDeleted(ctx, client, state.ID.ValueString(), 15*time.Minute); err != nil {
		resp.Diagnostics.AddError("compute: waiting for instance deletion", err.Error())
	}
}

func (r *instanceResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, path.Root("id"), req, resp)
}

// flatten updates the server-derived attributes; ForceNew inputs (image, flavor
// name, key_pair, network, user_data, ...) are preserved from the plan/state.
func (r *instanceResource) flatten(ctx context.Context, server *servers.Server, m *instanceModel) (diags diag.Diagnostics) {
	m.ID = types.StringValue(server.ID)
	m.Name = types.StringValue(server.Name)
	m.Status = types.StringValue(server.Status)

	ip := server.AccessIPv4
	if ip == "" {
		ip = firstIPv4(server.Addresses)
	}
	m.AccessIPv4 = types.StringValue(ip)

	meta, prio := splitMigrationPriority(server.Metadata)
	metaMap, d := types.MapValueFrom(ctx, types.StringType, meta)
	diags = append(diags, d...)
	m.Metadata = metaMap
	m.MigrationPriority = types.StringValue(prio)

	// availability_zone and security_groups are Optional+Computed: when the user
	// omits them the server assigns values, which must be read back or they stay
	// unknown after apply ("invalid result object").
	m.AvailabilityZone = types.StringValue(server.AvailabilityZone)

	seenSG := make(map[string]bool, len(server.SecurityGroups))
	sgs := make([]string, 0, len(server.SecurityGroups))
	for _, sg := range server.SecurityGroups {
		// Nova can list the same security group once per attached port; dedupe so
		// the Set is valid.
		if name, ok := sg["name"].(string); ok && !seenSG[name] {
			seenSG[name] = true
			sgs = append(sgs, name)
		}
	}
	sgSet, d := types.SetValueFrom(ctx, types.StringType, sgs)
	diags = append(diags, d...)
	m.SecurityGroups = sgSet

	// The network block's uuid/port are Optional+Computed; resolve any that the
	// user left unset (unknown) to a concrete value so the block is fully known.
	if !m.Network.IsNull() && !m.Network.IsUnknown() {
		var blocks []instanceNetworkModel
		diags = append(diags, m.Network.ElementsAs(ctx, &blocks, false)...)
		for i := range blocks {
			if blocks[i].UUID.IsNull() || blocks[i].UUID.IsUnknown() {
				blocks[i].UUID = types.StringValue("")
			}
			if blocks[i].Port.IsNull() || blocks[i].Port.IsUnknown() {
				blocks[i].Port = types.StringValue("")
			}
		}
		netList, d := types.ListValueFrom(ctx, m.Network.ElementType(ctx), blocks)
		diags = append(diags, d...)
		m.Network = netList
	}

	if m.Region.IsNull() || m.Region.IsUnknown() {
		m.Region = types.StringValue(r.config.Region)
	}
	return diags
}

func networksFromList(ctx context.Context, l types.List, diags *diag.Diagnostics) []servers.Network {
	if l.IsNull() || l.IsUnknown() {
		return nil
	}
	var blocks []instanceNetworkModel
	diags.Append(l.ElementsAs(ctx, &blocks, false)...)
	out := make([]servers.Network, 0, len(blocks))
	for _, b := range blocks {
		out = append(out, servers.Network{UUID: b.UUID.ValueString(), Port: b.Port.ValueString()})
	}
	return out
}

func flavorIDFromName(ctx context.Context, client *gophercloud.ServiceClient, name string) (string, error) {
	if name == "" {
		return "", fmt.Errorf("either flavor_id or flavor_name must be set")
	}
	pages, err := flavors.ListDetail(client, flavors.ListOpts{}).AllPages(ctx)
	if err != nil {
		return "", err
	}
	all, err := flavors.ExtractFlavors(pages)
	if err != nil {
		return "", err
	}
	for _, f := range all {
		if f.Name == name {
			return f.ID, nil
		}
	}
	return "", fmt.Errorf("no flavor named %q", name)
}

func imageIDFromName(ctx context.Context, client *gophercloud.ServiceClient, name string) (string, error) {
	pages, err := images.List(client, images.ListOpts{Name: name}).AllPages(ctx)
	if err != nil {
		return "", err
	}
	all, err := images.ExtractImages(pages)
	if err != nil {
		return "", err
	}
	var matches []images.Image
	for _, im := range all {
		if im.Name == name {
			matches = append(matches, im)
		}
	}
	switch len(matches) {
	case 0:
		return "", fmt.Errorf("no image named %q", name)
	case 1:
		return matches[0].ID, nil
	default:
		return "", fmt.Errorf("%d images named %q; use image_id to disambiguate", len(matches), name)
	}
}

// firstIPv4 returns the first IPv4 address across all attached networks.
func firstIPv4(addresses map[string]any) string {
	for _, v := range addresses {
		list, ok := v.([]any)
		if !ok {
			continue
		}
		for _, a := range list {
			entry, ok := a.(map[string]any)
			if !ok {
				continue
			}
			if ver, _ := entry["version"].(float64); ver == 4 {
				if ip, ok := entry["addr"].(string); ok {
					return ip
				}
			}
		}
	}
	return ""
}

func waitForServerActive(ctx context.Context, client *gophercloud.ServiceClient, id string, timeout time.Duration) (*servers.Server, error) {
	deadline := time.Now().Add(timeout)
	for {
		server, err := servers.Get(ctx, client, id).Extract()
		if err != nil {
			return nil, err
		}
		switch server.Status {
		case "ACTIVE":
			return server, nil
		case "ERROR":
			return nil, fmt.Errorf("instance %s entered ERROR state: %v", id, server.Fault.Message)
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("timed out waiting for instance %s to become active (last status %q)", id, server.Status)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
}

// waitForServerStatus blocks until the server reaches target status, failing on
// ERROR or timeout. Used for the VERIFY_RESIZE checkpoint during a resize.
func waitForServerStatus(ctx context.Context, client *gophercloud.ServiceClient, id, target string, timeout time.Duration) (*servers.Server, error) {
	deadline := time.Now().Add(timeout)
	for {
		server, err := servers.Get(ctx, client, id).Extract()
		if err != nil {
			return nil, err
		}
		switch server.Status {
		case target:
			return server, nil
		case "ERROR":
			return nil, fmt.Errorf("instance %s entered ERROR state: %v", id, server.Fault.Message)
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("timed out waiting for instance %s to reach %s (last status %q)", id, target, server.Status)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
}

func waitForServerDeleted(ctx context.Context, client *gophercloud.ServiceClient, id string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		_, err := servers.Get(ctx, client, id).Extract()
		if err != nil {
			if gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
				return nil
			}
			return err
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out waiting for instance %s to delete", id)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
}
