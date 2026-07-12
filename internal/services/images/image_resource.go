// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0
//
// Ported from terraform-provider-openstack v3.4.0
// (openstack/resource_openstack_images_image_v2.go), adapted for the
// terraform-plugin-framework and PCD.

// Package images implements the pcd_images_* resources and data sources (Glance v2).
package images

import (
	"context"
	"crypto/md5" //nolint:gosec // Glance checksums are md5; this only compares against them.
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/image/v2/imagedata"
	"github.com/gophercloud/gophercloud/v2/openstack/image/v2/imageimport"
	"github.com/gophercloud/gophercloud/v2/openstack/image/v2/images"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/int64default"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/mapplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/setplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/platform9/terraform-provider-pcd/internal/clients"
)

var (
	_ resource.Resource                = (*imageResource)(nil)
	_ resource.ResourceWithConfigure   = (*imageResource)(nil)
	_ resource.ResourceWithImportState = (*imageResource)(nil)
)

// NewImageResource is the factory registered with the provider.
func NewImageResource() resource.Resource {
	return &imageResource{}
}

type imageResource struct {
	config *clients.Config
}

type imageModel struct {
	ID              types.String `tfsdk:"id"`
	Name            types.String `tfsdk:"name"`
	ContainerFormat types.String `tfsdk:"container_format"`
	DiskFormat      types.String `tfsdk:"disk_format"`
	LocalFilePath   types.String `tfsdk:"local_file_path"`
	ImageSourceURL  types.String `tfsdk:"image_source_url"`
	MinDiskGB       types.Int64  `tfsdk:"min_disk_gb"`
	MinRAMMB        types.Int64  `tfsdk:"min_ram_mb"`
	Protected       types.Bool   `tfsdk:"protected"`
	Visibility      types.String `tfsdk:"visibility"`
	Hidden          types.Bool   `tfsdk:"hidden"`
	Tags            types.Set    `tfsdk:"tags"`
	VerifyChecksum  types.Bool   `tfsdk:"verify_checksum"`
	Properties      types.Map    `tfsdk:"properties"`
	Checksum        types.String `tfsdk:"checksum"`
	SizeBytes       types.Int64  `tfsdk:"size_bytes"`
	Status          types.String `tfsdk:"status"`
	Owner           types.String `tfsdk:"owner"`
	CreatedAt       types.String `tfsdk:"created_at"`
	UpdatedAt       types.String `tfsdk:"updated_at"`
	Region          types.String `tfsdk:"region"`
}

func (r *imageResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_images_image"
}

