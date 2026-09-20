// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package resmgr

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/hashicorp/terraform-plugin-framework/diag"
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
	Settings           types.Map    `tfsdk:"settings"`
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
				MarkdownDescription: "For `persistent-storage` only: the storage backend configurations to enable on this host, " +
					"named by the second-level keys of the cluster blueprint's `storage_backends_json` (the configuration names " +
					"under each backend, not the top-level backend names). Each becomes a Cinder backend section on the host; " +
					"the top-level backend name is what a volume type's `volume_backend_name` refers to."},
			"host_cluster": schema.StringAttribute{Optional: true,
				MarkdownDescription: "For `hypervisor` only: the host cluster (host aggregate) to join."},
			"settings": schema.MapAttribute{Optional: true, ElementType: types.StringType,
				MarkdownDescription: "For `dns` only: overrides for the settings of the granular `pf9-designate` role the cluster " +
					"role expands into, written through the resource-manager v1 role API once the cluster role is assigned " +
					"and again whenever it is re-assigned. Keys are the role's setting names as the PCD API reports them; " +
					"`listen = \"[::]:5354\"` makes designate-mdns serve zone transfers over IPv6 as well as IPv4 (the host's " +
					"`net.ipv6.bindv6only` must be `0`, the Linux default). Only the keys listed here are managed: the rest " +
					"keep the values PCD computes, and a key removed from this map keeps its last value until set again. " +
					"Values are always sent as strings, even for a setting the resource manager holds as a number or a " +
					"boolean. The resource manager keeps the role's settings after the `dns` role is removed, and a later " +
					"assignment briefly reports the old values until the role's defaults apply, so setting `settings` " +
					"makes create wait for the role to converge before it writes them, whether or not " +
					"`wait_until_converged` is set, and create always writes them. An update writes them only when a " +
					"managed key is missing from what the resource manager holds or differs from it, so the first apply " +
					"after an import writes nothing when the values already match, and retries while the resource " +
					"manager refuses role changes during convergence. The host agent then restarts designate-mdns, which " +
					"typically moves to the new address within seconds to a minute of the write; `wait_until_converged` " +
					"does not wait for that restart."},
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
	if !cfg.Settings.IsNull() && role != "dns" && !cfg.Role.IsUnknown() {
		resp.Diagnostics.AddAttributeError(path.Root("settings"), "settings requires role = \"dns\"",
			fmt.Sprintf("settings overrides pf9-designate's role settings and does not apply to %q.", role))
	}
}

func (r *hostClusterRoleResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.config = configureClient(req.ProviderData, &resp.Diagnostics)
}

// assignBody builds the v2 PUT body for the role, for Create and Update. The UI
// sends {} for roles with no options, so absent options are an empty object
// rather than no body.
func (r *hostClusterRoleResource) assignBody(ctx context.Context, m *hostClusterRoleModel, diags *diag.Diagnostics) map[string]any {
	body := map[string]any{}
	if m.Role.ValueString() == "hypervisor" && !m.HostCluster.IsNull() && m.HostCluster.ValueString() != "" {
		body["hostcluster"] = m.HostCluster.ValueString()
	}
	if m.Role.ValueString() == "persistent-storage" && !m.Backends.IsNull() && !m.Backends.IsUnknown() {
		var backends []string
		diags.Append(m.Backends.ElementsAs(ctx, &backends, false)...)
		body["backends"] = backends
	}
	return body
}

// roleOptionsChanged reports whether the PUT body assignBody would build from
// plan differs from what the server already holds according to state: the
// hypervisor's host_cluster or persistent-storage's backends. Everything else
// on the resource is client-side (wait_until_converged) or ForceNew, so a false
// here means resmgr has nothing to receive — and a repeated PUT is a real write
// resmgr acts on, not a no-op. A new server-side option sent on the v2 PUT must
// be added here too, or changes to it would be skipped; `settings` is written
// through v1 and has its own check, `settingsChanged`.
//
// The comparison is deliberately role-agnostic: it weighs both host_cluster
// and backends no matter which role is in play, even though assignBody only
// ever sends the one that matches. role is ForceNew and ValidateConfig pins
// each option to its role, so the option-to-role pairing can never drift
// within one plan/state pair — any mismatch this misses can only err toward
// sending the PUT, never toward silently skipping one.
func roleOptionsChanged(plan, state *hostClusterRoleModel) bool {
	if hostClusterOption(plan) != hostClusterOption(state) {
		return true
	}
	return !backendsOption(plan).Equal(backendsOption(state))
}

