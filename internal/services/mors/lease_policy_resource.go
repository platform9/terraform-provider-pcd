// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package mors

import (
	"context"
	"fmt"
	"net/http"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/hashicorp/terraform-plugin-framework-validators/int64validator"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringdefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/platform9/terraform-provider-pcd/internal/clients"
)

var (
	_ resource.Resource                = (*leasePolicyResource)(nil)
	_ resource.ResourceWithConfigure   = (*leasePolicyResource)(nil)
	_ resource.ResourceWithImportState = (*leasePolicyResource)(nil)
)

// leaseActions are what mors can do to a VM whose lease has run out. mors
// itself stores any string and treats everything but "delete" as a power off,
// so the schema validators are the only check a typo meets.
var leaseActions = []string{"power off", "delete"}

// defaultLeaseAction is mors's own default and the PCD UI's.
const defaultLeaseAction = "power off"

// NewLeasePolicyResource is the factory registered with the provider.
func NewLeasePolicyResource() resource.Resource {
	return &leasePolicyResource{}
}

type leasePolicyResource struct {
	config *clients.Config
}

type leasePolicyModel struct {
	ID            types.String `tfsdk:"id"`
	ProjectID     types.String `tfsdk:"project_id"`
	ExpiryMinutes types.Int64  `tfsdk:"expiry_minutes"`
	Action        types.String `tfsdk:"action"`
}

// leasePolicyAPI is mors's vm_lease_policy object. The response also carries
// created_at/by and updated_at/by, which the resource does not expose.
type leasePolicyAPI struct {
	TenantUUID string `json:"tenant_uuid"`
	ExpiryMins int64  `json:"expiry_mins"`
	Action     string `json:"action"`
}

type leasePolicyEnvelope struct {
	Policy leasePolicyAPI `json:"vm_lease_policy"`
}

func leasePolicyURL(client *gophercloud.ServiceClient, projectID string) string {
	return client.ServiceURL("tenant", projectID)
}

// leasePolicyBody builds the POST and PUT body. mors writes the project named
// by the body's tenant_uuid and ignores the one in the URL, so both are built
// from the same project ID; a mismatch would write another project's policy.
// PUT reads action without a default, so it is always sent.
func leasePolicyBody(projectID string, expiryMinutes int64, action string) leasePolicyEnvelope {
	return leasePolicyEnvelope{Policy: leasePolicyAPI{
		TenantUUID: projectID,
		ExpiryMins: expiryMinutes,
		Action:     action,
	}}
}

// getLeasePolicy reads a project's policy. A 200 that holds no policy is
// clients.ErrNoObject, not a policy of 0 minutes and no action.
// A project_id unknown at plan skips the schema's validator, so checkID
// guards every request: /v1/tenant/ is mors's list route, not a policy.
func getLeasePolicy(ctx context.Context, client *gophercloud.ServiceClient, projectID string) (*leasePolicyAPI, error) {
	if err := checkID("project_id", projectID); err != nil {
		return nil, err
	}
	var out leasePolicyEnvelope
	if err := getJSON(ctx, client, leasePolicyURL(client, projectID), &out); err != nil {
		return nil, err
	}
	if out.Policy.TenantUUID == "" {
		return nil, clients.ErrNoObject
	}
	return &out.Policy, nil
}

func writeLeasePolicy(ctx context.Context, client *gophercloud.ServiceClient, method, projectID string, expiryMinutes int64, action string) error {
	if err := checkID("project_id", projectID); err != nil {
		return err
	}
	return writeJSON(ctx, client, method, leasePolicyURL(client, projectID), leasePolicyBody(projectID, expiryMinutes, action))
}

func (r *leasePolicyResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_compute_lease_policy"
}

func (r *leasePolicyResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Manages a project's VM lease policy in mors, PCD's VM lease service: how long a VM in " +
			"the project may live, counted from when it was created, and what happens when that time is up. This is " +
			"the PCD UI's **Manage Lease Policy** on the Tenants page.\n\n" +
			"~> mors checks leases about once a minute. Creating a policy, or shortening one, acts within a minute " +
			"on every VM in the project that is already older than the new duration, except VMs that have their " +
			"own lease (`pcd_compute_instance_lease`): it powers them off or deletes them. A VM that was powered " +
			"off because its lease ran out is powered off again on every pass, so powering it back on does not " +
			"stick until its lease is extended with `pcd_compute_instance_lease`. Changing `action` from " +
			"`power off` to `delete` deletes those VMs within a minute, including the ones mors had already " +
			"powered off.\n\n" +
			"Destroying the policy also deletes every per-VM lease in the project (`pcd_compute_instance_lease` " +
			"resources then disappear on the next refresh), and so does replacing it (for example with " +
			"`-replace`, or by renaming the resource without a `moved` block): the VMs fall back to the policy " +
			"until the next apply writes their leases again. Leases set outside this configuration, with the PCD " +
			"UI's Customize Lease for example, are not written again. Use a `moved` block to rename it. A policy applies in the region whose mors holds it, " +
			"which is the provider's region; use a provider alias to manage another region's.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "The project ID.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"project_id": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "The project (tenant) the policy applies to. Changing this forces a new resource.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.RequiresReplace()},
				Validators:          []validator.String{stringvalidator.RegexMatches(idShape, idShapeMessage)},
			},
			"expiry_minutes": schema.Int64Attribute{
				Required: true,
				MarkdownDescription: "How long a VM may live, in minutes from its creation. The PCD UI enters it as " +
					"days, hours and minutes, so three days is `3 * 24 * 60`. At least 1.",
				Validators: []validator.Int64{int64validator.AtLeast(1)},
			},
			"action": schema.StringAttribute{
				Optional: true,
				Computed: true,
				Default:  stringdefault.StaticString(defaultLeaseAction),
				MarkdownDescription: "What mors does to a VM whose lease has run out: `power off` (the default) " +
					"or `delete`. Changing it from `power off` to `delete` deletes, within a minute, every VM in the " +
					"project that is already older than the duration and has no lease of its own, including the ones " +
					"mors had already powered off.",
				Validators: []validator.String{stringvalidator.OneOf(leaseActions...)},
			},
		},
	}
}