func (r *imageResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	forceNew := []planmodifier.String{stringplanmodifier.RequiresReplace()}
	stable := []planmodifier.String{stringplanmodifier.UseStateForUnknown()}
	resp.Schema = schema.Schema{
		MarkdownDescription: "Manages an image in PCD's Glance image service. Provide the image data via " +
			"`local_file_path` (uploaded from the machine running Terraform) or `image_source_url` " +
			"(fetched by Glance using the web-download import method).",
		Attributes: map[string]schema.Attribute{
			"id":               schema.StringAttribute{Computed: true, MarkdownDescription: "The image ID.", PlanModifiers: stable},
			"name":             schema.StringAttribute{Required: true, MarkdownDescription: "The name of the image."},
			"container_format": schema.StringAttribute{Required: true, MarkdownDescription: "Container format (e.g. bare, ovf). Changing this forces a new resource.", PlanModifiers: forceNew},
			"disk_format":      schema.StringAttribute{Required: true, MarkdownDescription: "Disk format (e.g. qcow2, raw). Changing this forces a new resource.", PlanModifiers: forceNew},
			"local_file_path":  schema.StringAttribute{Optional: true, MarkdownDescription: "Path to a local image file to upload. Mutually exclusive with image_source_url.", PlanModifiers: forceNew},
			"image_source_url": schema.StringAttribute{Optional: true, MarkdownDescription: "URL for Glance to fetch via web-download. Mutually exclusive with local_file_path.", PlanModifiers: forceNew},
			"min_disk_gb":      schema.Int64Attribute{Optional: true, Computed: true, Default: int64default.StaticInt64(0), MarkdownDescription: "Minimum disk (GB) required to boot the image."},
			"min_ram_mb":       schema.Int64Attribute{Optional: true, Computed: true, Default: int64default.StaticInt64(0), MarkdownDescription: "Minimum RAM (MB) required to boot the image."},
			"protected":        schema.BoolAttribute{Optional: true, Computed: true, Default: booldefault.StaticBool(false), MarkdownDescription: "Whether the image is protected from deletion."},
			"visibility":       schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "Image visibility: public, private, shared, or community.", PlanModifiers: stable},
			"hidden":           schema.BoolAttribute{Optional: true, Computed: true, Default: booldefault.StaticBool(false), MarkdownDescription: "Whether the image is hidden from the default list."},
			"tags":             schema.SetAttribute{Optional: true, Computed: true, ElementType: types.StringType, MarkdownDescription: "Tags applied to the image.", PlanModifiers: []planmodifier.Set{setplanmodifier.UseStateForUnknown()}},
			"verify_checksum":  schema.BoolAttribute{Optional: true, Computed: true, Default: booldefault.StaticBool(true), MarkdownDescription: "Verify the uploaded file's md5 against the Glance checksum (local_file_path only)."},
			"properties":       schema.MapAttribute{Optional: true, Computed: true, ElementType: types.StringType, MarkdownDescription: "User-defined key/value image properties (custom Glance metadata). Only keys you set here are managed; Glance system/read-only properties are not tracked.", PlanModifiers: []planmodifier.Map{mapplanmodifier.UseStateForUnknown()}},
			"checksum":         schema.StringAttribute{Computed: true, MarkdownDescription: "md5 checksum of the image data."},
			"size_bytes":       schema.Int64Attribute{Computed: true, MarkdownDescription: "Size of the image data in bytes."},
			"status":           schema.StringAttribute{Computed: true, MarkdownDescription: "Glance status (e.g. active)."},
			"owner":            schema.StringAttribute{Computed: true, MarkdownDescription: "Project that owns the image."},
			"created_at":       schema.StringAttribute{Computed: true, MarkdownDescription: "Creation timestamp (RFC3339)."},
			"updated_at":       schema.StringAttribute{Computed: true, MarkdownDescription: "Last-update timestamp (RFC3339)."},
			"region":           schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "The region. Defaults to the provider's region.", PlanModifiers: stable},
		},
	}
}

func (r *imageResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	if req.ProviderData == nil {
		return
	}
	config, ok := req.ProviderData.(*clients.Config)
	if !ok {
		resp.Diagnostics.AddError("Unexpected provider data type",
			fmt.Sprintf("Expected *clients.Config, got %T. This is a bug in the provider.", req.ProviderData))
		return
	}
	r.config = config
}

