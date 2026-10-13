// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0
//
// Ported from terraform-provider-openstack v3.4.0
// (openstack/resource_openstack_compute_aggregate_v2.go), adapted for the
// terraform-plugin-framework and PCD: an availability zone cannot be cleared,
// hosts are resmgr host UUIDs, adding a host waits out its registration with
// Nova, and the aggregates PCD manages for its clusters are refused.

package compute

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/aggregates"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/mapdefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/setdefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/platform9/terraform-provider-pcd/internal/clients"
	"github.com/platform9/terraform-provider-pcd/internal/tfstate"
)

var (
	_ resource.Resource                   = (*aggregateResource)(nil)
	_ resource.ResourceWithConfigure      = (*aggregateResource)(nil)
	_ resource.ResourceWithImportState    = (*aggregateResource)(nil)
	_ resource.ResourceWithValidateConfig = (*aggregateResource)(nil)
)

// reservedAggregateMetadataKeys are metadata keys the resource refuses:
// Nova stores the zone as availability_zone metadata (set it through zone),
// and pf9-managed marks the aggregates PCD's resource manager owns.
var reservedAggregateMetadataKeys = []string{"availability_zone", "pf9-managed"}

// regexpNoColon matches a zone name Nova accepts: a colon would read as the
// "<zone>:<host>" pin instances use.
var regexpNoColon = regexp.MustCompile(`^[^:]*$`)

// aggregateHostRetryInterval and aggregateHostRetryTimeout bound how long
// adding a host waits for Nova to know it. Nova answers 404 ComputeHostNotFound
// until the host's nova-compute service has registered, which happens only
// after the hypervisor role converges. They are vars so unit tests
// can shorten them.
var (
	aggregateHostRetryInterval = 15 * time.Second
	aggregateHostRetryTimeout  = 5 * time.Minute
)

// NewAggregateResource is the factory registered with the provider.
func NewAggregateResource() resource.Resource {
	return &aggregateResource{}
}

type aggregateResource struct {
	config *clients.Config
}

type aggregateModel struct {
	ID       types.String `tfsdk:"id"`
	Name     types.String `tfsdk:"name"`
	Zone     types.String `tfsdk:"zone"`
	Metadata types.Map    `tfsdk:"metadata"`
	Hosts    types.Set    `tfsdk:"hosts"`
	Region   types.String `tfsdk:"region"`
}

func (r *aggregateResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_compute_aggregate"
}

func (r *aggregateResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	stable := []planmodifier.String{stringplanmodifier.UseStateForUnknown()}
	resp.Schema = schema.Schema{
		MarkdownDescription: "Manages a Nova host aggregate: a named group of hypervisors with metadata the scheduler " +
			"can match, and optionally an availability zone. The PCD UI calls these host aggregates; the stretched " +
			"cluster (failure domain) feature is built from them, as the Failure domains guide shows. Aggregate names " +
			"are unique across the region and share their namespace with clusters, because PCD's resource manager " +
			"creates one aggregate per cluster under the cluster's name: an aggregate cannot take a cluster's name, " +
			"and a `pcd_cluster` created later with an aggregate's name fails. Those per-cluster aggregates, and the " +
			"aggregate named `compute`, belong to PCD and cannot be managed or imported here. Requires an admin token.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{Computed: true, MarkdownDescription: "The Nova aggregate ID, a decimal integer.", PlanModifiers: stable},
			"name": schema.StringAttribute{Required: true,
				MarkdownDescription: "The aggregate name, unique in the region. Updatable in place.",
				Validators:          []validator.String{stringvalidator.LengthBetween(1, 255)}},
			"zone": schema.StringAttribute{Optional: true,
				MarkdownDescription: "The availability zone the aggregate's hosts form. In PCD a cluster is an availability " +
					"zone named after the cluster, so this should be the hosts' cluster name (`pcd_cluster.<name>.name`) or " +
					"unset: a host can belong to aggregates of only one zone, so adding a host whose cluster differs fails. " +
					"Changing the zone from one value to another updates in place, which Nova refuses when the aggregate's " +
					"hosts already run instances. Nova cannot remove a zone once set, so removing this attribute replaces the aggregate. " +
					"When importing an aggregate that has a zone, set `zone` to it in the configuration first, or the plan replaces the aggregate.",
				Validators: []validator.String{
					stringvalidator.LengthBetween(1, 255),
					stringvalidator.RegexMatches(regexpNoColon, "must not contain ':'"),
				},
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplaceIf(zoneRemovedRequiresReplace,
						"Nova cannot remove an aggregate's availability zone, so removing zone replaces the aggregate.",
						"Nova cannot remove an aggregate's availability zone, so removing `zone` replaces the aggregate."),
				}},
			"metadata": schema.MapAttribute{Optional: true, Computed: true, ElementType: types.StringType,
				Default: mapdefault.StaticValue(types.MapValueMust(types.StringType, map[string]attr.Value{})),
				MarkdownDescription: "Key-value metadata the scheduler's aggregate filters match against. Authoritative: keys " +
					"set outside Terraform are removed on the next apply, and omitting the attribute removes every key. " +
					"`availability_zone` (use `zone`) and `pf9-managed` are reserved. Nova's aggregate filters in PCD " +
					"compare metadata with flavor extra specs, image properties and project IDs, so choose keys that " +
					"do not collide with those unless you mean them to."},
			"hosts": schema.SetAttribute{Optional: true, Computed: true, ElementType: types.StringType,
				Default: setdefault.StaticValue(types.SetValueMust(types.StringType, []attr.Value{})),
				MarkdownDescription: "The hypervisors in the aggregate, by resmgr host UUID: `data.pcd_host.<name>.id` or " +
					"`pcd_host_cluster_role.<name>.host_id` (PCD names a host's compute service after that UUID). " +
					"Authoritative: hosts added outside Terraform are removed on the next apply. Nova knows a host only " +
					"once its hypervisor role has converged, so adding one waits up to five minutes for it; when the " +
					"role is assigned in the same configuration, set `wait_until_converged = true` on it."},
			"region": schema.StringAttribute{Optional: true, Computed: true,
				MarkdownDescription: "The region. Defaults to the provider's region. Changing this forces a new resource.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.RequiresReplace(), stringplanmodifier.UseStateForUnknown()}},
		},
	}
}

