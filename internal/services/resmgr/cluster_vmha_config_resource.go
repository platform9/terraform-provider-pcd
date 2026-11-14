// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package resmgr

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"time"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/hashicorp/terraform-plugin-framework-validators/resourcevalidator"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/platform9/terraform-provider-pcd/internal/clients"
	"github.com/platform9/terraform-provider-pcd/internal/tfstate"
)

var (
	_ resource.Resource                     = (*clusterVMHAConfigResource)(nil)
	_ resource.ResourceWithConfigure        = (*clusterVMHAConfigResource)(nil)
	_ resource.ResourceWithImportState      = (*clusterVMHAConfigResource)(nil)
	_ resource.ResourceWithConfigValidators = (*clusterVMHAConfigResource)(nil)
)

// vmhaClusterNamePattern is a cluster name that is safe as the last segment of
// /v1/vmha/config/<cluster>: not empty, not `.` or `..` (which a normalized
// path turns into another route), and without a character that ends or
// escapes the path segment. It is checked by the schema, on import, and
// before every request.
var vmhaClusterNamePattern = regexp.MustCompile(`^[^/?#%\s]*[^/?#%\s.][^/?#%\s]*$`)

const vmhaClusterNameRule = "a cluster name that is not empty, not `.` or `..`, and has no `/`, `?`, `#`, `%` or whitespace"

// hamgr checks a write's cluster against its copy of resmgr's cluster list,
// which it rereads every five minutes on some PCD releases, so it refuses a
// write for a cluster created moments ago. The write then waits for hamgr to
// learn of a cluster resmgr already has. These are vars so unit tests can
// shorten them.
var (
	vmhaClusterKnownInterval = 10 * time.Second
	// vmhaClusterKnownTimeout is hamgr's 300-second cache plus a margin.
	vmhaClusterKnownTimeout = 6 * time.Minute
	// vmhaClusterKnownCheckTimeout bounds one retried write, so a request
	// hamgr never answers cannot hold the wait past its deadline.
	vmhaClusterKnownCheckTimeout = 30 * time.Second
)

// vmhaUnknownClusterBody is what hamgr's handler answers, with a 404, for a
// write whose cluster is missing from its copy of resmgr's cluster list.
var vmhaUnknownClusterBody = []byte("not found in resmgr")

// NewClusterVMHAConfigResource is the factory registered with the provider.
func NewClusterVMHAConfigResource() resource.Resource {
	return &clusterVMHAConfigResource{}
}

type clusterVMHAConfigResource struct {
	config *clients.Config
}

type clusterVMHAConfigModel struct {
	ID               types.String `tfsdk:"id"`
	ClusterName      types.String `tfsdk:"cluster_name"`
	EvacuationSpeed  types.String `tfsdk:"evacuation_speed"`
	NFSHeartbeatPath types.String `tfsdk:"nfs_heartbeat_path"`
}

// vmhaConfigAPI is a cluster's VM high-availability configuration as hamgr's
// /v1/vmha/config/<cluster> takes and returns it. hamgr merges a POST into
// the configuration it holds: a key the body leaves out keeps its value, except
// that a body carrying mode also makes hamgr store that mode's cooldown and
// host_config.post_delay. Every field is a pointer with omitempty, and the
// provider sends only what it writes. hamgr holds more keys (cooldown and host
// timeouts among them); this resource manages only these two.
// Observed on PCD 2026.8: after a POST of {"mode":"balanced"} the GET answered
// {"cooldown":45,"mode":"balanced","host_config":{"post_delay":30}} (setting a
// mode makes hamgr store that mode's cooldown and host_config.post_delay), and
// after a POST of only {"host_config":{"nfs_heartbeat_path":"/mnt/x"}} it
// answered {"cooldown":45,"mode":"balanced","host_config":
// {"nfs_heartbeat_path":"/mnt/x","post_delay":30}}: mode and
// host_config.post_delay both survived.
type vmhaConfigAPI struct {
	Mode       *string            `json:"mode,omitempty"`
	HostConfig *vmhaHostConfigAPI `json:"host_config,omitempty"`
}