func (r *imageResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan imageModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	localPath := plan.LocalFilePath.ValueString()
	sourceURL := plan.ImageSourceURL.ValueString()
	if (localPath == "") == (sourceURL == "") {
		resp.Diagnostics.AddError("Invalid image source",
			"Exactly one of local_file_path or image_source_url must be set.")
		return
	}

	client, err := r.config.ImageV2Client()
	if err != nil {
		resp.Diagnostics.AddError("images: building v2 client", err.Error())
		return
	}

	protected := plan.Protected.ValueBool()
	hidden := plan.Hidden.ValueBool()
	var tags []string
	if !plan.Tags.IsNull() && !plan.Tags.IsUnknown() {
		resp.Diagnostics.Append(plan.Tags.ElementsAs(ctx, &tags, false)...)
		if resp.Diagnostics.HasError() {
			return
		}
	}

	createOpts := images.CreateOpts{
		Name:            plan.Name.ValueString(),
		ContainerFormat: plan.ContainerFormat.ValueString(),
		DiskFormat:      plan.DiskFormat.ValueString(),
		MinDisk:         int(plan.MinDiskGB.ValueInt64()),
		MinRAM:          int(plan.MinRAMMB.ValueInt64()),
		Protected:       &protected,
		Hidden:          &hidden,
		Tags:            tags,
	}
	if v := plan.Visibility.ValueString(); v != "" {
		vis := images.ImageVisibility(v)
		createOpts.Visibility = &vis
	}
	if !plan.Properties.IsNull() && !plan.Properties.IsUnknown() {
		var userProps map[string]string
		resp.Diagnostics.Append(plan.Properties.ElementsAs(ctx, &userProps, false)...)
		if resp.Diagnostics.HasError() {
			return
		}
		createOpts.Properties = userProps
	}

	img, err := images.Create(ctx, client, createOpts).Extract()
	if err != nil {
		resp.Diagnostics.AddError("images: creating image", err.Error())
		return
	}

	// Load image data, then wait for it to become active.
	if localPath != "" {
		if err := r.uploadLocalFile(ctx, client, img.ID, localPath, plan.VerifyChecksum.ValueBool()); err != nil {
			_ = images.Delete(ctx, client, img.ID).ExtractErr()
			resp.Diagnostics.AddError("images: uploading image data", err.Error())
			return
		}
	} else {
		if err := imageimport.Create(ctx, client, img.ID, imageimport.CreateOpts{
			Name: imageimport.WebDownloadMethod,
			URI:  sourceURL,
		}).ExtractErr(); err != nil {
			_ = images.Delete(ctx, client, img.ID).ExtractErr()
			resp.Diagnostics.AddError("images: starting web-download import", err.Error())
			return
		}
	}

	img, err = waitForImageActive(ctx, client, img.ID, 30*time.Minute)
	if err != nil {
		resp.Diagnostics.AddError("images: waiting for active image", err.Error())
		return
	}

	resp.Diagnostics.Append(r.flatten(ctx, img, &plan)...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *imageResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state imageModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := r.config.ImageV2Client()
	if err != nil {
		resp.Diagnostics.AddError("images: building v2 client", err.Error())
		return
	}

	img, err := images.Get(ctx, client, state.ID.ValueString()).Extract()
	if err != nil {
		if gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
			resp.Diagnostics.AddWarning("Image not found",
				fmt.Sprintf("Image %s no longer exists and was removed from state.", state.ID.ValueString()))
			resp.State.RemoveResource(ctx)
			return
		}
		resp.Diagnostics.AddError("images: reading image", err.Error())
		return
	}

	resp.Diagnostics.Append(r.flatten(ctx, img, &state)...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *imageResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state imageModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := r.config.ImageV2Client()
	if err != nil {
		resp.Diagnostics.AddError("images: building v2 client", err.Error())
		return
	}

	var patch images.UpdateOpts
	if !plan.Name.Equal(state.Name) {
		patch = append(patch, images.ReplaceImageName{NewName: plan.Name.ValueString()})
	}
	if !plan.MinDiskGB.Equal(state.MinDiskGB) {
		patch = append(patch, images.ReplaceImageMinDisk{NewMinDisk: int(plan.MinDiskGB.ValueInt64())})
	}
	if !plan.MinRAMMB.Equal(state.MinRAMMB) {
		patch = append(patch, images.ReplaceImageMinRam{NewMinRam: int(plan.MinRAMMB.ValueInt64())})
	}
	if !plan.Protected.Equal(state.Protected) {
		patch = append(patch, images.ReplaceImageProtected{NewProtected: plan.Protected.ValueBool()})
	}
	if !plan.Visibility.Equal(state.Visibility) && plan.Visibility.ValueString() != "" {
		patch = append(patch, images.UpdateVisibility{Visibility: images.ImageVisibility(plan.Visibility.ValueString())})
	}
	if !plan.Tags.Equal(state.Tags) {
		var tags []string
		if !plan.Tags.IsNull() && !plan.Tags.IsUnknown() {
			resp.Diagnostics.Append(plan.Tags.ElementsAs(ctx, &tags, false)...)
			if resp.Diagnostics.HasError() {
				return
			}
		}
		patch = append(patch, images.ReplaceImageTags{NewTags: tags})
	}
	if !plan.Properties.Equal(state.Properties) {
		var planProps, stateProps map[string]string
		if !plan.Properties.IsNull() && !plan.Properties.IsUnknown() {
			resp.Diagnostics.Append(plan.Properties.ElementsAs(ctx, &planProps, false)...)
		}
		if !state.Properties.IsNull() && !state.Properties.IsUnknown() {
			resp.Diagnostics.Append(state.Properties.ElementsAs(ctx, &stateProps, false)...)
		}
		if resp.Diagnostics.HasError() {
			return
		}
		for k, v := range planProps {
			if sv, ok := stateProps[k]; !ok {
				patch = append(patch, images.UpdateImageProperty{Op: images.AddOp, Name: k, Value: v})
			} else if sv != v {
				patch = append(patch, images.UpdateImageProperty{Op: images.ReplaceOp, Name: k, Value: v})
			}
		}
		for k := range stateProps {
			if _, ok := planProps[k]; !ok {
				patch = append(patch, images.UpdateImageProperty{Op: images.RemoveOp, Name: k})
			}
		}
	}

	if len(patch) > 0 {
		if _, err := images.Update(ctx, client, plan.ID.ValueString(), patch).Extract(); err != nil {
			resp.Diagnostics.AddError("images: updating image", err.Error())
			return
		}
	}

	img, err := images.Get(ctx, client, plan.ID.ValueString()).Extract()
	if err != nil {
		resp.Diagnostics.AddError("images: reading image after update", err.Error())
		return
	}
	resp.Diagnostics.Append(r.flatten(ctx, img, &plan)...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *imageResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state imageModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := r.config.ImageV2Client()
	if err != nil {
		resp.Diagnostics.AddError("images: building v2 client", err.Error())
		return
	}

	// Glance refuses to delete a protected image; clear the flag first.
	if state.Protected.ValueBool() {
		if _, err := images.Update(ctx, client, state.ID.ValueString(), images.UpdateOpts{
			images.ReplaceImageProtected{NewProtected: false},
		}).Extract(); err != nil && !gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
			resp.Diagnostics.AddError("images: clearing protected before delete", err.Error())
			return
		}
	}

	if err := images.Delete(ctx, client, state.ID.ValueString()).ExtractErr(); err != nil {
		if gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
			return
		}
		resp.Diagnostics.AddError("images: deleting image", err.Error())
	}
}

