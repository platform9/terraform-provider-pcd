// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package compute

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/google/uuid"
	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/services"
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
	_ resource.Resource                = (*computeServiceResource)(nil)
	_ resource.ResourceWithConfigure   = (*computeServiceResource)(nil)
	_ resource.ResourceWithImportState = (*computeServiceResource)(nil)
)

// computeMicroversionServicesUUID is the first compute microversion whose
// os-services API names a service by UUID (PUT /os-services/{id}). The PCD UI
// uses the older host-and-binary PUT /os-services/enable|disable, which writes
// the same two fields.
const computeMicroversionServicesUUID = "2.53"

// Service status values. "maintenance" is how the PCD UI shows a host whose
// compute service is disabled with a reason; "disabled" has no reason.
const (
	serviceEnabled     = "enabled"
	serviceDisabled    = "disabled"
	serviceMaintenance = "maintenance"
)

// computeServicePollInterval and computeServiceRegisterTimeout bound Create's
// wait for a host's nova-compute service to appear, which happens once its
// hypervisor role converges. Vars so unit tests can shorten them.
var (
	computeServicePollInterval    = 15 * time.Second
	computeServiceRegisterTimeout = 10 * time.Minute
)

var errComputeServiceNotFound = errors.New("no nova-compute service for this host")

// maintenanceReason matches the reason the PCD UI writes when it puts a host in
// maintenance: "<migration uuid>:<epoch milliseconds>". The UI's maintenance
// status bar splits it at the colon and tracks the VMs tagged with the uuid.
var maintenanceReason = regexp.MustCompile(`^([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}):[0-9]+$`)

// NewComputeServiceResource is the factory registered with the provider.
func NewComputeServiceResource() resource.Resource {
	return &computeServiceResource{}
}

type computeServiceResource struct {
	config *clients.Config
}

type computeServiceModel struct {
	ID             types.String `tfsdk:"id"`
	HostID         types.String `tfsdk:"host_id"`
	Status         types.String `tfsdk:"status"`
	ServiceID      types.String `tfsdk:"service_id"`
	DisabledReason types.String `tfsdk:"disabled_reason"`
	ComputeState   types.String `tfsdk:"compute_state"`
}

func (r *computeServiceResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_compute_service"
}

func (r *computeServiceResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	stable := []planmodifier.String{stringplanmodifier.UseStateForUnknown()}
	resp.Schema = schema.Schema{
		MarkdownDescription: "Controls whether Nova schedules new instances onto a hypervisor: the PCD UI's Enable/Disable " +
			"Scheduling and Enable/Disable Maintenance Mode on a host. It manages the host's existing `nova-compute` " +
			"service record, so creating it changes the service's status rather than creating anything, and destroying " +
			"it re-enables scheduling. Creating it waits up to ten minutes for the host's `nova-compute` service to " +
			"register, which happens once its hypervisor role has converged. " +
			"`status = \"maintenance\"` only disables scheduling, with the reason the PCD UI " +
			"writes, so the UI shows the host in maintenance; it moves no instances. To move them off, run the " +
			"`pcd_compute_host_drain` action afterward (Terraform 1.14 or later). On a cluster with a single hypervisor, " +
			"disabling it stops new instances and, per the PCD UI, most other VM operations in that cluster until it is " +
			"enabled again. Requires an admin token.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{Computed: true, MarkdownDescription: "The host ID, the same as `host_id`.", PlanModifiers: stable},
			"host_id": schema.StringAttribute{Required: true,
				MarkdownDescription: "The hypervisor's resmgr host UUID: `data.pcd_host.<name>.id` or " +
					"`pcd_host_cluster_role.<name>.host_id`. PCD names the host's compute service after it. Reference the " +
					"hypervisor's `pcd_host_cluster_role` so this resource is created after the role and destroyed before it. " +
					"Changing this forces a new resource.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
				Validators:    []validator.String{stringvalidator.LengthAtLeast(1)}},
			"status": schema.StringAttribute{Required: true,
				MarkdownDescription: "`enabled` schedules new instances onto the host. `disabled` stops that and leaves " +
					"running instances where they are. `maintenance` is `disabled` with the reason the PCD UI writes for " +
					"maintenance mode (`<uuid>:<epoch milliseconds>`); a host already in maintenance keeps its reason, " +
					"including one the UI wrote. A host disabled with any other reason (from the CLI, for example) reads as " +
					"`maintenance` too. The UI offers maintenance only from `enabled`; Nova and this resource also allow it from `disabled`.",
				Validators: []validator.String{stringvalidator.OneOf(serviceEnabled, serviceDisabled, serviceMaintenance)}},
			"service_id": schema.StringAttribute{Computed: true, MarkdownDescription: "The Nova service UUID.", PlanModifiers: stable},
			"disabled_reason": schema.StringAttribute{Computed: true,
				MarkdownDescription: "The reason Nova holds for the disabled service; null when it has none."},
			"compute_state": schema.StringAttribute{Computed: true,
				MarkdownDescription: "Whether the host's `nova-compute` is reporting in: `up` or `down`. Informational."},
		},
	}
}