type vmhaHostConfigAPI struct {
	NFSHeartbeatPath *string `json:"nfs_heartbeat_path,omitempty"`
}

// vmhaNoConfigBody is what hamgr's handler answers, with a 404, for a cluster
// it holds no configuration for. vmhaNoRouteBody is its router's 404 for a
// path it does not serve: a hamgr without the configuration API.
var (
	vmhaNoConfigBody = []byte("no config found for cluster")
	vmhaNoRouteBody  = []byte("404 page not found")
)

func (r *clusterVMHAConfigResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_cluster_vmha_config"
}

func (r *clusterVMHAConfigResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Manages a cluster's VM high-availability settings in hamgr, PCD's VM high-availability " +
			"service: the evacuation speed and the NFS heartbeat path, which the PCD UI's cluster dialogs also set in " +
			"PCD releases whose UI has them. Turning VM high availability on or off stays on `pcd_cluster` " +
			"(`vm_high_availability.enabled`); these settings act while it is on. Set at least one of " +
			"`evacuation_speed` and `nfs_heartbeat_path`.\n\n" +
			"hamgr keeps one configuration per cluster and merges every write into it, so this resource takes over " +
			"whatever the cluster already has: an attribute left out of the configuration keeps the cluster's value " +
			"and reads it back. hamgr has no way to delete a configuration, so destroying this resource leaves the " +
			"settings in PCD as they are, removes them from Terraform state, and warns. hamgr keeps them by cluster " +
			"name, so they also outlive the cluster, and a later cluster of the same name gets them. Writing a " +
			"configuration needs a cluster that PCD's resource manager knows. hamgr can take up to five minutes to " +
			"learn of a cluster that was just created, so when hamgr refuses a write for a cluster the resource " +
			"manager has, the provider retries it for up to six minutes; creating this resource in the same apply as " +
			"its cluster can therefore wait that long.\n\n" +
			"Import works for a cluster whose configuration hamgr already holds. For a cluster with none, import " +
			"fails with \"Cannot import non-existent remote object\"; declare the resource and apply instead, and " +
			"the first write creates the configuration.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{Computed: true, MarkdownDescription: "The cluster name.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()}},
			"cluster_name": schema.StringAttribute{Required: true,
				MarkdownDescription: "The name of the cluster, for example `pcd_cluster.<name>.name`. Changing this forces a new resource.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.RequiresReplace()},
				Validators:          []validator.String{stringvalidator.RegexMatches(vmhaClusterNamePattern, "must be "+vmhaClusterNameRule)}},
			"evacuation_speed": schema.StringAttribute{Optional: true, Computed: true,
				MarkdownDescription: "How long PCD waits, once a host stops responding, before it restarts the host's VMs " +
					"elsewhere: `conservative`, `balanced` or `aggressive` (by the PCD UI's description, about 10, 5 and 2 " +
					"minutes to the first restart). A shorter wait recovers sooner but is likelier to react to a brief " +
					"network outage or a routine reboot. A write that carries it, on create and on an update that changes " +
					"it, also makes hamgr set the cluster's `cooldown` and `host_config.post_delay` to that speed's fixed " +
					"values (`aggressive` 20 s and 15 s, `balanced` 45 s and 30 s, `conservative` 150 s and 59 s), " +
					"replacing values set outside this resource, and push `post_delay` to the cluster's hosts that have " +
					"the VM high-availability role. Omitted, the cluster's value is left as it is and read back; it reads " +
					"null when the cluster has none, and hamgr then uses its default.",
				Validators: []validator.String{stringvalidator.OneOf("conservative", "balanced", "aggressive")}},
			"nfs_heartbeat_path": schema.StringAttribute{Optional: true, Computed: true,
				MarkdownDescription: "A path on shared NFS storage, mounted on every hypervisor in the cluster, where " +
					"each host writes a liveness heartbeat, so that PCD can tell a host cut off from the network from one " +
					"that failed: the Storage Heartbeat of the PCD UI's cluster dialogs, in PCD releases whose UI has " +
					"them. hamgr pushes it to the cluster's hosts that have the VM high-availability role. `\"\"` turns " +
					"the storage heartbeat off. Omitted, the cluster's value is left as it is and read back (`\"\"` when " +
					"there is none). Setting it sends hamgr's `host_config` with only this key; this resource does not " +
					"manage the other host settings hamgr keeps there."},
		},
	}
}