func (r *imageResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, path.Root("id"), req, resp)
}

// uploadLocalFile streams a local file to Glance and, when requested, verifies
// the md5 checksum Glance computed against the file.
func (r *imageResource) uploadLocalFile(ctx context.Context, client *gophercloud.ServiceClient, id, localPath string, verify bool) error {
	f, err := os.Open(localPath)
	if err != nil {
		return fmt.Errorf("opening %s: %w", localPath, err)
	}
	defer f.Close()

	if err := imagedata.Upload(ctx, client, id, f).ExtractErr(); err != nil {
		return err
	}
	if !verify {
		return nil
	}

	sum, err := fileMD5(localPath)
	if err != nil {
		return fmt.Errorf("computing checksum: %w", err)
	}
	img, err := images.Get(ctx, client, id).Extract()
	if err != nil {
		return err
	}
	if img.Checksum != "" && img.Checksum != sum {
		return fmt.Errorf("checksum mismatch: local %s, glance %s", sum, img.Checksum)
	}
	return nil
}

// flatten copies server-known fields onto the model. local_file_path,
// image_source_url, and verify_checksum are inputs and are preserved as-is.
func (r *imageResource) flatten(ctx context.Context, img *images.Image, m *imageModel) (diags diag.Diagnostics) {
	m.ID = types.StringValue(img.ID)
	m.Name = types.StringValue(img.Name)
	m.ContainerFormat = types.StringValue(img.ContainerFormat)
	m.DiskFormat = types.StringValue(img.DiskFormat)
	m.MinDiskGB = types.Int64Value(int64(img.MinDiskGigabytes))
	m.MinRAMMB = types.Int64Value(int64(img.MinRAMMegabytes))
	m.Protected = types.BoolValue(img.Protected)
	m.Visibility = types.StringValue(string(img.Visibility))
	m.Hidden = types.BoolValue(img.Hidden)
	m.Checksum = types.StringValue(img.Checksum)
	m.SizeBytes = types.Int64Value(img.SizeBytes)
	m.Status = types.StringValue(string(img.Status))
	m.Owner = types.StringValue(img.Owner)
	m.CreatedAt = types.StringValue(img.CreatedAt.Format(time.RFC3339))
	m.UpdatedAt = types.StringValue(img.UpdatedAt.Format(time.RFC3339))

	tagVals := img.Tags
	if tagVals == nil {
		tagVals = []string{}
	}
	tags, d := types.SetValueFrom(ctx, types.StringType, tagVals)
	diags = append(diags, d...)
	m.Tags = tags

	// Echo-only: Glance returns many system/read-only properties (os_hash_*, stores,
	// direct_url, owner_specified.*, ...) via RemainingKeys. Track only the keys the
	// user manages (already present in the model), or every apply would try to
	// modify read-only props and the plan would never converge.
	managed := map[string]struct{}{}
	if !m.Properties.IsNull() && !m.Properties.IsUnknown() {
		var cur map[string]string
		diags = append(diags, m.Properties.ElementsAs(ctx, &cur, false)...)
		for k := range cur {
			managed[k] = struct{}{}
		}
	}
	props := make(map[string]string, len(managed))
	for k, v := range img.Properties {
		if _, ok := managed[k]; ok && !isSystemImageProperty(k) {
			props[k] = fmt.Sprintf("%v", v)
		}
	}
	propsMap, d := types.MapValueFrom(ctx, types.StringType, props)
	diags = append(diags, d...)
	m.Properties = propsMap

	if m.Region.IsNull() || m.Region.IsUnknown() {
		m.Region = types.StringValue(r.config.Region)
	}
	return diags
}