func (r *computeServiceResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.config = configureClient(req.ProviderData, &resp.Diagnostics)
}

func (r *computeServiceResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan computeServiceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	client, err := r.config.ComputeV2Client()
	if err != nil {
		resp.Diagnostics.AddError("compute: building v2 client", err.Error())
		return
	}
	svc, err := waitForComputeService(ctx, client, plan.HostID.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("compute: finding the host's compute service", err.Error())
		return
	}
	if svc, err = applyServiceStatus(ctx, client, svc, plan.Status.ValueString(), time.Now()); err != nil {
		resp.Diagnostics.AddError("compute: updating the host's compute service", err.Error())
		return
	}
	flattenComputeService(svc, plan.HostID.ValueString(), &plan)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *computeServiceResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state computeServiceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	// A row with no host ID names no service: a list with an empty host
	// filter returns every nova-compute service.
	if tfstate.DropRowWithoutID(ctx, resp, state.ID, "compute service", "") {
		return
	}
	client, err := r.config.ComputeV2Client()
	if err != nil {
		resp.Diagnostics.AddError("compute: building v2 client", err.Error())
		return
	}
	svc, err := lookupComputeService(ctx, client, state.HostID.ValueString())
	if errors.Is(err, errComputeServiceNotFound) {
		resp.Diagnostics.AddWarning("Compute service not found",
			fmt.Sprintf("Host %s has no nova-compute service any more (its hypervisor role was removed or the host "+
				"was deauthorized), so it was removed from state.", state.HostID.ValueString()))
		resp.State.RemoveResource(ctx)
		return
	}
	if err != nil {
		resp.Diagnostics.AddError("compute: reading the host's compute service", err.Error())
		return
	}
	flattenComputeService(svc, state.HostID.ValueString(), &state)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *computeServiceResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan computeServiceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	client, err := r.config.ComputeV2Client()
	if err != nil {
		resp.Diagnostics.AddError("compute: building v2 client", err.Error())
		return
	}
	svc, err := lookupComputeService(ctx, client, plan.HostID.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("compute: finding the host's compute service", err.Error())
		return
	}
	if svc, err = applyServiceStatus(ctx, client, svc, plan.Status.ValueString(), time.Now()); err != nil {
		resp.Diagnostics.AddError("compute: updating the host's compute service", err.Error())
		return
	}
	flattenComputeService(svc, plan.HostID.ValueString(), &plan)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

// Delete re-enables scheduling, so destroying the resource never strands a
// host that no instance can land on. A host whose service is gone needs nothing.
func (r *computeServiceResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state computeServiceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if tfstate.SkipDeleteWithoutID(resp, state.ID, "compute service", "") {
		return
	}
	client, err := r.config.ComputeV2Client()
	if err != nil {
		resp.Diagnostics.AddError("compute: building v2 client", err.Error())
		return
	}
	svc, err := lookupComputeService(ctx, client, state.HostID.ValueString())
	if errors.Is(err, errComputeServiceNotFound) {
		return
	}
	if err != nil {
		resp.Diagnostics.AddError("compute: finding the host's compute service", err.Error())
		return
	}
	if _, err := applyServiceStatus(ctx, client, svc, serviceEnabled, time.Now()); err != nil {
		resp.Diagnostics.AddError("compute: re-enabling the host's compute service", err.Error())
	}
}

func (r *computeServiceResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), req.ID)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("host_id"), req.ID)...)
}

// lookupComputeService finds the host's nova-compute service. It lists at the
// UUID microversion so the returned ID can address PUT /os-services/{id}.
func lookupComputeService(ctx context.Context, client *gophercloud.ServiceClient, hostID string) (*services.Service, error) {
	// A list without a host filter would return every hypervisor's service.
	if hostID == "" {
		return nil, errors.New("compute service lookup needs a host ID")
	}
	pages, err := services.List(withMicroversion(client, computeMicroversionServicesUUID),
		services.ListOpts{Binary: "nova-compute", Host: hostID}).AllPages(ctx)
	if err != nil {
		return nil, err
	}
	all, err := services.ExtractServices(pages)
	if err != nil {
		return nil, err
	}
	switch len(all) {
	case 0:
		return nil, errComputeServiceNotFound
	case 1:
		return &all[0], nil
	default:
		return nil, fmt.Errorf("%d nova-compute services report host %s; expected one", len(all), hostID)
	}
}