// hostClusterOption is host_cluster as assignBody sends it: null, unknown and
// "" are all "omitted".
func hostClusterOption(m *hostClusterRoleModel) string {
	if m.HostCluster.IsNull() || m.HostCluster.IsUnknown() {
		return ""
	}
	return m.HostCluster.ValueString()
}

// backendsOption is backends as assignBody sends it: null and unknown are both
// "omitted"; an empty list is sent as [] and so is a value in its own right.
func backendsOption(m *hostClusterRoleModel) types.List {
	if m.Backends.IsNull() || m.Backends.IsUnknown() {
		return types.ListNull(types.StringType)
	}
	return m.Backends
}

// settingsOption is settings as the resource manages them: null and unknown
// both mean "nothing managed".
func settingsOption(m *hostClusterRoleModel) types.Map {
	if m.Settings.IsNull() || m.Settings.IsUnknown() {
		return types.MapNull(types.StringType)
	}
	return m.Settings
}

// settingsChanged reports whether the managed settings differ between plan
// and state. They are written through resmgr v1, apart from the v2 assignment
// roleOptionsChanged guards, so the two are kept separate: a settings-only
// change must not re-PUT the cluster role.
func settingsChanged(plan, state *hostClusterRoleModel) bool {
	return !settingsOption(plan).Equal(settingsOption(state))
}

// managedSettings is the plan's settings as a Go map; nil when none.
func managedSettings(ctx context.Context, m *hostClusterRoleModel, diags *diag.Diagnostics) map[string]string {
	if m.Settings.IsNull() || m.Settings.IsUnknown() {
		return nil
	}
	out := map[string]string{}
	diags.Append(m.Settings.ElementsAs(ctx, &out, false)...)
	return out
}

// mergeSettings returns the body for a v1 role PUT: everything resmgr holds
// with the managed keys overwritten. The PUT replaces the whole settings
// object, whose other keys the uber-role expansion computed (for
// pf9-designate, `debug` alongside `listen`), so a body holding only the
// overrides would drop them.
func mergeSettings(current map[string]any, managed map[string]string) map[string]any {
	out := make(map[string]any, len(current)+len(managed))
	for k, v := range current {
		out[k] = v
	}
	for k, v := range managed {
		out[k] = v
	}
	return out
}

// readSettings narrows the role's current settings to the managed keys, as
// strings, so state compares against exactly what the configuration set. A
// managed key resmgr no longer reports is left out, which plans a re-apply.
func readSettings(current map[string]any, managed map[string]string) map[string]string {
	out := make(map[string]string, len(managed))
	for k := range managed {
		if v, ok := current[k]; ok {
			out[k] = settingString(v)
		}
	}
	return out
}

// settingsApplied reports whether resmgr already holds every managed setting,
// compared as Read compares them: readSettings(current, managed) must equal
// managed, the same keys with the same string values. Only Update relies on
// it; on Create the values resmgr reports can be stale (see applySettings).
func settingsApplied(current map[string]any, managed map[string]string) bool {
	held := readSettings(current, managed)
	if len(held) != len(managed) {
		return false
	}
	for k, v := range managed {
		if held[k] != v {
			return false
		}
	}
	return true
}

// settingString renders a JSON scalar the way a user writes it in HCL.
func settingString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case bool:
		return strconv.FormatBool(t)
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case nil:
		return ""
	default:
		b, _ := json.Marshal(t)
		return string(b)
	}
}

