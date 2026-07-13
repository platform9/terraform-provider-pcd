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
	_ resource.Resource                = (*instanceResource)(nil)
	_ resource.ResourceWithConfigure   = (*instanceResource)(nil)
	_ resource.ResourceWithImportState = (*instanceResource)(nil)
)

// NewInstanceResource is the factory registered with the provider.
func NewInstanceResource() resource.Resource {
	return &instanceResource{}
}

type instanceResource struct {
	config *clients.Config
}

type instanceModel struct {
	ID               types.String `tfsdk:"id"`
	Name             types.String `tfsdk:"name"`
	ImageID          types.String `tfsdk:"image_id"`
	ImageName        types.String `tfsdk:"image_name"`
	FlavorID         types.String `tfsdk:"flavor_id"`
	FlavorName       types.String `tfsdk:"flavor_name"`
	KeyPair          types.String `tfsdk:"key_pair"`
	SecurityGroups   types.Set    `tfsdk:"security_groups"`
	Network          types.List   `tfsdk:"network"`
	Metadata         types.Map    `tfsdk:"metadata"`
	UserData         types.String `tfsdk:"user_data"`
	AvailabilityZone types.String `tfsdk:"availability_zone"`
	ConfigDrive      types.Bool   `tfsdk:"config_drive"`
	AccessIPv4       types.String `tfsdk:"access_ip_v4"`
	Status           types.String `tfsdk:"status"`
	Region           types.String `tfsdk:"region"`
}

type instanceNetworkModel struct {
	UUID types.String `tfsdk:"uuid"`
	Name types.String `tfsdk:"name"`
	Port types.String `tfsdk:"port"`
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
			"image_id":   schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "The image ID to boot from (alternative to image_name). Changing this forces a new resource.", PlanModifiers: fnC},
			"image_name": schema.StringAttribute{Optional: true, MarkdownDescription: "The image name to boot from, resolved via Glance (alternative to image_id). Exactly one of image_id/image_name is required. Changing this forces a new resource.", PlanModifiers: fn},
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
			"metadata":          schema.MapAttribute{Optional: true, Computed: true, ElementType: types.StringType, MarkdownDescription: "Key-value metadata attached to the instance.", PlanModifiers: []planmodifier.Map{mapplanmodifier.UseStateForUnknown()}},
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
		},
	}
}

func (r *instanceResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.config = configureClient(req.ProviderData, &resp.Diagnostics)
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

	imageNameSet := !plan.ImageName.IsNull() && plan.ImageName.ValueString() != ""
	imageIDSet := !plan.ImageID.IsNull() && !plan.ImageID.IsUnknown() && plan.ImageID.ValueString() != ""
	switch {
	case imageNameSet && imageIDSet:
		resp.Diagnostics.AddError("Invalid image", "Set only one of image_id or image_name.")
		return
	case !imageNameSet && !imageIDSet:
		resp.Diagnostics.AddError("Invalid image", "Exactly one of image_id or image_name must be set.")
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

	server, err := servers.Create(ctx, client, createOpts, nil).Extract()
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
	if !plan.Metadata.IsNull() && !plan.Metadata.IsUnknown() && !plan.Metadata.Equal(state.Metadata) {
		meta := map[string]string{}
		resp.Diagnostics.Append(plan.Metadata.ElementsAs(ctx, &meta, false)...)
		if resp.Diagnostics.HasError() {
			return
		}
		if _, err := servers.UpdateMetadata(ctx, client, plan.ID.ValueString(), servers.MetadataOpts(meta)).Extract(); err != nil {
			resp.Diagnostics.AddError("compute: updating instance metadata", err.Error())
			return
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

	meta := make(map[string]string, len(server.Metadata))
	for k, v := range server.Metadata {
		meta[k] = v
	}
	metaMap, d := types.MapValueFrom(ctx, types.StringType, meta)
	diags = append(diags, d...)
	m.Metadata = metaMap

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
