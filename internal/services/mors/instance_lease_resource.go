// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package mors

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/servers"
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
	_ resource.Resource                = (*instanceLeaseResource)(nil)
	_ resource.ResourceWithConfigure   = (*instanceLeaseResource)(nil)
	_ resource.ResourceWithImportState = (*instanceLeaseResource)(nil)
)

// leaseExpiryLayout is mors's only date format (%Y-%m-%dT%H:%M:%SZ): UTC with
// whole seconds. mors parses the request with exactly this layout and returns
// the expiry in it, so a value in this form round-trips through state as is.
const leaseExpiryLayout = "2006-01-02T15:04:05Z"

// leaseExpiryShape is the exact text mors returns: time.Parse alone would also
// take a one-digit hour or fractional seconds, which mors would hand back in a
// different form and so plan a change on every refresh.
var leaseExpiryShape = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}Z$`)

// invalidLeaseExpiry reports why s is not an expiry mors accepts and echoes
// back unchanged, or "" when it is.
func invalidLeaseExpiry(s string) string {
	if !leaseExpiryShape.MatchString(s) {
		return fmt.Sprintf("%q must be a UTC time with whole seconds, written like 2026-12-31T23:59:59Z: that is "+
			"the only form mors accepts and the form it returns.", s)
	}
	if _, err := time.Parse(leaseExpiryLayout, s); err != nil {
		return fmt.Sprintf("%q is not a valid date and time: %v", s, err)
	}
	return ""
}

// leaseExpiryValidator applies invalidLeaseExpiry at plan time.
type leaseExpiryValidator struct{}

func (leaseExpiryValidator) Description(context.Context) string {
	return "must be a UTC time with whole seconds, written like 2026-12-31T23:59:59Z"
}

func (v leaseExpiryValidator) MarkdownDescription(ctx context.Context) string {
	return v.Description(ctx)
}

func (leaseExpiryValidator) ValidateString(_ context.Context, req validator.StringRequest, resp *validator.StringResponse) {
	if req.ConfigValue.IsNull() || req.ConfigValue.IsUnknown() {
		return
	}
	if msg := invalidLeaseExpiry(req.ConfigValue.ValueString()); msg != "" {
		resp.Diagnostics.AddAttributeError(req.Path, "Invalid lease expiry", msg)
	}
}

// leaseWithinPolicy returns an error when expiry is later than now plus the
// project's policy duration. mors accepts such a lease when it is created but
// then refuses every update that leaves the expiry beyond one policy period,
// even one that changes only action. The PCD UI caps a custom lease at one additional lease period for the same
// reason.
func leaseWithinPolicy(expiry string, now time.Time, policyMinutes int64) error {
	at, err := time.Parse(leaseExpiryLayout, expiry)
	if err != nil {
		return err
	}
	limit := now.UTC().Truncate(time.Second).Add(time.Duration(policyMinutes) * time.Minute)
	if at.After(limit) {
		return fmt.Errorf("expiry %s is more than one lease period (%d minutes, the project's policy) from now. "+
			"mors would accept it, but then refuses every update that leaves the expiry beyond one policy period. "+
			"Choose an expiry no later than %s", expiry, policyMinutes, limit.Format(leaseExpiryLayout))
	}
	return nil
}

// NewInstanceLeaseResource is the factory registered with the provider.
func NewInstanceLeaseResource() resource.Resource {
	return &instanceLeaseResource{}
}

type instanceLeaseResource struct {
	config *clients.Config
}

type instanceLeaseModel struct {
	ID         types.String `tfsdk:"id"`
	InstanceID types.String `tfsdk:"instance_id"`
	ProjectID  types.String `tfsdk:"project_id"`
	Expiry     types.String `tfsdk:"expiry"`
	Action     types.String `tfsdk:"action"`
}

// instanceLeaseAPI is mors's per-VM lease. The GET response carries
// tenant_uuid; the write body leaves it out, because mors takes the project
// from the URL.
type instanceLeaseAPI struct {
	InstanceUUID string `json:"instance_uuid"`
	TenantUUID   string `json:"tenant_uuid,omitempty"`
	Expiry       string `json:"expiry"`
	Action       string `json:"action"`
}

// checkLeaseIDs refuses an empty or malformed project or instance ID before
// any request: either would send the request to a route that is not this
// lease. An ID unknown at plan skips the schema's validators, so "" can reach
// apply.
func checkLeaseIDs(projectID, instanceID string) error {
	if err := checkID("project_id", projectID); err != nil {
		return err
	}
	return checkID("instance_id", instanceID)
}

func instanceLeaseURL(client *gophercloud.ServiceClient, projectID, instanceID string) string {
	return client.ServiceURL("tenant", projectID, "instance", instanceID)
}

// instanceLeaseBody builds the POST and PUT body. mors updates the VM named by
// the body's instance_uuid, not the one in the URL, so both come from the same
// instance ID.
func instanceLeaseBody(instanceID, expiry, action string) instanceLeaseAPI {
	return instanceLeaseAPI{InstanceUUID: instanceID, Expiry: expiry, Action: action}
}

// getInstanceLease reads a VM's own lease. A 200 that holds no lease is
// clients.ErrNoObject, not a lease with no expiry and no action.
func getInstanceLease(ctx context.Context, client *gophercloud.ServiceClient, projectID, instanceID string) (*instanceLeaseAPI, error) {
	if err := checkLeaseIDs(projectID, instanceID); err != nil {
		return nil, err
	}
	var out instanceLeaseAPI
	if err := getJSON(ctx, client, instanceLeaseURL(client, projectID, instanceID), &out); err != nil {
		return nil, err
	}
	if out.InstanceUUID == "" {
		return nil, clients.ErrNoObject
	}
	return &out, nil
}

func writeInstanceLease(ctx context.Context, client *gophercloud.ServiceClient, method, projectID, instanceID, expiry, action string) error {
	if err := checkLeaseIDs(projectID, instanceID); err != nil {
		return err
	}
	return writeJSON(ctx, client, method, instanceLeaseURL(client, projectID, instanceID), instanceLeaseBody(instanceID, expiry, action))
}

func instanceLeaseID(projectID, instanceID string) string {
	return projectID + "/" + instanceID
}

func (r *instanceLeaseResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_compute_instance_lease"
}

func (r *instanceLeaseResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Manages a VM's own lease in mors, PCD's VM lease service: a fixed time at which mors " +
			"powers the VM off or deletes it, in place of the project's `pcd_compute_lease_policy`. This is the PCD " +
			"UI's **Customize Lease** on a VM.\n\n" +
			"The VM's project must have a lease policy; create the policy in the same configuration and reference " +
			"it through `project_id` so it is created first and destroyed last. The policy therefore exists " +
			"before the lease does, so a VM already older than the policy has no lease for a moment and mors may " +
			"act on it first: to protect such a VM, apply the policy with a duration longer than the VM's age, " +
			"add the lease, and shorten the policy in a later apply. `expiry` may be at most one policy " +
			"period from the time of the apply, as in the PCD UI. Write it as a fixed time, or with the " +
			"`hashicorp/time` provider's `time_offset`; `timeadd(timestamp(), ...)` changes on every plan.\n\n" +
			"~> mors checks leases about once a minute. Destroying this resource removes the VM's own lease, and " +
			"the VM falls back to the project's policy, counted from its creation: a VM older than the policy is " +
			"powered off or deleted within a minute. After a `power off` lease runs out, mors powers the VM off " +
			"again on every pass: a `pcd_compute_instance` with `power_state = \"active\"` then starts it on " +
			"every apply and mors stops it again, until the lease is extended. A `delete` lease deletes the VM, " +
			"and its `pcd_compute_instance` then plans a new one. Deleting the project's policy also deletes " +
			"this lease, and so does replacing the policy (for example with `-replace` on " +
			"`pcd_compute_lease_policy`, or by renaming the policy's resource without a `moved` block): the VM falls back to the policy until the next apply writes the lease again.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "`<project_id>/<instance_id>`.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"instance_id": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "The VM the lease is for. For a VM this configuration does not manage, look it " +
					"up with the `pcd_compute_instance` data source. Changing this forces a new resource.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
				Validators:    []validator.String{stringvalidator.RegexMatches(idShape, idShapeMessage)},
			},
			"project_id": schema.StringAttribute{
				Optional: true,
				Computed: true,
				MarkdownDescription: "The VM's project. Read from the VM when omitted; when set, it must be the " +
					"VM's project, which the provider checks before creating the lease, because mors does not. " +
					"Setting it to `pcd_compute_lease_policy.<name>.project_id` orders the lease after the " +
					"policy. Changing this forces a new resource.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown(), stringplanmodifier.RequiresReplace()},
				Validators:    []validator.String{stringvalidator.RegexMatches(idShape, idShapeMessage)},
			},
			"expiry": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "When the lease runs out, as a UTC time with whole seconds: " +
					"`2026-12-31T23:59:59Z`. Must be in the future, and no later than one policy period from the " +
					"time of the apply. Updated in place.",
				Validators: []validator.String{leaseExpiryValidator{}},
			},
			"action": schema.StringAttribute{
				Optional: true,
				Computed: true,
				Default:  stringdefault.StaticString(defaultLeaseAction),
				MarkdownDescription: "What mors does when the lease runs out: `power off` (the default) or " +
					"`delete`. Updated in place, which mors refuses once `expiry` has passed.",
				Validators: []validator.String{stringvalidator.OneOf(leaseActions...)},
			},
		},
	}
}

