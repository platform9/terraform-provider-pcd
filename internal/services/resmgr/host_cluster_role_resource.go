// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package resmgr

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/platform9/terraform-provider-pcd/internal/clients"
)

var (
	_ resource.Resource                   = (*hostClusterRoleResource)(nil)
	_ resource.ResourceWithConfigure      = (*hostClusterRoleResource)(nil)
	_ resource.ResourceWithImportState    = (*hostClusterRoleResource)(nil)
	_ resource.ResourceWithValidateConfig = (*hostClusterRoleResource)(nil)
)

// clusterRoles are the resmgr v2 "uber-roles". Assigning one makes the SERVER
// expand it into the granular pf9-* roles with per-role settings computed from
// the cluster blueprint and the host's host config — persistent-storage, for
// example, resolves its named backends against blueprint.storageBackends. This
// is the API the PCD UI onboards hosts with, and the only one that yields a
// converging host without hand-supplied role settings (see pcd_host_role for
// the low-level granular alternative and its hazards).
var clusterRoles = map[string]bool{
	"hypervisor":         true,
	"image-library":      true,
	"persistent-storage": true,
	"dns":                true,
}

// clusterRoleMarkers names one granular role each cluster role expands into,
// used by waitConverged to verify THIS role's convergence rather than only the
// host aggregate. role_status is computed over the roles assigned at that
// moment, so during onboarding there is a window where it reads "ok" before a
// concurrently-assigned cluster role's PUT has landed — an aggregate-only wait
// returns early and un-gates downstream resources (e.g. an image upload
// against a Glance that is not serving yet). (KVM markers; the VMware
// variants are out of scope for this provider today.)
var clusterRoleMarkers = map[string]string{
	"hypervisor":         "pf9-ostackhost-neutron",
	"image-library":      "pf9-glance-role",
	"persistent-storage": "pf9-cindervolume-base",
	"dns":                "pf9-designate",
}

// NewHostClusterRoleResource is the factory registered with the provider.
func NewHostClusterRoleResource() resource.Resource {
	return &hostClusterRoleResource{}
}

type hostClusterRoleResource struct {
	config *clients.Config
}

type hostClusterRoleModel struct {
	ID                 types.String `tfsdk:"id"`
	HostID             types.String `tfsdk:"host_id"`
	Role               types.String `tfsdk:"role"`
	Backends           types.List   `tfsdk:"backends"`
	HostCluster        types.String `tfsdk:"host_cluster"`
	WaitUntilConverged types.Bool   `tfsdk:"wait_until_converged"`
}

func (r *hostClusterRoleResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_host_cluster_role"
}

func (r *hostClusterRoleResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	forceNew := []planmodifier.String{stringplanmodifier.RequiresReplace()}
	resp.Schema = schema.Schema{
		MarkdownDescription: "Assigns a PCD **cluster role** (resmgr v2 uber-role) to an onboarded host: " +
			"`hypervisor`, `image-library`, `persistent-storage`, or `dns`. The PCD control plane expands the " +
			"cluster role into its granular `pf9-*` roles and computes their settings from the cluster blueprint " +
			"and the host's host configuration, so the host converges without hand-written role settings. This is " +
			"the resource to onboard hosts with; `pcd_host_role` is the low-level granular API underneath it.",
		Attributes: map[string]schema.Attribute{
			"id":      schema.StringAttribute{Computed: true, MarkdownDescription: "The composite `<host_id>/<role>` ID.", PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()}},
			"host_id": schema.StringAttribute{Required: true, MarkdownDescription: "The resmgr host UUID. Changing this forces a new resource.", PlanModifiers: forceNew},
			"role": schema.StringAttribute{Required: true, MarkdownDescription: "The cluster role: `hypervisor`, `image-library`, `persistent-storage`, or `dns`. " +
				"Changing this forces a new resource.", PlanModifiers: forceNew},
			"backends": schema.ListAttribute{Optional: true, ElementType: types.StringType,
				MarkdownDescription: "For `persistent-storage` only: the storage backend names to enable on this host, " +
					"as named in the cluster blueprint's `storage_backends_json` (its top-level keys)."},
			"host_cluster": schema.StringAttribute{Optional: true,
				MarkdownDescription: "For `hypervisor` only: the host cluster (host aggregate) to join."},
			"wait_until_converged": schema.BoolAttribute{Optional: true, Computed: true, Default: booldefault.StaticBool(false),
				MarkdownDescription: "Wait until the host reports `role_status = ok` before completing. Role convergence " +
					"installs and configures services on the host and typically takes several minutes. Enable this when " +
					"later resources in the same configuration need the host operational (e.g. booting an instance on a " +
					"freshly onboarded hypervisor)."},
		},
	}
}