// ConfigValidators requires one of the two settings: with neither, there is
// nothing for the resource to manage.
func (r *clusterVMHAConfigResource) ConfigValidators(_ context.Context) []resource.ConfigValidator {
	return []resource.ConfigValidator{
		resourcevalidator.AtLeastOneOf(path.MatchRoot("evacuation_speed"), path.MatchRoot("nfs_heartbeat_path")),
	}
}

func (r *clusterVMHAConfigResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.config = configureClient(req.ProviderData, &resp.Diagnostics)
}

func (r *clusterVMHAConfigResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan clusterVMHAConfigModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	client, err := r.config.HamgrV1Client()
	if err != nil {
		resp.Diagnostics.AddError("hamgr: building client", err.Error())
		return
	}
	name := plan.ClusterName.ValueString()
	if err := r.writeVMHAConfig(ctx, client, name, vmhaConfigBody(&plan), true); err != nil {
		resp.Diagnostics.AddError("hamgr: writing VM high-availability configuration", vmhaErrorDetail(err))
		return
	}
	plan.ID = types.StringValue(name)
	if !tfstate.RecordCreated(ctx, resp, &plan) {
		return
	}
	if err := readBackVMHAConfig(ctx, client, &plan); err != nil {
		resp.Diagnostics.AddError("hamgr: reading VM high-availability configuration", vmhaErrorDetail(err)+
			"\n\nhamgr accepted the settings, so Terraform records this resource as tainted: the next apply writes "+
			"the settings again, or `terraform untaint` keeps the resource as written.")
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *clusterVMHAConfigResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state clusterVMHAConfigModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	client, err := r.config.HamgrV1Client()
	if err != nil {
		resp.Diagnostics.AddError("hamgr: building client", err.Error())
		return
	}
	found, err := readVMHAConfig(ctx, client, &state)
	if err != nil {
		resp.Diagnostics.AddError("hamgr: reading VM high-availability configuration", vmhaErrorDetail(err))
		return
	}
	if !found {
		resp.State.RemoveResource(ctx)
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *clusterVMHAConfigResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state clusterVMHAConfigModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	client, err := r.config.HamgrV1Client()
	if err != nil {
		resp.Diagnostics.AddError("hamgr: building client", err.Error())
		return
	}
	if body := vmhaUpdateBody(&plan, &state); body.Mode != nil || body.HostConfig != nil {
		if err := r.writeVMHAConfig(ctx, client, plan.ClusterName.ValueString(), body, false); err != nil {
			resp.Diagnostics.AddError("hamgr: writing VM high-availability configuration", vmhaErrorDetail(err))
			return
		}
	}
	if err := readBackVMHAConfig(ctx, client, &plan); err != nil {
		resp.Diagnostics.AddError("hamgr: reading VM high-availability configuration", vmhaErrorDetail(err)+
			"\n\nhamgr accepted the settings; running the apply again reads them back.")
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

// Delete calls nothing: hamgr has no DELETE for a configuration. It warns, so
// nobody takes the destroy for a reset.
func (r *clusterVMHAConfigResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state clusterVMHAConfigModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.AddWarning("VM high-availability settings left in place",
		"PCD has no way to delete a cluster's VM high-availability configuration, so hamgr keeps the evacuation "+
			"speed and NFS heartbeat path of cluster "+state.ClusterName.ValueString()+", also after the cluster is "+
			"deleted, and a later cluster of the same name gets them; Terraform only stops managing them. To leave the cluster "+
			"with the conservative speed and no storage heartbeat, apply evacuation_speed = \"conservative\" and "+
			"nfs_heartbeat_path = \"\" before destroying.")
}

// ImportState takes the cluster name as the import ID. A name that would not
// address the cluster's own configuration route is refused before any
// request, and the state stays null.
func (r *clusterVMHAConfigResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	if !vmhaClusterNamePattern.MatchString(req.ID) {
		resp.Diagnostics.AddAttributeError(path.Root("cluster_name"), "Import needs a cluster name", fmt.Sprintf(
			"Pass the name of the cluster whose VM high-availability configuration to import, %s; got %q.",
			vmhaClusterNameRule, req.ID))
		return
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), req.ID)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("cluster_name"), req.ID)...)
}

// vmhaConfigBody is Create's POST body for m: only the attributes the
// configuration sets. hamgr keeps every key the body leaves out, apart from
// the cooldown and post_delay that a mode resets.
func vmhaConfigBody(m *clusterVMHAConfigModel) vmhaConfigAPI {
	var body vmhaConfigAPI
	if v := m.EvacuationSpeed; !v.IsNull() && !v.IsUnknown() {
		mode := v.ValueString()
		body.Mode = &mode
	}
	if v := m.NFSHeartbeatPath; !v.IsNull() && !v.IsUnknown() {
		p := v.ValueString()
		body.HostConfig = &vmhaHostConfigAPI{NFSHeartbeatPath: &p}
	}
	return body
}

// vmhaUpdateBody is Update's POST body: only the settings whose planned value
// is known and differs from the state's. A body that carries mode makes hamgr
// reset the cluster's cooldown and post_delay, so an unchanged speed is not
// sent again.
func vmhaUpdateBody(plan, state *clusterVMHAConfigModel) vmhaConfigAPI {
	var body vmhaConfigAPI
	if v := plan.EvacuationSpeed; !v.IsNull() && !v.IsUnknown() && !v.Equal(state.EvacuationSpeed) {
		mode := v.ValueString()
		body.Mode = &mode
	}
	if v := plan.NFSHeartbeatPath; !v.IsNull() && !v.IsUnknown() && !v.Equal(state.NFSHeartbeatPath) {
		p := v.ValueString()
		body.HostConfig = &vmhaHostConfigAPI{NFSHeartbeatPath: &p}
	}
	return body
}

// checkVMHAClusterName refuses a cluster name that would address another
// hamgr route once it is put into the URL path.
func checkVMHAClusterName(cluster string) error {
	if !vmhaClusterNamePattern.MatchString(cluster) {
		return fmt.Errorf("cluster name %q cannot address a VM high-availability configuration: it must be %s",
			cluster, vmhaClusterNameRule)
	}
	return nil
}

// postVMHAConfig writes body into the cluster's configuration. hamgr answers
// 200 with {"status":"ok"}; any 2xx counts.
func postVMHAConfig(ctx context.Context, client *gophercloud.ServiceClient, cluster string, body vmhaConfigAPI) error {
	if err := checkVMHAClusterName(cluster); err != nil {
		return err
	}
	_, err := client.Post(ctx, client.ServiceURL("vmha", "config", cluster), body, nil,
		&gophercloud.RequestOpts{OkCodes: []int{200, 201, 202, 204}})
	return err
}

// writeVMHAConfig posts body once. When hamgr refuses it because the cluster
// is missing from its copy of resmgr's cluster list, it asks resmgr: a cluster
// resmgr does not have, an answer for a cluster whose name differs, or an
// answer that cannot tell, fails at once; a cluster resmgr has under exactly
// this name means hamgr has not reread the list yet, and the write is retried
// until hamgr accepts it. resmgr looks a name up regardless of case, while
// hamgr compares names exactly, so the answer's name is compared exactly.
// creating is true for Create, whose failure leaves nothing in state.
func (r *clusterVMHAConfigResource) writeVMHAConfig(ctx context.Context, client *gophercloud.ServiceClient, cluster string, body vmhaConfigAPI, creating bool) error {
	err := postVMHAConfig(ctx, client, cluster, body)
	if err == nil || !vmhaUnknownCluster(err) {
		return err
	}
	resmgr, rmErr := r.config.ResmgrV2Client()
	var known struct {
		Name string `json:"name"`
	}
	if rmErr == nil {
		rmErr = getJSON(ctx, resmgr, resmgr.ServiceURL("clusters", cluster), &known)
	}
	if rmErr == nil && known.Name == "" {
		rmErr = errors.New("its answer names no cluster")
	}
	switch {
	case isNotFound(rmErr):
		return fmt.Errorf("PCD's resource manager has no cluster named %q, so hamgr cannot hold its VM "+
			"high-availability configuration: %w", cluster, err)
	case rmErr != nil:
		return fmt.Errorf("%w\n\nAsking PCD's resource manager for cluster %q failed as well: %v", err, cluster, rmErr)
	case known.Name != cluster:
		return fmt.Errorf("PCD's resource manager has a cluster named %q, not %q, and hamgr matches cluster names "+
			"exactly: give the name exactly as the resource manager has it: %w", known.Name, cluster, err)
	}
	return waitForVMHAClusterKnown(ctx, client, cluster, body, err, creating)
}

// waitForVMHAClusterKnown retries the write every vmhaClusterKnownInterval
// while hamgr answers refusal's kind, for up to vmhaClusterKnownTimeout. Any
// other answer ends the wait with that answer. creating says what the next
// apply does after a write of unknown outcome: a failed Create leaves nothing
// in state, so it sends the settings again; a failed Update keeps the prior
// state, whose refresh reads hamgr, so it sends only what hamgr does not hold.
func waitForVMHAClusterKnown(ctx context.Context, client *gophercloud.ServiceClient, cluster string, body vmhaConfigAPI, refusal error, creating bool) error {
	// The timeout is on the context, so it also ends a write hamgr never
	// answers: the HTTP client has no timeout of its own.
	waitCtx, cancel := context.WithTimeout(ctx, vmhaClusterKnownTimeout)
	defer cancel()
	last := refusal.Error()
	// ended is the error for a wait that ends before hamgr accepts the write.
	// Between attempts every write so far was refused, so nothing was written;
	// while a retried write is unanswered, its outcome is unknown.
	ended := func(inFlight bool) error {
		unknown := "the outcome of the last write is unknown, and the next apply sends the settings again"
		if !creating {
			unknown += " if hamgr does not hold them"
		}
		switch {
		case ctx.Err() != nil && inFlight:
			return fmt.Errorf("waiting for hamgr to learn of cluster %q was interrupted while a write was "+
				"unanswered: %s (last answer: %s): %w", cluster, unknown, last, ctx.Err())
		case ctx.Err() != nil:
			return fmt.Errorf("waiting for hamgr to learn of cluster %q was interrupted; nothing was written "+
				"(last answer: %s): %w", cluster, last, ctx.Err())
		case inFlight:
			return fmt.Errorf("hamgr had not accepted a write for cluster %q %s after the first attempt, although "+
				"the resource manager has it, and the last write was unanswered when the wait ended: %s. hamgr "+
				"rereads the cluster list every five minutes, or cannot reach the resource manager; if it keeps "+
				"failing, check hamgr (last answer: %s)", cluster, vmhaClusterKnownTimeout, unknown, last)
		}
		return fmt.Errorf("hamgr still answers that cluster %q is not found in resmgr %s after the first attempt, "+
			"although the resource manager has it: hamgr rereads the cluster list every five minutes, or cannot "+
			"reach the resource manager; run the apply again, and if it keeps failing check hamgr (last answer: %s)",
			cluster, vmhaClusterKnownTimeout, last)
	}
	for {
		timer := time.NewTimer(vmhaClusterKnownInterval)
		select {
		case <-waitCtx.Done():
			timer.Stop()
			return ended(false)
		case <-timer.C:
		}
		attemptCtx, cancelAttempt := context.WithTimeout(waitCtx, vmhaClusterKnownCheckTimeout)
		err := postVMHAConfig(attemptCtx, client, cluster, body)
		attemptErr := attemptCtx.Err()
		cancelAttempt()
		switch {
		case err == nil:
			return nil
		case vmhaUnknownCluster(err):
			last = err.Error()
		case waitCtx.Err() != nil:
			return ended(true)
		case attemptErr != nil:
			return fmt.Errorf("hamgr did not answer a write for cluster %q within %s: %w",
				cluster, vmhaClusterKnownCheckTimeout, err)
		default:
			return err
		}
	}
}

// vmhaUnknownCluster reports whether err is hamgr refusing a write because the
// cluster is missing from its copy of resmgr's cluster list: a 404 from its
// handler, whose plain-text body says so.
func vmhaUnknownCluster(err error) bool {
	var code gophercloud.ErrUnexpectedResponseCode
	return errors.As(err, &code) && code.Actual == http.StatusNotFound && bytes.Contains(code.Body, vmhaUnknownClusterBody)
}

// readVMHAConfig reads the cluster's configuration into m and reports whether
// hamgr holds one: only hamgr's own "no config found" 404 means it does not.
// A 200 without a configuration is an error. A key hamgr does not return reads
// as null for evacuation_speed and "" for nfs_heartbeat_path, the value that
// turns the storage heartbeat off, so a stored `{}` reads as both.
func readVMHAConfig(ctx context.Context, client *gophercloud.ServiceClient, m *clusterVMHAConfigModel) (bool, error) {
	cluster := m.ClusterName.ValueString()
	if err := checkVMHAClusterName(cluster); err != nil {
		return false, err
	}
	var got vmhaConfigAPI
	err := getJSON(ctx, client, client.ServiceURL("vmha", "config", cluster), &got)
	if vmhaNoConfig(err) {
		return false, nil
	}
	if errors.Is(err, errAbsent) {
		return false, fmt.Errorf("hamgr answered without a configuration for cluster %q", cluster)
	}
	if err != nil {
		return false, err
	}
	m.ID = m.ClusterName
	m.EvacuationSpeed = types.StringPointerValue(got.Mode)
	m.NFSHeartbeatPath = types.StringValue("")
	if got.HostConfig != nil && got.HostConfig.NFSHeartbeatPath != nil {
		m.NFSHeartbeatPath = types.StringValue(*got.HostConfig.NFSHeartbeatPath)
	}
	return true, nil
}

// readBackVMHAConfig reads the configuration Create or Update has just
// written into m. A configuration hamgr says it does not hold is an error
// here: the write succeeded.
func readBackVMHAConfig(ctx context.Context, client *gophercloud.ServiceClient, m *clusterVMHAConfigModel) error {
	found, err := readVMHAConfig(ctx, client, m)
	if err == nil && !found {
		err = errors.New("hamgr reports no configuration for the cluster right after writing it")
	}
	return err
}

// vmhaNoConfig reports whether err is hamgr saying it holds no configuration
// for the cluster: a 404 from its handler, whose plain-text body says so.
func vmhaNoConfig(err error) bool {
	var code gophercloud.ErrUnexpectedResponseCode
	return errors.As(err, &code) && code.Actual == http.StatusNotFound && bytes.Contains(code.Body, vmhaNoConfigBody)
}

// vmhaErrorDetail is err's text, plus a hint when the 404 came from hamgr's
// router, not a handler: hamgr does not serve the URL, either because
// endpoint_overrides.hamgr points elsewhere or because this hamgr has no VM
// high-availability configuration API.
func vmhaErrorDetail(err error) string {
	var code gophercloud.ErrUnexpectedResponseCode
	if errors.As(err, &code) && code.Actual == http.StatusNotFound && bytes.Contains(code.Body, vmhaNoRouteBody) {
		return err.Error() + "\n\nhamgr does not serve this URL. If endpoint_overrides.hamgr is set, check it; " +
			"otherwise this PCD release may predate hamgr's VM high-availability configuration API."
	}
	return err.Error()
}