func (r *instanceLeaseResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.config = configureClient(req.ProviderData, &resp.Diagnostics)
}

func (r *instanceLeaseResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan instanceLeaseModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	instanceID := plan.InstanceID.ValueString()
	// /servers/ is Nova's list route, not a VM, and a malformed ID would
	// address another route.
	if err := checkID("instance_id", instanceID); err != nil {
		resp.Diagnostics.AddAttributeError(path.Root("instance_id"), "mors: creating instance lease",
			fmt.Sprintf("%v. Set instance_id to the ID of the VM the lease is for.", err))
		return
	}
	if !plan.ProjectID.IsNull() && !plan.ProjectID.IsUnknown() {
		if err := checkID("project_id", plan.ProjectID.ValueString()); err != nil {
			resp.Diagnostics.AddAttributeError(path.Root("project_id"), "mors: creating instance lease",
				fmt.Sprintf("%v. Set project_id to the VM's project, or omit it.", err))
			return
		}
	}

	// mors never checks that the VM exists or belongs to the project in the
	// URL, and its scheduler acts on the lease as the mors service user under
	// that project's policy. Nova is the only place the owner can be checked.
	compute, err := r.config.ComputeV2Client()
	if err != nil {
		resp.Diagnostics.AddError("mors: building compute client", err.Error())
		return
	}
	// Extract never returns nil: a 200 without the server would give project "".
	server, err := clients.RequireObject(servers.Get(ctx, compute, instanceID).Extract())
	if err == nil && (server.ID == "" || server.TenantID == "") {
		err = clients.ErrNoObject
	}
	if err != nil {
		if gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
			resp.Diagnostics.AddAttributeError(path.Root("instance_id"), "mors: creating instance lease",
				fmt.Sprintf("Instance %s does not exist, or the provider's credentials cannot see it.", instanceID))
			return
		}
		resp.Diagnostics.AddError("mors: reading the instance", err.Error())
		return
	}
	if server.ID != instanceID {
		resp.Diagnostics.AddAttributeError(path.Root("instance_id"), "mors: creating instance lease", fmt.Sprintf(
			"mors: Nova answered the request for instance %s with server %s. Set instance_id to the VM's ID as "+
				"Nova returns it (%s).", instanceID, server.ID, server.ID))
		return
	}
	projectID := server.TenantID
	if !plan.ProjectID.IsNull() && !plan.ProjectID.IsUnknown() && plan.ProjectID.ValueString() != projectID {
		resp.Diagnostics.AddAttributeError(path.Root("project_id"), "mors: creating instance lease", fmt.Sprintf(
			"Instance %s belongs to project %s, not %s. mors would file the lease under %s without checking, "+
				"so the provider refuses it. Set project_id to the VM's project, or omit it.",
			instanceID, projectID, plan.ProjectID.ValueString(), plan.ProjectID.ValueString()))
		return
	}

	client, err := r.config.MorsV1Client()
	if err != nil {
		resp.Diagnostics.AddError("mors: building client", err.Error())
		return
	}
	policy, err := getLeasePolicy(ctx, client, projectID)
	if err != nil {
		if isNotFound(err) {
			resp.Diagnostics.AddError("mors: creating instance lease", fmt.Sprintf(
				"Project %s has no lease policy. mors acts on a VM's own lease only in a project that has one; "+
					"create it with pcd_compute_lease_policy and set this lease's project_id from it, so the "+
					"policy is created first.", projectID))
			return
		}
		resp.Diagnostics.AddError("mors: reading the project's lease policy", err.Error())
		return
	}
	if err := leaseWithinPolicy(plan.Expiry.ValueString(), time.Now(), policy.ExpiryMins); err != nil {
		resp.Diagnostics.AddAttributeError(path.Root("expiry"), "mors: creating instance lease", err.Error())
		return
	}

	if err := writeInstanceLease(ctx, client, http.MethodPost, projectID, instanceID, plan.Expiry.ValueString(), plan.Action.ValueString()); err != nil {
		if isConflict(err) {
			id := instanceLeaseID(projectID, instanceID)
			resp.Diagnostics.AddError("mors: creating instance lease", fmt.Sprintf(
				"Instance %s already has its own lease, set outside this configuration (with the PCD UI's "+
					"Customize Lease, for example) or by an earlier apply whose answer was lost. Import it to manage it here: terraform import <resource address> %s",
				instanceID, id))
			return
		}
		resp.Diagnostics.AddError("mors: creating instance lease", err.Error())
		return
	}

	plan.ProjectID = types.StringValue(projectID)
	plan.ID = types.StringValue(instanceLeaseID(projectID, instanceID))
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *instanceLeaseResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state instanceLeaseModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := r.config.MorsV1Client()
	if err != nil {
		resp.Diagnostics.AddError("mors: building client", err.Error())
		return
	}

	instanceID := state.InstanceID.ValueString()
	lease, err := getInstanceLease(ctx, client, state.ProjectID.ValueString(), instanceID)
	if err != nil {
		// Gone: destroyed outside Terraform, removed with the project's
		// policy, or removed with the VM by a "delete" lease that ran out.
		if isNotFound(err) {
			resp.State.RemoveResource(ctx)
			return
		}
		resp.Diagnostics.AddError("mors: reading instance lease", err.Error())
		return
	}

	// The lease's own project is the truth: an update through another
	// project's URL moves a lease, and a move must show as a change.
	if lease.TenantUUID != "" {
		state.ProjectID = types.StringValue(lease.TenantUUID)
	}
	state.ID = types.StringValue(instanceLeaseID(state.ProjectID.ValueString(), instanceID))
	state.Expiry = types.StringValue(lease.Expiry)
	state.Action = types.StringValue(lease.Action)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *instanceLeaseResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan instanceLeaseModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := r.config.MorsV1Client()
	if err != nil {
		resp.Diagnostics.AddError("mors: building client", err.Error())
		return
	}

	projectID, instanceID := plan.ProjectID.ValueString(), plan.InstanceID.ValueString()
	// mors refuses an expiry in the past or beyond one policy period with a
	// 422 whose body says which; the error carries it.
	if err := writeInstanceLease(ctx, client, http.MethodPut, projectID, instanceID, plan.Expiry.ValueString(), plan.Action.ValueString()); err != nil {
		resp.Diagnostics.AddError("mors: updating instance lease", err.Error())
		return
	}
	// A PUT for a lease mors does not hold answers 200 and changes nothing.
	if _, err := getInstanceLease(ctx, client, projectID, instanceID); err != nil {
		if isNotFound(err) {
			resp.Diagnostics.AddError("mors: updating instance lease", fmt.Sprintf(
				"Instance %s no longer has its own lease: it was removed outside Terraform, and mors ignores an "+
					"update to a lease it does not hold. The next plan creates it again.", instanceID))
			return
		}
		resp.Diagnostics.AddError("mors: reading instance lease after the update", err.Error())
		return
	}

	plan.ID = types.StringValue(instanceLeaseID(projectID, instanceID))
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *instanceLeaseResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state instanceLeaseModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	// An empty or malformed project or instance ID names no lease, and the URL
	// it would build is not one to delete.
	projectID, instanceID := state.ProjectID.ValueString(), state.InstanceID.ValueString()
	if checkLeaseIDs(projectID, instanceID) != nil {
		return
	}

	client, err := r.config.MorsV1Client()
	if err != nil {
		resp.Diagnostics.AddError("mors: building client", err.Error())
		return
	}

	url := instanceLeaseURL(client, projectID, instanceID)
	if err := deleteObject(ctx, client, url); err != nil {
		resp.Diagnostics.AddError("mors: deleting instance lease", err.Error())
	}
}

// ImportState takes `<project_id>/<instance_id>`. Read fills in the rest from
// mors, so import needs no Nova lookup.
func (r *instanceLeaseResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	parts := strings.SplitN(req.ID, "/", 2)
	if len(parts) != 2 {
		resp.Diagnostics.AddError("Invalid import ID", fmt.Sprintf("expected <project_id>/<instance_id>, got %q", req.ID))
		return
	}
	// SplitN keeps any further '/' in the instance part, which checkID refuses.
	if err := checkLeaseIDs(parts[0], parts[1]); err != nil {
		resp.Diagnostics.AddError("Invalid import ID", fmt.Sprintf("expected <project_id>/<instance_id>, got %q: %v", req.ID, err))
		return
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), req.ID)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("project_id"), parts[0])...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("instance_id"), parts[1])...)
}
