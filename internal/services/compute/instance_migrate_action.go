// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package compute

import (
	"context"
	"fmt"

	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/servers"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/action"
	"github.com/hashicorp/terraform-plugin-framework/action/schema"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/platform9/terraform-provider-pcd/internal/clients"
)

var (
	_ action.Action                   = (*instanceMigrateAction)(nil)
	_ action.ActionWithConfigure      = (*instanceMigrateAction)(nil)
	_ action.ActionWithValidateConfig = (*instanceMigrateAction)(nil)
)

// NewInstanceMigrateAction is the factory registered with the provider.
func NewInstanceMigrateAction() action.Action {
	return &instanceMigrateAction{}
}

type instanceMigrateAction struct {
	config *clients.Config
}

type instanceMigrateModel struct {
	InstanceID       types.String `tfsdk:"instance_id"`
	Mode             types.String `tfsdk:"mode"`
	Host             types.String `tfsdk:"host"`
	AvailabilityZone types.String `tfsdk:"availability_zone"`
	Confirm          types.Bool   `tfsdk:"confirm"`
	Region           types.String `tfsdk:"region"`
}

func (a *instanceMigrateAction) Metadata(_ context.Context, req action.MetadataRequest, resp *action.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_compute_instance_migrate"
}

func (a *instanceMigrateAction) Schema(_ context.Context, _ action.SchemaRequest, resp *action.SchemaResponse) {
	nonEmpty := []validator.String{stringvalidator.LengthAtLeast(1)}
	resp.Schema = schema.Schema{
		MarkdownDescription: "Moves an instance to another hypervisor, as the PCD UI's Migrate does: live for a running or " +
			"paused instance, cold for a shut-off or suspended one, to a host you name or one the scheduler picks, and " +
			"optionally to another cluster. A cold migration is confirmed unless `confirm = false`. The action waits for " +
			"Nova to finish and fails when the instance did not move, with Nova's reason (Nova accepts a migration and " +
			"reports a failure such as NoValidHost only afterward). The instance keeps its ID, addresses and volumes, " +
			"so `pcd_compute_instance` sees no change, except that a move to another cluster changes its availability " +
			"zone: when the instance sets `availability_zone`, change it to the new cluster in the configuration after " +
			"the action has moved the instance and before the next plan (changing it in the same apply as the move " +
			"plans a replacement). Nova refuses a move to the host the instance is on. PCD can also move instances " +
			"itself (Dynamic Resource Rebalancing, VM high availability). Requires an admin token and Terraform 1.14 or later.",
		Attributes: map[string]schema.Attribute{
			"instance_id": schema.StringAttribute{Required: true, MarkdownDescription: "The instance to move.", Validators: nonEmpty},
			"mode": schema.StringAttribute{Optional: true,
				MarkdownDescription: "`auto` (the default) migrates live when the instance is active or paused and cold when it " +
					"is shut off or suspended, as the UI does. `live` and `cold` force one kind: `cold` on an active " +
					"instance stops and restarts it, which the UI never does.",
				Validators: []validator.String{stringvalidator.OneOf("auto", "live", "cold")}},
			"host": schema.StringAttribute{Optional: true,
				MarkdownDescription: "The target hypervisor's resmgr host UUID (`data.pcd_host.<name>.id`). Omit it to let the scheduler choose.",
				Validators:          nonEmpty},
			"availability_zone": schema.StringAttribute{Optional: true,
				MarkdownDescription: "Move the instance to another cluster: the cluster's availability zone, which in PCD is " +
					"the cluster's name. Needs `host` (in that cluster) and a shut-off or suspended instance, as the UI " +
					"requires, and always migrates cold. Needs a PCD whose compute API offers microversion 2.101.",
				Validators: nonEmpty},
			"confirm": schema.BoolAttribute{Optional: true,
				MarkdownDescription: "Confirm a cold migration once Nova has moved the instance. Defaults to `true`. With " +
					"`false` the action ends with the instance in `VERIFY_RESIZE`, to be confirmed or reverted from the UI " +
					"or CLI; PCD confirms it by itself after a day."},
			"region": schema.StringAttribute{Optional: true,
				MarkdownDescription: "The region the instance is in. Defaults to the provider's region."},
		},
	}
}

func (a *instanceMigrateAction) Configure(_ context.Context, req action.ConfigureRequest, resp *action.ConfigureResponse) {
	a.config = configureClient(req.ProviderData, &resp.Diagnostics)
}

