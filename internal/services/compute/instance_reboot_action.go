// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package compute

import (
	"context"
	"fmt"
	"time"

	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/servers"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/action"
	"github.com/hashicorp/terraform-plugin-framework/action/schema"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/platform9/terraform-provider-pcd/internal/clients"
)

var (
	_ action.Action              = (*instanceRebootAction)(nil)
	_ action.ActionWithConfigure = (*instanceRebootAction)(nil)
)

// NewInstanceRebootAction is the factory registered with the provider.
func NewInstanceRebootAction() action.Action {
	return &instanceRebootAction{}
}

type instanceRebootAction struct {
	config *clients.Config
}

type instanceRebootModel struct {
	InstanceID types.String `tfsdk:"instance_id"`
	Type       types.String `tfsdk:"type"`
	Region     types.String `tfsdk:"region"`
}

func (a *instanceRebootAction) Metadata(_ context.Context, req action.MetadataRequest, resp *action.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_compute_instance_reboot"
}

func (a *instanceRebootAction) Schema(_ context.Context, _ action.SchemaRequest, resp *action.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Reboots a compute instance: the PCD UI's **Reboot** and **Hard Reboot** power actions. " +
			"A reboot is an event, not a state, so it is an action rather than a value of `power_state`. The action " +
			"waits until Nova has finished the reboot and fails unless the instance ends `ACTIVE` (it can end in " +
			"`ERROR`, or `SHUTOFF` when the guest shuts itself down). Nova records a reboot that fails while the guest " +
			"keeps running only in the instance's action log (`openstack server event list`), so the action reports " +
			"such a reboot as done. A soft reboot asks the guest operating system to restart and needs an `ACTIVE` instance; a hard reboot " +
			"power-cycles the instance and also works on one that is stopped, paused, suspended or in `ERROR`, which " +
			"makes it the usual way to recover an instance in `ERROR`. A hard reboot leaves the instance running, so " +
			"an instance whose `power_state` is `shutoff` is stopped again by the next apply. To reboot again from " +
			"a trigger, change the triggering resource, for example the `input` of a `terraform_data`.",
		Attributes: map[string]schema.Attribute{
			"instance_id": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "The ID of the instance to reboot.",
			},
			"type": schema.StringAttribute{
				Optional: true,
				MarkdownDescription: "`SOFT` (default) asks the guest to restart; `HARD` power-cycles the instance. When " +
					"the guest ignores a soft reboot, the hypervisor falls back to a hard one after its shutdown timeout.",
				Validators: []validator.String{stringvalidator.OneOf("SOFT", "HARD")},
			},
			"region": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "The region the instance is in. Defaults to the provider's region.",
			},
		},
	}
}

func (a *instanceRebootAction) Configure(_ context.Context, req action.ConfigureRequest, resp *action.ConfigureResponse) {
	a.config = configureClient(req.ProviderData, &resp.Diagnostics)
}

func (a *instanceRebootAction) Invoke(ctx context.Context, req action.InvokeRequest, resp *action.InvokeResponse) {
	var cfg instanceRebootModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &cfg)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := a.config.ForRegion(cfg.Region.ValueString()).ComputeV2Client()
	if err != nil {
		resp.Diagnostics.AddError("compute: building v2 client", err.Error())
		return
	}

	id := cfg.InstanceID.ValueString()
	method := servers.SoftReboot
	if cfg.Type.ValueString() == string(servers.HardReboot) {
		method = servers.HardReboot
	}
	resp.SendProgress(action.InvokeProgressEvent{Message: fmt.Sprintf("Requesting a %s reboot of instance %s", method, id)})
	if err := servers.Reboot(ctx, client, id, servers.RebootOpts{Type: method}).ExtractErr(); err != nil {
		resp.Diagnostics.AddError("compute: rebooting instance", err.Error())
		return
	}
	// Nova reports the old status (SHUTOFF, PAUSED, SUSPENDED, ERROR) for the
	// whole of a hard reboot from that status, so wait for the task to end,
	// not for a status change. A finished reboot leaves the instance ACTIVE;
	// any other steady status means it did not come back up.
	resp.SendProgress(action.InvokeProgressEvent{Message: fmt.Sprintf("Waiting for instance %s to finish rebooting", id)})
	settled, err := waitForServerSettled(ctx, client, id, []string{"ACTIVE", "SHUTOFF", "PAUSED", "SUSPENDED"}, 30*time.Minute)
	if err != nil {
		resp.Diagnostics.AddError("compute: waiting for instance to reboot", err.Error())
		return
	}
	if settled.Status != "ACTIVE" {
		resp.Diagnostics.AddError("compute: waiting for instance to reboot",
			fmt.Sprintf("Instance %s is %s after the reboot, not ACTIVE. The instance's action log "+
				"(openstack server event list %s) has the reason.", id, settled.Status, id))
		return
	}
	resp.SendProgress(action.InvokeProgressEvent{Message: fmt.Sprintf("Instance %s is ACTIVE", id)})
}