func (r *leasePolicyResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.config = configureClient(req.ProviderData, &resp.Diagnostics)
}

func (r *leasePolicyResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan leasePolicyModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := r.config.MorsV1Client()
	if err != nil {
		resp.Diagnostics.AddError("mors: building client", err.Error())
		return
	}

	projectID := plan.ProjectID.ValueString()
	if err := writeLeasePolicy(ctx, client, http.MethodPost, projectID, plan.ExpiryMinutes.ValueInt64(), plan.Action.ValueString()); err != nil {
		if isConflict(err) {
			resp.Diagnostics.AddError("mors: creating lease policy", fmt.Sprintf(
				"Project %s already has a lease policy, set outside this configuration (in the PCD UI, for "+
					"example) or by an earlier apply whose answer was lost. Import it to manage it here: terraform import <resource address> %s", projectID, projectID))
			return
		}
		resp.Diagnostics.AddError("mors: creating lease policy", err.Error())
		return
	}

	plan.ID = types.StringValue(projectID)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *leasePolicyResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state leasePolicyModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	client, err := r.config.MorsV1Client()
	if err != nil {
		resp.Diagnostics.AddError("mors: building client", err.Error())
		return
	}

	policy, err := getLeasePolicy(ctx, client, state.ProjectID.ValueString())
	if err != nil {
		if isNotFound(err) {
			resp.State.RemoveResource(ctx)
			return
		}
		resp.Diagnostics.AddError("mors: reading lease policy", err.Error())
		return
	}

	state.ID = state.ProjectID
	state.ExpiryMinutes = types.Int64Value(policy.ExpiryMins)
	state.Action = types.StringValue(policy.Action)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *leasePolicyResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan leasePolicyModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := r.config.MorsV1Client()
	if err != nil {
		resp.Diagnostics.AddError("mors: building client", err.Error())
		return
	}

	projectID := plan.ProjectID.ValueString()
	if err := writeLeasePolicy(ctx, client, http.MethodPut, projectID, plan.ExpiryMinutes.ValueInt64(), plan.Action.ValueString()); err != nil {
		resp.Diagnostics.AddError("mors: updating lease policy", err.Error())
		return
	}
	// mors answers a PUT for a project that has no policy with 200 and changes
	// nothing, so only a read proves the update landed.
	if _, err := getLeasePolicy(ctx, client, projectID); err != nil {
		if isNotFound(err) {
			resp.Diagnostics.AddError("mors: updating lease policy", fmt.Sprintf(
				"Project %s no longer has a lease policy: it was deleted outside Terraform, and mors ignores an "+
					"update to a policy it does not hold. The next plan creates it again.", projectID))
			return
		}
		resp.Diagnostics.AddError("mors: reading lease policy after the update", err.Error())
		return
	}

	plan.ID = types.StringValue(projectID)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *leasePolicyResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state leasePolicyModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	// An empty or malformed project ID names no policy: /v1/tenant/ is not one
	// to delete, and any other malformed ID would address another route.
	projectID := state.ProjectID.ValueString()
	if checkID("project_id", projectID) != nil {
		return
	}

	client, err := r.config.MorsV1Client()
	if err != nil {
		resp.Diagnostics.AddError("mors: building client", err.Error())
		return
	}

	if err := deleteObject(ctx, client, leasePolicyURL(client, projectID)); err != nil {
		resp.Diagnostics.AddError("mors: deleting lease policy", err.Error())
	}
}

// ImportState takes the project ID.
func (r *leasePolicyResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	if err := checkID("project_id", req.ID); err != nil {
		resp.Diagnostics.AddError("Invalid import ID", fmt.Sprintf("expected <project_id>: %v", err))
		return
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), req.ID)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("project_id"), req.ID)...)
}