// systemImageProperties are Glance-managed keys that surface in Image.Properties
// but must never be tracked or patched as user metadata. The echo-only filter in
// flatten already restricts to user-managed keys; this is a second guard in case
// a user names a key that collides with one of these.
var systemImageProperties = map[string]struct{}{
	"os_hash_algo": {}, "os_hash_value": {}, "os_hidden": {},
	"stores": {}, "store": {}, "direct_url": {}, "locations": {}, "location": {},
	"self": {}, "schema": {}, "file": {}, "size": {}, "virtual_size": {},
	"checksum": {}, "container_format": {}, "disk_format": {}, "min_disk": {},
	"min_ram": {}, "owner": {}, "protected": {}, "visibility": {}, "status": {},
	"tags": {}, "id": {}, "name": {}, "created_at": {}, "updated_at": {},
	"metadata": {}, "properties": {},
}

// isSystemImageProperty reports whether a Glance property key is system/read-only.
func isSystemImageProperty(key string) bool {
	if _, ok := systemImageProperties[key]; ok {
		return true
	}
	return strings.HasPrefix(key, "owner_specified.") || strings.HasPrefix(key, "os_glance_")
}

func waitForImageActive(ctx context.Context, client *gophercloud.ServiceClient, id string, timeout time.Duration) (*images.Image, error) {
	deadline := time.Now().Add(timeout)
	for {
		img, err := images.Get(ctx, client, id).Extract()
		if err != nil {
			return nil, err
		}
		switch img.Status {
		case images.ImageStatusActive:
			return img, nil
		case images.ImageStatusKilled:
			return nil, fmt.Errorf("image %s entered killed state", id)
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("timed out waiting for image %s to become active (last status %q)", id, img.Status)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(3 * time.Second):
		}
	}
}

func fileMD5(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := md5.New() //nolint:gosec // matching Glance's md5 checksum
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