// zoneRemovedRequiresReplace replaces the aggregate when its zone is removed:
// Nova answers 400 ("does not support empty named availability zone") to both
// availability_zone: null on PUT and to the same key in set_metadata, so the
// only way to an aggregate without a zone is a new one. A change from one zone
// to another stays in place.
func zoneRemovedRequiresReplace(_ context.Context, req planmodifier.StringRequest, resp *stringplanmodifier.RequiresReplaceIfFuncResponse) {
	resp.RequiresReplace = !req.StateValue.IsNull() && req.PlanValue.IsNull()
}

func (r *aggregateResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.config = configureClient(req.ProviderData, &resp.Diagnostics)
}

// ValidateConfig refuses the reserved metadata keys at plan time.
func (r *aggregateResource) ValidateConfig(ctx context.Context, req resource.ValidateConfigRequest, resp *resource.ValidateConfigResponse) {
	var meta types.Map
	resp.Diagnostics.Append(req.Config.GetAttribute(ctx, path.Root("metadata"), &meta)...)
	if resp.Diagnostics.HasError() || meta.IsNull() || meta.IsUnknown() {
		return
	}
	for _, k := range reservedAggregateMetadataKeys {
		if _, ok := meta.Elements()[k]; ok {
			resp.Diagnostics.AddAttributeError(path.Root("metadata").AtMapKey(k), "Reserved aggregate metadata key",
				fmt.Sprintf("%q is reserved: set the availability zone through zone, and pf9-managed marks the aggregates PCD manages for its clusters.", k))
		}
	}
}

