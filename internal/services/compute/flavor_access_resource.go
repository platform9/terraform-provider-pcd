// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0
//
// Ported from terraform-provider-openstack v3.4.0
// (openstack/resource_openstack_compute_flavor_access_v2.go), adapted for the
// terraform-plugin-framework and PCD. One resource is one project's access to
// one private flavor (POST /flavors/{id}/action addTenantAccess and
// removeTenantAccess; GET /flavors/{id}/os-flavor-access).

package compute

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/flavors"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/platform9/terraform-provider-pcd/internal/clients"
)

var (
	_ resource.Resource                = (*flavorAccessResource)(nil)
	_ resource.ResourceWithConfigure   = (*flavorAccessResource)(nil)
	_ resource.ResourceWithImportState = (*flavorAccessResource)(nil)
)

// NewFlavorAccessResource is the factory registered with the provider.
func NewFlavorAccessResource() resource.Resource {
	return &flavorAccessResource{}
}

type flavorAccessResource struct {
	config *clients.Config
}

type flavorAccessModel struct {
	ID       types.String `tfsdk:"id"`
	FlavorID types.String `tfsdk:"flavor_id"`
	TenantID types.String `tfsdk:"tenant_id"`
	Region   types.String `tfsdk:"region"`
}

func (r *flavorAccessResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_compute_flavor_access"
}

func (r *flavorAccessResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	forceNew := []planmodifier.String{stringplanmodifier.RequiresReplace()}
	resp.Schema = schema.Schema{
		MarkdownDescription: "Grants one project access to one private compute flavor (admin only), the PCD UI's " +
			"\"Edit Tenants\" on a flavor. Use one resource per flavor and project pair, with `for_each` for a list " +
			"of projects. The flavor must be private (`is_public = false` on `pcd_compute_flavor`): Nova keeps no " +
			"access list for a public flavor, so the provider refuses one at apply time. The resource is not " +
			"authoritative: grants made outside Terraform for other projects are left alone. A grant that already " +
			"exists when the resource is created (made in the UI, say) is adopted, so destroying the resource " +
			"revokes it.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{Computed: true, MarkdownDescription: "The composite `<flavor_id>/<tenant_id>` ID.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()}},
			"flavor_id": schema.StringAttribute{Required: true, PlanModifiers: forceNew,
				MarkdownDescription: "The ID of a private flavor. Changing this forces a new resource."},
			"tenant_id": schema.StringAttribute{Required: true, PlanModifiers: forceNew,
				MarkdownDescription: "The ID of the project to grant access to; Nova checks that it exists. Changing " +
					"this forces a new resource."},
			"region": schema.StringAttribute{Optional: true, Computed: true,
				PlanModifiers:       []planmodifier.String{stringplanmodifier.RequiresReplace(), stringplanmodifier.UseStateForUnknown()},
				MarkdownDescription: "The region. Defaults to the provider's region. Changing this forces a new resource."},
		},
	}
}

func (r *flavorAccessResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.config = configureClient(req.ProviderData, &resp.Diagnostics)
}