// ValidateConfig catches the combinations the UI never sends. Unknown values
// are left to Invoke.
func (a *instanceMigrateAction) ValidateConfig(ctx context.Context, req action.ValidateConfigRequest, resp *action.ValidateConfigResponse) {
	var cfg instanceMigrateModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &cfg)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if cfg.AvailabilityZone.IsNull() || cfg.AvailabilityZone.IsUnknown() {
		return
	}
	if cfg.Host.IsNull() {
		resp.Diagnostics.AddAttributeError(path.Root("host"), "host is required with availability_zone",
			"A move to another cluster needs the target host in that cluster, as in the PCD UI.")
	}
	if cfg.Mode.ValueString() == "live" {
		resp.Diagnostics.AddAttributeError(path.Root("mode"), "availability_zone needs a cold migration",
			"Nova moves an instance to another cluster only by cold migration; omit mode or set it to cold.")
	}
}

func (a *instanceMigrateAction) Invoke(ctx context.Context, req action.InvokeRequest, resp *action.InvokeResponse) {
	var cfg instanceMigrateModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &cfg)...)
	if resp.Diagnostics.HasError() {
		return
	}
	mode := cfg.Mode.ValueString()
	if mode == "" {
		mode = "auto"
	}
	confirm := cfg.Confirm.IsNull() || cfg.Confirm.ValueBool()
	id := cfg.InstanceID.ValueString()
	// ValidateConfig skips this check when availability_zone was unknown at
	// plan time; without a host the request would be a plain cold migration
	// that stays in the current cluster.
	if cfg.AvailabilityZone.ValueString() != "" && cfg.Host.ValueString() == "" {
		resp.Diagnostics.AddAttributeError(path.Root("host"), "host is required with availability_zone",
			"A move to another cluster needs the target host in that cluster, as in the PCD UI.")
		return
	}

	client, err := a.config.ForRegion(cfg.Region.ValueString()).ComputeV2Client()
	if err != nil {
		resp.Diagnostics.AddError("compute: building v2 client", err.Error())
		return
	}
	// A 200 without a server is an error of its own, not an instance whose
	// host the token cannot see.
	srv, err := clients.RequireObject(servers.Get(ctx, client, id).Extract())
	if err == nil && srv.ID == "" {
		err = clients.ErrNoObject
	}
	if err != nil {
		resp.Diagnostics.AddError("compute: reading instance", err.Error())
		return
	}
	live, err := resolveMigrateMode(mode, srv.Status, cfg.AvailabilityZone.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("Instance cannot be migrated this way", err.Error())
		return
	}
	res, err := migrateServer(ctx, client, srv, migrateRequest{
		Live: live, Host: cfg.Host.ValueString(), AvailabilityZone: cfg.AvailabilityZone.ValueString(), Confirm: confirm,
	}, func(msg string) { resp.SendProgress(action.InvokeProgressEvent{Message: msg}) })
	if err != nil {
		resp.Diagnostics.AddError("Migration failed", err.Error())
		return
	}
	if res.Unconfirmed {
		resp.Diagnostics.AddWarning("Cold migration not confirmed",
			fmt.Sprintf("Instance %s is on %s in VERIFY_RESIZE. Confirm or revert the migration from the PCD UI "+
				"(Confirm Resize); PCD confirms it by itself after a day.", id, res.Server.Host))
	}
	if host := cfg.Host.ValueString(); host != "" && res.Server.Host != host {
		resp.Diagnostics.AddWarning("Instance moved to another host",
			fmt.Sprintf("Instance %s moved to %s, not to the requested %s.", id, res.Server.Host, host))
	}
}

// resolveMigrateMode decides live or cold for an instance in status, or says
// why the instance cannot be moved that way. auto follows the PCD UI.
func resolveMigrateMode(mode, status, zone string) (live bool, err error) {
	if zone != "" {
		if mode == "live" {
			return false, fmt.Errorf("a move to another cluster is always a cold migration; omit mode or set it to cold")
		}
		if status != "SHUTOFF" && status != "SUSPENDED" {
			return false, fmt.Errorf("instance is %s; a move to another cluster needs a shut-off or suspended instance, as in the PCD UI", status)
		}
		return false, nil
	}
	switch mode {
	case "live":
		if !liveMigratable(status) {
			return false, fmt.Errorf("instance is %s; live migration needs an active or paused instance", status)
		}
		return true, nil
	case "cold":
		if !coldMigratable(status) {
			return false, fmt.Errorf("instance is %s; cold migration needs an active, shut-off or suspended instance", status)
		}
		return false, nil
	default:
		switch {
		case liveMigratable(status):
			return true, nil
		case coldMigratable(status):
			return false, nil
		}
		return false, fmt.Errorf("instance is %s; only active, paused, shut-off and suspended instances can be migrated", status)
	}
}