// waitRoleSettings polls the v1 per-host role until the endpoint answers at
// all, and returns the settings it reports. A PUT to a granular role resmgr
// does not report would assign it directly, with only these settings. An
// answer does not mean the current assignment's expansion has landed: resmgr
// keeps answering for a host that carried the role before, and a new
// assignment can return a previous assignment's settings until the expansion
// resets them. That is why Create waits for convergence before it writes.
func waitRoleSettings(ctx context.Context, client *gophercloud.ServiceClient, url string) (map[string]any, error) {
	deadline := time.Now().Add(10 * time.Minute)
	for {
		var current map[string]any
		err := getJSON(ctx, client, url, &current)
		if err == nil {
			return current, nil
		}
		if !isNotFound(err) {
			return nil, err
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("resmgr did not report the granular role at %s within 10 minutes of the cluster role being assigned", url)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
}

// applySettings writes the managed settings onto the cluster role's granular
// marker role through resmgr v1, merged over what resmgr currently holds. The
// write goes through putRole, which retries 409 RoleUpdateConflict for up to
// ten minutes: resmgr refuses role writes while the host converges. Create
// waits for convergence before it calls this, and so does an Update that
// re-assigns the role with wait_until_converged set; the retry is the
// fallback for an Update that does not wait.
//
// Without force, when resmgr already holds every managed key with its
// configured value there is nothing to write, and no PUT is sent: a PUT is a
// real write the host agent acts on by restarting designate-mdns. That is the
// case of the first apply after an import, where state has no settings yet.
//
// Create passes force, so it always writes. resmgr keeps a host's
// pf9-designate settings after the dns role is removed, and a new assignment
// reports those old values until the expansion resets the role to its
// defaults. What Create reads can therefore match the configuration only
// because it is left over from an earlier assignment, and skipping the write
// could leave the role at its defaults.
func (r *hostClusterRoleResource) applySettings(ctx context.Context, hostID, role string, managed map[string]string, force bool) error {
	if len(managed) == 0 {
		return nil
	}
	clientV1, err := r.config.ResmgrV1Client()
	if err != nil {
		return err
	}
	url := clientV1.ServiceURL("hosts", hostID, "roles", clusterRoleMarkers[role])
	current, err := waitRoleSettings(ctx, clientV1, url)
	if err != nil {
		return err
	}
	if !force && settingsApplied(current, managed) {
		return nil
	}
	return r.putRole(ctx, clientV1, url, mergeSettings(current, managed))
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
	// Decode settings before anything is written: failing after the assignment
	// would leave the role assigned in resmgr and absent from state.
	managed := managedSettings(ctx, &plan, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := r.config.ResmgrV2Client()
	if err != nil {
		resp.Diagnostics.AddError("resmgr: building client", err.Error())
		return
	}

	hostID, role := plan.HostID.ValueString(), plan.Role.ValueString()
	body := r.assignBody(ctx, &plan, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}

	if err := r.putRole(ctx, client, client.ServiceURL("hosts", hostID, "roles", role), body); err != nil {
		resp.Diagnostics.AddError("resmgr: assigning cluster role", err.Error())
		return
	}
	plan.ID = types.StringValue(hostID + "/" + role)

	// Settings wait for the role to converge whether or not the flag is set:
	// until the expansion resets the role, resmgr can report, and keep, a
	// previous assignment's settings, and a write that lands before the reset
	// is lost to it. One wait serves both.
	if plan.WaitUntilConverged.ValueBool() || len(managed) > 0 {
		if err := r.waitConverged(ctx, hostID, role); err != nil {
			// The role is assigned in resmgr, so it is recorded, with settings
			// unset since they were not written yet. A create that errors with
			// a non-null state leaves the resource tainted, so the next apply
			// replaces it (removes the role and assigns it again) unless the
			// user untaints it. Untainted, the next apply writes any configured
			// settings through Update and does not wait for convergence again.
			plan.Settings = types.MapNull(types.StringType)
			resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
			resp.Diagnostics.AddError("resmgr: waiting for host convergence", err.Error()+"\n\n"+
				"The role is assigned, but Terraform marks it tainted and the next apply would remove and assign it again; "+
				"`terraform untaint <address>` keeps it, and the next apply then writes any configured settings "+
				"without waiting for convergence again.")
			return
		}
	}
	// Settings go on last, after convergence. Forced: what resmgr reports for a
	// new assignment can be stale (see applySettings).
	if err := r.applySettings(ctx, hostID, role, managed, true); err != nil {
		// The role is assigned in resmgr, so it is recorded, with settings
		// unset since the write failed. A create that errors with a non-null
		// state leaves the resource tainted, so the next apply replaces it
		// (removes the role and assigns it again) unless the user untaints it.
		// Untainted, the next apply retries only the settings write.
		plan.Settings = types.MapNull(types.StringType)
		resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
		resp.Diagnostics.AddError("resmgr: applying role settings", err.Error()+"\n\n"+
			"The role is assigned, but Terraform marks it tainted and the next apply would remove and assign it again; "+
			"`terraform untaint <address>` keeps it, and the next apply then retries only the settings write.")
		return
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
	if managed := managedSettings(ctx, &state, &resp.Diagnostics); len(managed) > 0 {
		clientV1, err := r.config.ResmgrV1Client()
		if err != nil {
			resp.Diagnostics.AddError("resmgr: building client", err.Error())
			return
		}
		var current map[string]any
		err = getJSON(ctx, clientV1, clientV1.ServiceURL("hosts", state.HostID.ValueString(), "roles", clusterRoleMarkers[state.Role.ValueString()]), &current)
		switch {
		case isNotFound(err):
			// The granular role is not visible (mid-expansion or the deauth
			// window); keep the last known settings rather than plan a rewrite.
		case err != nil:
			resp.Diagnostics.AddError("resmgr: reading role settings", err.Error())
			return
		default:
			m, d := types.MapValueFrom(ctx, types.StringType, readSettings(current, managed))
			resp.Diagnostics.Append(d...)
			state.Settings = m
		}
	}
	state.ID = types.StringValue(state.HostID.ValueString() + "/" + state.Role.ValueString())
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

// Update re-PUTs the assignment when a server-side option changed: backends and
// host_cluster are options on the same role. settings is written separately,
// through resmgr v1, when it changed or the role was re-PUT, after any wait for
// convergence, and only when a managed key differs from what resmgr holds.
// wait_until_converged is client-side only, and when it is the only thing that
// changed there is nothing to send — a repeated PUT is a real write resmgr acts
// on, not a no-op.
func (r *hostClusterRoleResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state hostClusterRoleModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	optionsChanged := roleOptionsChanged(&plan, &state)
	if !optionsChanged && !settingsChanged(&plan, &state) {
		plan.ID = types.StringValue(plan.HostID.ValueString() + "/" + plan.Role.ValueString())
		resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
		return
	}
	// Decode settings before anything is written, as in Create.
	managed := managedSettings(ctx, &plan, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}

	hostID, role := plan.HostID.ValueString(), plan.Role.ValueString()
	if optionsChanged {
		client, err := r.config.ResmgrV2Client()
		if err != nil {
			resp.Diagnostics.AddError("resmgr: building client", err.Error())
			return
		}
		body := r.assignBody(ctx, &plan, &resp.Diagnostics)
		if resp.Diagnostics.HasError() {
			return
		}
		if err := r.putRole(ctx, client, client.ServiceURL("hosts", hostID, "roles", role), body); err != nil {
			resp.Diagnostics.AddError("resmgr: updating cluster role", err.Error())
			return
		}
	}
	plan.ID = types.StringValue(hostID + "/" + role)

	if plan.WaitUntilConverged.ValueBool() && optionsChanged {
		if err := r.waitConverged(ctx, hostID, role); err != nil {
			plan.Settings = state.Settings
			resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
			resp.Diagnostics.AddError("resmgr: waiting for host convergence", err.Error())
			return
		}
	}
	// Settings go on last, after any re-assignment has converged: the
	// expansion may have reset them, and resmgr refuses the write meanwhile.
	if err := r.applySettings(ctx, hostID, role, managed, false); err != nil {
		plan.Settings = state.Settings
		resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
		resp.Diagnostics.AddError("resmgr: applying role settings", err.Error())
		return
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
	// wait_until_converged is client-side only and has no server value to read
	// back. Left null, the schema default plans `null -> false` on every
	// imported role and applying that re-PUTs the role. Write the default now so
	// the imported state already matches what the plan will hold.
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("wait_until_converged"), false)...)
}
