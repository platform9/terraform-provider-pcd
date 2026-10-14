// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package compute

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/servers"
	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/tags"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/action"
	"github.com/hashicorp/terraform-plugin-framework/action/schema"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/platform9/terraform-provider-pcd/internal/clients"
)

var (
	_ action.Action              = (*hostDrainAction)(nil)
	_ action.ActionWithConfigure = (*hostDrainAction)(nil)
)

// NewHostDrainAction is the factory registered with the provider.
func NewHostDrainAction() action.Action {
	return &hostDrainAction{}
}

type hostDrainAction struct {
	config *clients.Config
}

type hostDrainModel struct {
	HostID         types.String `tfsdk:"host_id"`
	SkipPoweredOff types.Bool   `tfsdk:"skip_powered_off"`
	Region         types.String `tfsdk:"region"`
}

func (a *hostDrainAction) Metadata(_ context.Context, req action.MetadataRequest, resp *action.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_compute_host_drain"
}

func (a *hostDrainAction) Schema(_ context.Context, _ action.SchemaRequest, resp *action.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Moves every instance off a hypervisor that is in maintenance, as the PCD UI's Enable " +
			"Maintenance Mode does after it disables the host: active and paused instances live-migrate, shut-off and " +
			"suspended ones cold-migrate (and are confirmed), and the scheduler picks each target. Instances are moved " +
			"one at a time and each move is checked: an instance that is still on the host afterward is reported with " +
			"Nova's reason, and the others still move. Instances in any other state (resizing, rescued, in error, " +
			"shelved, or with a task in progress) are left in place with a warning. When the host was put in maintenance " +
			"the PCD UI's way (`pcd_compute_service` with `status = \"maintenance\"`), every instance moved is tagged with " +
			"the maintenance ID first, so the UI's maintenance progress view tracks it. Running the action again moves " +
			"whatever is still on the host. The host must already be disabled, so nothing new lands on it meanwhile; " +
			"on a host that still takes new instances the action moves nothing and warns. " +
			"Instances may move to another cluster: one managed by Terraform with `availability_zone` set then plans a " +
			"replacement, so check the plan after a drain. Requires an admin token and Terraform 1.14 or later.",
		Attributes: map[string]schema.Attribute{
			"host_id": schema.StringAttribute{Required: true,
				MarkdownDescription: "The hypervisor's resmgr host UUID, as `pcd_compute_service.<name>.host_id` or `data.pcd_host.<name>.id`.",
				Validators:          []validator.String{stringvalidator.LengthAtLeast(1)}},
			"skip_powered_off": schema.BoolAttribute{Optional: true,
				MarkdownDescription: "Leave shut-off instances on the host, as the UI's \"skip powered-off VMs\" does. " +
					"Suspended instances still move. Defaults to `false`."},
			"region": schema.StringAttribute{Optional: true,
				MarkdownDescription: "The region the hypervisor is in. Defaults to the provider's region."},
		},
	}
}

func (a *hostDrainAction) Configure(_ context.Context, req action.ConfigureRequest, resp *action.ConfigureResponse) {
	a.config = configureClient(req.ProviderData, &resp.Diagnostics)
}