// waitForComputeService waits for the host's nova-compute service to register,
// which it does once the hypervisor role has converged.
func waitForComputeService(ctx context.Context, client *gophercloud.ServiceClient, hostID string) (*services.Service, error) {
	// The timeout is on the context, so it also ends a request that Nova never
	// answers: the HTTP client has no timeout of its own.
	ctx, cancel := context.WithTimeout(ctx, computeServiceRegisterTimeout)
	defer cancel()
	timedOut := func() error {
		return fmt.Errorf("host %s has no nova-compute service after %s. Check that host_id is the resmgr "+
			"host UUID of a hypervisor, and set wait_until_converged = true on its pcd_host_cluster_role so the "+
			"role converges before this resource is created: %w", hostID, computeServiceRegisterTimeout, ctx.Err())
	}
	for {
		svc, err := lookupComputeService(ctx, client, hostID)
		if err != nil && ctx.Err() != nil {
			return nil, timedOut()
		}
		if !errors.Is(err, errComputeServiceNotFound) {
			return svc, err
		}
		select {
		case <-ctx.Done():
			return nil, timedOut()
		case <-time.After(computeServicePollInterval):
		}
	}
}

// serviceStatusOf maps Nova's two fields onto the three states the PCD UI
// shows: a disabled service with a reason is in maintenance.
func serviceStatusOf(svc *services.Service) string {
	switch {
	case svc.Status == serviceEnabled:
		return serviceEnabled
	case svc.DisabledReason != "":
		return serviceMaintenance
	default:
		return serviceDisabled
	}
}

// newMaintenanceReason is the reason the PCD UI writes for maintenance mode.
func newMaintenanceReason(now time.Time) string {
	return fmt.Sprintf("%s:%d", uuid.NewString(), now.UnixMilli())
}

// maintenanceID returns the migration uuid in a UI-style maintenance reason.
func maintenanceID(reason string) (string, bool) {
	m := maintenanceReason.FindStringSubmatch(reason)
	if m == nil {
		return "", false
	}
	return m[1], true
}

// serviceUpdateFor returns the update that takes svc to want, or nil when it
// is already there. Entering maintenance writes a new UI-style reason; a host
// already in maintenance keeps the reason it has. Nova rejects a reason with
// "enabled", and "disabled" without one clears the old reason.
func serviceUpdateFor(svc *services.Service, want string, now time.Time) *services.UpdateOpts {
	if serviceStatusOf(svc) == want {
		return nil
	}
	switch want {
	case serviceEnabled:
		return &services.UpdateOpts{Status: services.ServiceEnabled}
	case serviceDisabled:
		return &services.UpdateOpts{Status: services.ServiceDisabled}
	default:
		return &services.UpdateOpts{Status: services.ServiceDisabled, DisabledReason: newMaintenanceReason(now)}
	}
}

// applyServiceStatus updates the service when it is not already in want and
// returns the service as Nova reports it afterward. gophercloud decodes a 200
// without a service object as an empty service, which is an error here rather
// than a service with no ID and no status.
func applyServiceStatus(ctx context.Context, client *gophercloud.ServiceClient, svc *services.Service, want string, now time.Time) (*services.Service, error) {
	opts := serviceUpdateFor(svc, want, now)
	if opts == nil {
		return svc, nil
	}
	updated, err := services.Update(ctx, withMicroversion(client, computeMicroversionServicesUUID), svc.ID, *opts).Extract()
	if err != nil {
		return nil, err
	}
	if updated.ID == "" {
		return nil, fmt.Errorf("the Compute API answered the update of service %s without a service object", svc.ID)
	}
	return updated, nil
}

func flattenComputeService(svc *services.Service, hostID string, m *computeServiceModel) {
	m.ID = types.StringValue(hostID)
	m.HostID = types.StringValue(hostID)
	m.Status = types.StringValue(serviceStatusOf(svc))
	m.ServiceID = types.StringValue(svc.ID)
	if svc.DisabledReason == "" {
		m.DisabledReason = types.StringNull()
	} else {
		m.DisabledReason = types.StringValue(svc.DisabledReason)
	}
	m.ComputeState = types.StringValue(svc.State)
}