func (r *aggregateResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan aggregateModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	client, err := r.config.ForRegion(plan.Region.ValueString()).ComputeV2Client()
	if err != nil {
		resp.Diagnostics.AddError("compute: building v2 client", err.Error())
		return
	}

	agg, err := aggregates.Create(ctx, client, aggregates.CreateOpts{
		Name:             plan.Name.ValueString(),
		AvailabilityZone: plan.Zone.ValueString(),
	}).Extract()
	if err != nil {
		resp.Diagnostics.AddError("compute: creating aggregate", err.Error())
		return
	}
	if agg == nil {
		resp.Diagnostics.AddError("compute: creating aggregate", "The Compute API answered without an aggregate object.")
		return
	}

	// Record the aggregate as Nova created it (no hosts, no metadata yet) before
	// adding hosts, which can wait minutes: a failure then leaves it in state,
	// tainted, so the next apply deletes and recreates it instead of failing on
	// the name Nova still holds.
	created := plan
	resp.Diagnostics.Append(flattenAggregate(ctx, agg, &created, r.config.Region)...)
	if resp.Diagnostics.HasError() || !tfstate.RecordCreated(ctx, resp, &created) {
		return
	}

	var hosts []string
	resp.Diagnostics.Append(plan.Hosts.ElementsAs(ctx, &hosts, false)...)
	meta := extractStringMap(ctx, plan.Metadata, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}
	slices.Sort(hosts)
	for _, h := range hosts {
		if err := addAggregateHost(ctx, client, agg.ID, h); err != nil {
			resp.Diagnostics.AddError("compute: adding host "+h+" to aggregate", err.Error())
			return
		}
	}
	if len(meta) > 0 {
		if _, err := aggregates.SetMetadata(ctx, client, agg.ID, aggregates.SetMetadataOpts{
			Metadata: aggregateMetadataUpdate(nil, meta),
		}).Extract(); err != nil {
			resp.Diagnostics.AddError("compute: setting aggregate metadata", err.Error())
			return
		}
	}

	r.readInto(ctx, client, agg.ID, &plan, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *aggregateResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state aggregateModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if tfstate.DropRowWithoutID(ctx, resp, state.ID, "host aggregate", state.Name.ValueString()) {
		return
	}
	id, err := parseAggregateID(state.ID.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("Invalid aggregate ID", err.Error())
		return
	}
	client, err := r.config.ForRegion(state.Region.ValueString()).ComputeV2Client()
	if err != nil {
		resp.Diagnostics.AddError("compute: building v2 client", err.Error())
		return
	}
	agg, err := getAggregate(ctx, client, id)
	if err != nil {
		if gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
			resp.Diagnostics.AddWarning("Aggregate not found",
				fmt.Sprintf("Aggregate %d no longer exists and was removed from state.", id))
			resp.State.RemoveResource(ctx)
			return
		}
		resp.Diagnostics.AddError("compute: reading aggregate", err.Error())
		return
	}
	if msg := pcdManagedAggregate(agg); msg != "" {
		resp.Diagnostics.AddError("Aggregate managed by PCD", msg)
		return
	}
	resp.Diagnostics.Append(flattenAggregate(ctx, agg, &state, r.config.Region)...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

// Update applies changes in the order that keeps Nova's zone checks happy:
// remove hosts, rename or re-zone, add hosts, then metadata. Upstream renames
// first, which fails when a re-zone conflicts with hosts that are leaving.
func (r *aggregateResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state aggregateModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	id, err := parseAggregateID(state.ID.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("Invalid aggregate ID", err.Error())
		return
	}
	client, err := r.config.ForRegion(plan.Region.ValueString()).ComputeV2Client()
	if err != nil {
		resp.Diagnostics.AddError("compute: building v2 client", err.Error())
		return
	}

	var planHosts, stateHosts []string
	resp.Diagnostics.Append(plan.Hosts.ElementsAs(ctx, &planHosts, false)...)
	resp.Diagnostics.Append(state.Hosts.ElementsAs(ctx, &stateHosts, false)...)
	if resp.Diagnostics.HasError() {
		return
	}
	slices.Sort(planHosts)
	slices.Sort(stateHosts)

	for _, h := range stateHosts {
		if !slices.Contains(planHosts, h) {
			if err := removeAggregateHost(ctx, client, id, h); err != nil {
				resp.Diagnostics.AddError("compute: removing host "+h+" from aggregate", err.Error())
				return
			}
		}
	}

	var opts aggregates.UpdateOpts
	if !plan.Name.Equal(state.Name) {
		opts.Name = plan.Name.ValueString()
	}
	// The removal of a zone is a replacement (zoneRemovedRequiresReplace), so a
	// changed zone here is always a value Nova can set.
	if !plan.Zone.Equal(state.Zone) && !plan.Zone.IsNull() {
		opts.AvailabilityZone = plan.Zone.ValueString()
	}
	if opts != (aggregates.UpdateOpts{}) {
		if _, err := aggregates.Update(ctx, client, id, opts).Extract(); err != nil {
			resp.Diagnostics.AddError("compute: updating aggregate", err.Error())
			return
		}
	}

	for _, h := range planHosts {
		if !slices.Contains(stateHosts, h) {
			if err := addAggregateHost(ctx, client, id, h); err != nil {
				resp.Diagnostics.AddError("compute: adding host "+h+" to aggregate", err.Error())
				return
			}
		}
	}

	if !plan.Metadata.Equal(state.Metadata) {
		oldMeta := extractStringMap(ctx, state.Metadata, &resp.Diagnostics)
		newMeta := extractStringMap(ctx, plan.Metadata, &resp.Diagnostics)
		if resp.Diagnostics.HasError() {
			return
		}
		if body := aggregateMetadataUpdate(oldMeta, newMeta); len(body) > 0 {
			if _, err := aggregates.SetMetadata(ctx, client, id, aggregates.SetMetadataOpts{Metadata: body}).Extract(); err != nil {
				resp.Diagnostics.AddError("compute: setting aggregate metadata", err.Error())
				return
			}
		}
	}

	r.readInto(ctx, client, id, &plan, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

// Delete removes every host the aggregate holds now, not only those in state
// (Nova refuses to delete an aggregate that has hosts), then deletes it.
func (r *aggregateResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state aggregateModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if tfstate.SkipDeleteWithoutID(resp, state.ID, "host aggregate", state.Name.ValueString()) {
		return
	}
	id, err := parseAggregateID(state.ID.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("Invalid aggregate ID", err.Error())
		return
	}
	client, err := r.config.ForRegion(state.Region.ValueString()).ComputeV2Client()
	if err != nil {
		resp.Diagnostics.AddError("compute: building v2 client", err.Error())
		return
	}
	agg, err := getAggregate(ctx, client, id)
	if err != nil {
		if gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
			return
		}
		resp.Diagnostics.AddError("compute: reading aggregate before delete", err.Error())
		return
	}
	hosts := slices.Clone(agg.Hosts)
	slices.Sort(hosts)
	for _, h := range hosts {
		if err := removeAggregateHost(ctx, client, id, h); err != nil {
			resp.Diagnostics.AddError("compute: removing host "+h+" from aggregate", err.Error())
			return
		}
	}
	if err := aggregates.Delete(ctx, client, id).ExtractErr(); err != nil && !gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
		resp.Diagnostics.AddError("compute: deleting aggregate", err.Error())
	}
}

func (r *aggregateResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	if _, err := parseAggregateID(req.ID); err != nil {
		resp.Diagnostics.AddError("Invalid import ID", err.Error()+" Find the ID with `pcdctl aggregate list`.")
		return
	}
	resource.ImportStatePassthroughID(ctx, path.Root("id"), req, resp)
}

// readInto re-reads the aggregate and flattens it into m.
func (r *aggregateResource) readInto(ctx context.Context, client *gophercloud.ServiceClient, id int, m *aggregateModel, diags *diag.Diagnostics) {
	agg, err := getAggregate(ctx, client, id)
	if err != nil {
		diags.AddError("compute: reading aggregate", err.Error())
		return
	}
	diags.Append(flattenAggregate(ctx, agg, m, r.config.Region)...)
}

// getAggregate reads aggregate id. A 200 that holds no aggregate object
// decodes to nil; that is an error, not a missing aggregate, which would drop
// one that exists from state.
func getAggregate(ctx context.Context, client *gophercloud.ServiceClient, id int) (*aggregates.Aggregate, error) {
	agg, err := aggregates.Get(ctx, client, id).Extract()
	if err != nil {
		return nil, err
	}
	if agg == nil {
		return nil, fmt.Errorf("the Compute API answered without an aggregate object for ID %d", id)
	}
	return agg, nil
}

// parseAggregateID converts a state or import ID to Nova's integer ID.
func parseAggregateID(s string) (int, error) {
	id, err := strconv.Atoi(s)
	if err != nil || id < 0 {
		return 0, fmt.Errorf("aggregate ID %q is not a non-negative integer", s)
	}
	return id, nil
}

// pcdManagedAggregate explains why an aggregate belongs to PCD, or returns "".
// The resource manager creates one aggregate per cluster, marked pf9-managed,
// and adds the cluster's hosts to it; the PCD UI never lets anyone edit or
// delete the aggregate named compute.
func pcdManagedAggregate(a *aggregates.Aggregate) string {
	if a.Metadata["pf9-managed"] == "true" {
		return fmt.Sprintf("Aggregate %d (%q) is the aggregate PCD's resource manager keeps for cluster %q; manage the cluster "+
			"with pcd_cluster and its hosts with pcd_host_cluster_role. To give the cluster's hosts scheduler metadata, "+
			"create a separate pcd_compute_aggregate with zone = %q and the same hosts.", a.ID, a.Name, a.AvailabilityZone, a.AvailabilityZone)
	}
	if a.Name == "compute" {
		return fmt.Sprintf("Aggregate %d (%q) belongs to PCD and cannot be managed by Terraform.", a.ID, a.Name)
	}
	return ""
}

// flattenAggregate writes the server's view of the aggregate into m. Nova
// mirrors the zone into the availability_zone metadata key; it is dropped
// here so it never shows as metadata drift.
func flattenAggregate(ctx context.Context, a *aggregates.Aggregate, m *aggregateModel, region string) diag.Diagnostics {
	var diags diag.Diagnostics
	m.ID = types.StringValue(strconv.Itoa(a.ID))
	m.Name = types.StringValue(a.Name)
	if a.AvailabilityZone == "" {
		m.Zone = types.StringNull()
	} else {
		m.Zone = types.StringValue(a.AvailabilityZone)
	}
	meta := make(map[string]string, len(a.Metadata))
	for k, v := range a.Metadata {
		if k != "availability_zone" {
			meta[k] = v
		}
	}
	metaVal, d := types.MapValueFrom(ctx, types.StringType, meta)
	diags.Append(d...)
	m.Metadata = metaVal
	hosts := a.Hosts
	if hosts == nil {
		hosts = []string{}
	}
	hostsVal, d := types.SetValueFrom(ctx, types.StringType, hosts)
	diags.Append(d...)
	m.Hosts = hostsVal
	if m.Region.IsNull() || m.Region.IsUnknown() {
		m.Region = types.StringValue(region)
	}
	return diags
}

// aggregateMetadataUpdate builds a set_metadata body: every new key with its
// value, and every key that disappeared as nil, which Nova deletes.
func aggregateMetadataUpdate(oldMeta, newMeta map[string]string) map[string]any {
	body := make(map[string]any, len(oldMeta)+len(newMeta))
	for k, v := range newMeta {
		body[k] = v
	}
	for k := range oldMeta {
		if _, kept := newMeta[k]; !kept {
			body[k] = nil
		}
	}
	return body
}

// addAggregateHost adds host to the aggregate, retrying while Nova does not
// know the host yet. Only "Compute host ... could not be found" and "Host ...
// is not mapped to any cell" are retried: an AggregateNotFound is also a 404
// and must fail at once. PCD's Nova commits the membership before it updates
// Placement, so a 500 (or a gateway's 504) after that step is a success when
// the aggregate lists the host; re-adding a member is a no-op there.
func addAggregateHost(ctx context.Context, client *gophercloud.ServiceClient, id int, host string) error {
	// The timeout is on the context, so it also ends a request that Nova never
	// answers: the HTTP client has no timeout of its own.
	ctx, cancel := context.WithTimeout(ctx, aggregateHostRetryTimeout)
	defer cancel()
	last := "none"
	timedOut := func() error {
		return fmt.Errorf("host %s is still unknown to Nova after %s; its hypervisor role has probably not "+
			"converged. Set wait_until_converged = true on the host's pcd_host_cluster_role so the role "+
			"converges before the aggregate adds the host (last answer: %s): %w",
			host, aggregateHostRetryTimeout, last, ctx.Err())
	}
	for {
		_, err := aggregates.AddHost(ctx, client, id, aggregates.AddHostOpts{Host: host}).Extract()
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return timedOut()
		}
		var code gophercloud.ErrUnexpectedResponseCode
		if !errors.As(err, &code) {
			return err
		}
		switch {
		case code.Actual == http.StatusInternalServerError || code.Actual == http.StatusGatewayTimeout:
			if agg, getErr := getAggregate(ctx, client, id); getErr == nil && slices.Contains(agg.Hosts, host) {
				return nil
			}
			return err
		case code.Actual == http.StatusNotFound && hostNotRegistered(string(code.Body)):
			last = err.Error()
		default:
			return err
		}
		select {
		case <-ctx.Done():
			return timedOut()
		case <-time.After(aggregateHostRetryInterval):
		}
	}
}

// hostNotRegistered reports whether a 404 body is Nova's "this host has no
// compute service yet" answer rather than a missing aggregate.
func hostNotRegistered(body string) bool {
	return strings.Contains(body, "Compute host") || strings.Contains(body, "is not mapped to any cell")
}

// removeAggregateHost removes host, treating "not a member" (404) as done.
func removeAggregateHost(ctx context.Context, client *gophercloud.ServiceClient, id int, host string) error {
	_, err := aggregates.RemoveHost(ctx, client, id, aggregates.RemoveHostOpts{Host: host}).Extract()
	if err != nil && !gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
		return err
	}
	return nil
}