// ValidateConfig enforces the per-role attribute pairings the API silently
// ignores: backends belongs to persistent-storage, host_cluster to hypervisor.
func (r *hostClusterRoleResource) ValidateConfig(ctx context.Context, req resource.ValidateConfigRequest, resp *resource.ValidateConfigResponse) {
	var cfg hostClusterRoleModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &cfg)...)
	if resp.Diagnostics.HasError() {
		return
	}
	role := cfg.Role.ValueString()
	if !cfg.Role.IsNull() && !cfg.Role.IsUnknown() && !clusterRoles[role] {
		resp.Diagnostics.AddAttributeError(path.Root("role"), "Invalid cluster role",
			fmt.Sprintf("%q is not a cluster role. Valid roles: hypervisor, image-library, persistent-storage, dns. "+
				"For granular pf9-* roles use pcd_host_role.", role))
	}
	if !cfg.Backends.IsNull() && role != "persistent-storage" && !cfg.Role.IsUnknown() {
		resp.Diagnostics.AddAttributeError(path.Root("backends"), "backends requires role = \"persistent-storage\"",
			fmt.Sprintf("backends selects blueprint storage backends and only applies to persistent-storage, not %q.", role))
	}
	if !cfg.HostCluster.IsNull() && role != "hypervisor" && !cfg.Role.IsUnknown() {
		resp.Diagnostics.AddAttributeError(path.Root("host_cluster"), "host_cluster requires role = \"hypervisor\"",
			fmt.Sprintf("host_cluster joins a hypervisor to a host cluster and does not apply to %q.", role))
	}
}

func (r *hostClusterRoleResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.config = configureClient(req.ProviderData, &resp.Diagnostics)
}

// assignBody builds the PUT body for the role. The UI sends {} for roles with
// no options, so absent options are an empty object rather than no body.
func (r *hostClusterRoleResource) assignBody(ctx context.Context, m *hostClusterRoleModel, resp *resource.CreateResponse) map[string]any {
	body := map[string]any{}
	if m.Role.ValueString() == "hypervisor" && !m.HostCluster.IsNull() && m.HostCluster.ValueString() != "" {
		body["hostcluster"] = m.HostCluster.ValueString()
	}
	if m.Role.ValueString() == "persistent-storage" && !m.Backends.IsNull() && !m.Backends.IsUnknown() {
		var backends []string
		resp.Diagnostics.Append(m.Backends.ElementsAs(ctx, &backends, false)...)
		body["backends"] = backends
	}
	return body
}

// putRole PUTs the role assignment, retrying while resmgr answers 409
// RoleUpdateConflict. resmgr rejects role changes while the host is mid-
// convergence, and assigning several cluster roles to one host in a single
// apply makes that window easy to hit; the state clears on its own, so a
// bounded retry is the correct client behavior.
func (r *hostClusterRoleResource) putRole(ctx context.Context, client *gophercloud.ServiceClient, url string, body map[string]any) error {
	const window = 10 * time.Minute
	deadline := time.Now().Add(window)
	for {
		_, err := client.Put(ctx, url, body, nil, &gophercloud.RequestOpts{OkCodes: []int{200, 201, 202, 204}})
		if err == nil || !gophercloud.ResponseCodeIs(err, 409) {
			return err
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("still conflicting after %s (resmgr answers 409 while the host converges): %w", window, err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Second):
		}
	}
}