func (r *flavorAccessResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan flavorAccessModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := r.config.ForRegion(plan.Region.ValueString()).ComputeV2Client()
	if err != nil {
		resp.Diagnostics.AddError("compute: building v2 client", err.Error())
		return
	}

	flavorID := plan.FlavorID.ValueString()
	tenantID := plan.TenantID.ValueString()

	// At the provider's base microversion Nova accepts a grant on a public
	// flavor, and then answers 404 to every access-list read, so Read would drop
	// the grant on each refresh and the plan would never settle. Refuse it here,
	// as the PCD UI does by offering "Edit Tenants" only on private flavors.
	flavor, err := clients.RequireObject(flavors.Get(ctx, client, flavorID).Extract())
	if err != nil {
		if gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
			resp.Diagnostics.AddAttributeError(path.Root("flavor_id"), "Flavor not found",
				fmt.Sprintf("No flavor with ID %q exists.", flavorID))
			return
		}
		resp.Diagnostics.AddError("compute: reading flavor", err.Error())
		return
	}
	if flavor.IsPublic {
		resp.Diagnostics.AddAttributeError(path.Root("flavor_id"), "Flavor is public",
			fmt.Sprintf("Flavor %s (%s) is public, and project access applies only to private flavors. "+
				"Create the flavor with is_public = false.", flavor.Name, flavorID))
		return
	}

	if _, err := flavors.AddAccess(ctx, client, flavorID, flavors.AddAccessOpts{Tenant: tenantID}).Extract(); err != nil {
		// 409 is Nova's FlavorAccessExists: the grant is already there, so it is
		// adopted, as upstream does.
		if !gophercloud.ResponseCodeIs(err, http.StatusConflict) {
			resp.Diagnostics.AddError("compute: granting flavor access", err.Error())
			return
		}
	}

	plan.ID = types.StringValue(flavorID + "/" + tenantID)
	if plan.Region.IsNull() || plan.Region.IsUnknown() {
		plan.Region = types.StringValue(r.config.Region)
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *flavorAccessResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state flavorAccessModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := r.config.ForRegion(state.Region.ValueString()).ComputeV2Client()
	if err != nil {
		resp.Diagnostics.AddError("compute: building v2 client", err.Error())
		return
	}

	flavorID := state.FlavorID.ValueString()
	tenantID := state.TenantID.ValueString()
	found, err := flavorAccessExists(ctx, client, flavorID, tenantID)
	if err != nil {
		resp.Diagnostics.AddError("compute: listing flavor access", err.Error())
		return
	}
	if !found {
		resp.Diagnostics.AddWarning("Flavor access not found",
			fmt.Sprintf("Project %s no longer has access to flavor %s (revoked, or the flavor was deleted); "+
				"it was removed from state.", tenantID, flavorID))
		resp.State.RemoveResource(ctx)
		return
	}

	state.ID = types.StringValue(flavorID + "/" + tenantID)
	if state.Region.IsNull() || state.Region.IsUnknown() {
		state.Region = types.StringValue(r.config.Region)
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

// Update is unreachable: every attribute forces replacement.
func (r *flavorAccessResource) Update(_ context.Context, _ resource.UpdateRequest, resp *resource.UpdateResponse) {
	resp.Diagnostics.AddError("compute: flavor access update",
		"flavor_id, tenant_id and region force replacement; this should not be reached.")
}

func (r *flavorAccessResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state flavorAccessModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := r.config.ForRegion(state.Region.ValueString()).ComputeV2Client()
	if err != nil {
		resp.Diagnostics.AddError("compute: building v2 client", err.Error())
		return
	}

	opts := flavors.RemoveAccessOpts{Tenant: state.TenantID.ValueString()}
	if _, err := flavors.RemoveAccess(ctx, client, state.FlavorID.ValueString(), opts).Extract(); err != nil {
		// 404 is FlavorAccessNotFound or FlavorNotFound: already gone.
		if gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
			return
		}
		resp.Diagnostics.AddError("compute: revoking flavor access", err.Error())
	}
}

func (r *flavorAccessResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	flavorID, tenantID, err := splitFlavorAccessID(req.ID)
	if err != nil {
		resp.Diagnostics.AddError("Invalid import ID", err.Error())
		return
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), req.ID)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("flavor_id"), flavorID)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("tenant_id"), tenantID)...)
}

// splitFlavorAccessID parses "<flavor_id>/<tenant_id>". It is a sibling of
// splitInstanceScopedID, whose error text names instance_id.
func splitFlavorAccessID(id string) (flavorID, tenantID string, err error) {
	parts := strings.SplitN(id, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("expected <flavor_id>/<tenant_id>, got %q", id)
	}
	return parts[0], parts[1], nil
}

// flavorAccessExists reports whether tenantID is on flavorID's access list.
// Only a real 404 (the flavor is gone) or the tenant's absence count as
// "not found"; any other error is returned, so a 403 or a 5xx never drops a
// grant from state. Upstream turns every list error into a 404; this does not.
func flavorAccessExists(ctx context.Context, client *gophercloud.ServiceClient, flavorID, tenantID string) (bool, error) {
	pages, err := flavors.ListAccesses(client, flavorID).AllPages(ctx)
	if err != nil {
		if gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
			return false, nil
		}
		return false, err
	}
	accesses, err := flavors.ExtractAccesses(pages)
	if err != nil {
		return false, err
	}
	for _, a := range accesses {
		if a.TenantID == tenantID {
			return true, nil
		}
	}
	return false, nil
}