func (a *hostDrainAction) Invoke(ctx context.Context, req action.InvokeRequest, resp *action.InvokeResponse) {
	var cfg hostDrainModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &cfg)...)
	if resp.Diagnostics.HasError() {
		return
	}
	hostID := cfg.HostID.ValueString()
	progress := func(msg string) { resp.SendProgress(action.InvokeProgressEvent{Message: msg}) }

	client, err := a.config.ForRegion(cfg.Region.ValueString()).ComputeV2Client()
	if err != nil {
		resp.Diagnostics.AddError("compute: building v2 client", err.Error())
		return
	}
	svc, err := lookupComputeService(ctx, client, hostID)
	if errors.Is(err, errComputeServiceNotFound) {
		resp.Diagnostics.AddError("Host has no compute service", fmt.Sprintf("Host %s has no nova-compute service; host_id must be a hypervisor's resmgr UUID.", hostID))
		return
	}
	if err != nil {
		resp.Diagnostics.AddError("compute: finding the host's compute service", err.Error())
		return
	}
	// A host that still takes new instances is not drained: an instance moved
	// off it could be replaced by a new one at once. Warning rather than
	// failing keeps a trigger tied to disabled_reason harmless when the host
	// goes back to enabled.
	if svc.Status != serviceDisabled {
		resp.Diagnostics.AddWarning("Host not in maintenance",
			fmt.Sprintf("Host %s still accepts new instances, so nothing was moved. Set status = \"maintenance\" "+
				"on its pcd_compute_service first.", hostID))
		return
	}
	tag, tagged := maintenanceID(svc.DisabledReason)
	if !tagged {
		resp.Diagnostics.AddWarning("No maintenance ID on the host",
			fmt.Sprintf("Host %s is disabled without the PCD UI's maintenance reason, so the instances moved are not "+
				"tagged and the UI's maintenance progress view will not track them.", hostID))
	}

	pages, err := servers.List(client, servers.ListOpts{AllTenants: true, Host: hostID}).AllPages(ctx)
	if err != nil {
		resp.Diagnostics.AddError("compute: listing the host's instances", err.Error())
		return
	}
	onHost, err := servers.ExtractServers(pages)
	if err != nil {
		resp.Diagnostics.AddError("compute: listing the host's instances", err.Error())
		return
	}
	moves, left := drainPlan(onHost, cfg.SkipPoweredOff.ValueBool())
	if len(left) > 0 {
		resp.Diagnostics.AddWarning("Instances left on the host", "These instances stay on "+hostID+": "+strings.Join(left, "; "))
	}

	// A canceled context (Terraform interrupted) stops the drain before the
	// next instance: the ones not yet started are reported as not attempted,
	// not as failed migrations, and are neither tagged nor moved.
	var failed, notAttempted []string
	canceled := false
	for i, m := range moves {
		if ctx.Err() != nil {
			canceled = true
			for _, rest := range moves[i:] {
				notAttempted = append(notAttempted, fmt.Sprintf("%s (%s)", rest.server.Name, rest.server.ID))
			}
			break
		}
		srv := m.server
		progress(fmt.Sprintf("%d/%d: %s (%s)", i+1, len(moves), srv.Name, srv.ID))
		if tagged {
			if err := tags.Add(ctx, withMicroversion(client, computeMicroversionServerTags), srv.ID, tag).ExtractErr(); err != nil {
				resp.Diagnostics.AddWarning("Tagging an instance failed",
					fmt.Sprintf("Instance %s was not tagged %s, so the PCD UI's progress view will not count it: %v", srv.ID, tag, err))
			}
		}
		_, err := migrateServer(ctx, client, &srv, migrateRequest{Live: m.live, Confirm: true}, progress)
		switch {
		case err != nil && ctx.Err() != nil:
			// Where the instance is now cannot be read on a canceled context.
			canceled = true
			failed = append(failed, fmt.Sprintf("%s (%s): %v; Nova may still be moving it", srv.Name, srv.ID, err))
		case err != nil:
			failed = append(failed, fmt.Sprintf("%s (%s): %v; %s", srv.Name, srv.ID, err, whereNow(ctx, client, srv.ID)))
		}
	}
	noun := "instances"
	if len(moves) == 1 {
		noun = "instance"
	}
	if canceled {
		attempted := len(moves) - len(notAttempted)
		verb := "were"
		if len(moves) == 1 {
			verb = "was"
		}
		detail := fmt.Sprintf("The drain of %s was canceled (%v) after %d of %d %s %s attempted. Run the action "+
			"again to move the instances still on the host.", hostID, ctx.Err(), attempted, len(moves), noun, verb)
		if len(failed) > 0 {
			detail += "\nFailed:\n" + strings.Join(failed, "\n")
		}
		if len(notAttempted) > 0 {
			detail += "\nNot attempted:\n" + strings.Join(notAttempted, "\n")
		}
		resp.Diagnostics.AddError("Host drain canceled", detail)
		return
	}
	if len(failed) > 0 {
		migrations := "migrations"
		if len(moves) == 1 {
			migrations = "migration"
		}
		resp.Diagnostics.AddError("Host drain incomplete",
			fmt.Sprintf("%d of %d %s off %s failed, and the host stays disabled. Each line below says where "+
				"the instance is now. An instance in VERIFY_RESIZE has already moved, and its cold migration must be "+
				"confirmed or reverted (Nova's confirm or revert resize, or the PCD UI) before the action runs again, "+
				"which sees only the instances still on the host. Fix the cause of any other failure and run the "+
				"action again.\n%s", len(failed), len(moves), migrations, hostID, strings.Join(failed, "\n")))
		return
	}
	progress(fmt.Sprintf("%d %s moved off %s", len(moves), noun, hostID))
}

// whereNow reads, once, where a failed migration left the instance. A cold
// migration whose confirm failed has already left the host and waits in
// VERIFY_RESIZE on another one, where running the drain again does not see it.
// The host is read at the default microversion, as the migration's own check
// reads it.
func whereNow(ctx context.Context, client *gophercloud.ServiceClient, id string) string {
	now, err := clients.RequireObject(servers.Get(ctx, client, id).Extract())
	if err == nil && now.ID == "" {
		err = clients.ErrNoObject
	}
	if err != nil {
		return fmt.Sprintf("its current state could not be read: %v", err)
	}
	where := "now " + now.Status
	if now.Host != "" {
		where += " on " + now.Host
	}
	if now.TaskState != "" {
		where += " (task " + now.TaskState + ")"
	}
	if now.Status == "VERIFY_RESIZE" {
		where += ", unconfirmed"
	}
	return where
}

// drainMove is one instance the drain will move, and how.
type drainMove struct {
	server servers.Server
	live   bool
}

// drainPlan picks the instances to move, as the PCD UI's maintenance dialog
// does: active and paused ones live, shut-off and suspended ones cold, and
// shut-off ones not at all when skipPoweredOff is set. Everything else, and
// anything with a task in progress, stays and is described in left.
func drainPlan(onHost []servers.Server, skipPoweredOff bool) (moves []drainMove, left []string) {
	for _, s := range onHost {
		switch {
		case s.TaskState != "":
			left = append(left, fmt.Sprintf("%s (%s) is busy: %s", s.Name, s.ID, s.TaskState))
		case liveMigratable(s.Status):
			moves = append(moves, drainMove{server: s, live: true})
		case s.Status == "SHUTOFF" && skipPoweredOff:
			left = append(left, fmt.Sprintf("%s (%s) is shut off and skip_powered_off is set", s.Name, s.ID))
		case coldMigratable(s.Status):
			moves = append(moves, drainMove{server: s})
		default:
			left = append(left, fmt.Sprintf("%s (%s) is %s, which cannot be migrated", s.Name, s.ID, s.Status))
		}
	}
	return moves, left
}