func (r *hostClusterRoleResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan hostClusterRoleModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := r.config.ResmgrV2Client()
	if err != nil {
		resp.Diagnostics.AddError("resmgr: building client", err.Error())
		return
	}

	hostID, role := plan.HostID.ValueString(), plan.Role.ValueString()
	body := r.assignBody(ctx, &plan, resp)
	if resp.Diagnostics.HasError() {
		return
	}

	if err := r.putRole(ctx, client, client.ServiceURL("hosts", hostID, "roles", role), body); err != nil {
		resp.Diagnostics.AddError("resmgr: assigning cluster role", err.Error())
		return
	}
	plan.ID = types.StringValue(hostID + "/" + role)

	if plan.WaitUntilConverged.ValueBool() {
		if err := r.waitConverged(ctx, hostID, role); err != nil {
			// The assignment itself succeeded: keep the resource in state so a
			// re-apply retries the wait instead of duplicating the assignment.
			resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
			resp.Diagnostics.AddError("resmgr: waiting for host convergence", err.Error())
			return
		}
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

// waitConverged polls the v1 host until role_status settles at ok (or fails).
// v1 is the API that carries role_status and per-role detail.
//
// role_status flaps to "failed" transiently during normal onboarding (the
// hostagent gives up a converge round, resmgr re-issues the desired state, and
// the next round proceeds), so a single failed poll is not terminal — only a
// failed state that persists across consecutive polls is.
func (r *hostClusterRoleResource) waitConverged(ctx context.Context, hostID, role string) error {
	clientV1, err := r.config.ResmgrV1Client()
	if err != nil {
		return err
	}
	const (
		timeout         = 30 * time.Minute
		poll            = 15 * time.Second
		failedThreshold = 8 // consecutive failed polls (2 min) before giving up
	)
	deadline := time.Now().Add(timeout)
	failedStreak := 0
	for {
		var host struct {
			RoleStatus  string            `json:"role_status"`
			RolesStatus map[string]string `json:"roles_status_details"`
		}
		if err := getJSON(ctx, clientV1, clientV1.ServiceURL("hosts", hostID), &host); err != nil {
			return err
		}
		switch host.RoleStatus {
		case "ok":
			// The aggregate alone is not proof: require this cluster role's
			// granular marker to be present and applied too.
			if marker := clusterRoleMarkers[role]; marker == "" || host.RolesStatus[marker] == "applied" || host.RolesStatus[marker] == "ok" {
				return nil
			}
			failedStreak = 0
		case "failed":
			failedStreak++
			if failedStreak >= failedThreshold {
				return fmt.Errorf("host stayed in role_status=failed for %s; per-role states: %v. "+
					"Check /var/log/pf9/hostagent.log on the host — resmgr does not propagate the failing app or reason",
					time.Duration(failedThreshold)*poll, host.RolesStatus)
			}
		default:
			failedStreak = 0
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("host did not reach role_status=ok within %s (last: %s, per-role: %v)", timeout, host.RoleStatus, host.RolesStatus)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(poll):
		}
	}
}

func (r *hostClusterRoleResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state hostClusterRoleModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := r.config.ResmgrV2Client()
	if err != nil {
		resp.Diagnostics.AddError("resmgr: building client", err.Error())
		return
	}

	// The v2 host view reports cluster roles under their uber-role names, so
	// membership is checked directly against the configured role.
	host, known, err := hostRecord(ctx, client, state.HostID.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("resmgr: reading host", err.Error())
		return
	}
	if !known {
		resp.State.RemoveResource(ctx)
		return
	}
	found := false
	for _, r := range host.Roles {
		if r == state.Role.ValueString() {
			found = true
			break
		}
	}
	if !found {
		resp.State.RemoveResource(ctx)
		return
	}
	state.ID = types.StringValue(state.HostID.ValueString() + "/" + state.Role.ValueString())
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

// Update re-PUTs the assignment: backends and host_cluster are options on the
// same role, and wait_until_converged is client-side only.
func (r *hostClusterRoleResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan hostClusterRoleModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := r.config.ResmgrV2Client()
	if err != nil {
		resp.Diagnostics.AddError("resmgr: building client", err.Error())
		return
	}

	hostID, role := plan.HostID.ValueString(), plan.Role.ValueString()
	body := map[string]any{}
	if role == "hypervisor" && !plan.HostCluster.IsNull() && plan.HostCluster.ValueString() != "" {
		body["hostcluster"] = plan.HostCluster.ValueString()
	}
	if role == "persistent-storage" && !plan.Backends.IsNull() && !plan.Backends.IsUnknown() {
		var backends []string
		resp.Diagnostics.Append(plan.Backends.ElementsAs(ctx, &backends, false)...)
		body["backends"] = backends
	}
	if resp.Diagnostics.HasError() {
		return
	}

	if err := r.putRole(ctx, client, client.ServiceURL("hosts", hostID, "roles", role), body); err != nil {
		resp.Diagnostics.AddError("resmgr: updating cluster role", err.Error())
		return
	}
	plan.ID = types.StringValue(hostID + "/" + role)
	if plan.WaitUntilConverged.ValueBool() {
		if err := r.waitConverged(ctx, hostID, role); err != nil {
			resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
			resp.Diagnostics.AddError("resmgr: waiting for host convergence", err.Error())
			return
		}
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *hostClusterRoleResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state hostClusterRoleModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := r.config.ResmgrV2Client()
	if err != nil {
		resp.Diagnostics.AddError("resmgr: building client", err.Error())
		return
	}

	url := client.ServiceURL("hosts", state.HostID.ValueString(), "roles", state.Role.ValueString())
	const window = 10 * time.Minute
	deadline := time.Now().Add(window)
	for {
		_, err := client.Delete(ctx, url, &gophercloud.RequestOpts{OkCodes: []int{200, 202, 204}})
		if err == nil || isNotFound(err) {
			return
		}
		if !gophercloud.ResponseCodeIs(err, 409) {
			resp.Diagnostics.AddError("resmgr: removing cluster role", err.Error())
			return
		}
		if time.Now().After(deadline) {
			resp.Diagnostics.AddError("resmgr: removing cluster role",
				fmt.Sprintf("still conflicting after %s (resmgr answers 409 while the host converges): %s", window, err))
			return
		}
		select {
		case <-ctx.Done():
			resp.Diagnostics.AddError("resmgr: removing cluster role", ctx.Err().Error())
			return
		case <-time.After(10 * time.Second):
		}
	}
}

func (r *hostClusterRoleResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	parts := strings.SplitN(req.ID, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		resp.Diagnostics.AddError("Invalid import ID", fmt.Sprintf("expected <host_id>/<role>, got %q", req.ID))
		return
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), req.ID)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("host_id"), parts[0])...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("role"), parts[1])...)
}
