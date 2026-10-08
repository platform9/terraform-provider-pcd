// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package compute

import (
	"context"
	"fmt"
	"time"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/servers"
	"github.com/hashicorp/terraform-plugin-framework/action"
	"github.com/hashicorp/terraform-plugin-framework/action/schema"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/platform9/terraform-provider-pcd/internal/clients"
)

var (
	_ action.Action              = (*instanceRebuildAction)(nil)
	_ action.ActionWithConfigure = (*instanceRebuildAction)(nil)
)

// actionProgressInterval is how often a waiting action tells Terraform it is
// still working. A var so unit tests can shorten it.
var actionProgressInterval = 30 * time.Second

// rebuildActionRerun ends the action's refusal advice. A plain apply does not
// run the action again once its trigger has fired, so it names the ways that do.
const rebuildActionRerun = "run the action again with terraform apply " +
	"-invoke=action.pcd_compute_instance_rebuild.<name>, or change the input of the resource whose " +
	"action_trigger runs it"

// NewInstanceRebuildAction is the factory registered with the provider.
func NewInstanceRebuildAction() action.Action {
	return &instanceRebuildAction{}
}

type instanceRebuildAction struct {
	config *clients.Config
}

type instanceRebuildActionModel struct {
	InstanceID types.String `tfsdk:"instance_id"`
	Region     types.String `tfsdk:"region"`
}

func (a *instanceRebuildAction) Metadata(_ context.Context, req action.MetadataRequest, resp *action.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_compute_instance_rebuild"
}

func (a *instanceRebuildAction) Schema(_ context.Context, _ action.SchemaRequest, resp *action.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Reimages an instance with the image it already runs: Nova erases the root disk and rewrites it " +
			"from that image, and the instance keeps its ID, ports and IP addresses, attached volumes, metadata, key pair " +
			"and user data. This is the PCD UI's Rebuild action with the image left as it is, used to return an instance to " +
			"its golden image. Nothing in Terraform state changes, so the action can run again at any time. To move an " +
			"instance to a different image, change `image_id` or `image_name` on `pcd_compute_instance` instead, which " +
			"rebuilds it in place. The instance must be `ACTIVE`, `SHUTOFF` or in `ERROR` with no task in progress; a " +
			"stopped instance is stopped again afterward.",
		Attributes: map[string]schema.Attribute{
			"instance_id": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "The ID of the instance to reimage.",
			},
			"region": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "The region the instance is in. Defaults to the provider's region.",
			},
		},
	}
}

func (a *instanceRebuildAction) Configure(_ context.Context, req action.ConfigureRequest, resp *action.ConfigureResponse) {
	a.config = configureClient(req.ProviderData, &resp.Diagnostics)
}

func (a *instanceRebuildAction) Invoke(ctx context.Context, req action.InvokeRequest, resp *action.InvokeResponse) {
	var cfg instanceRebuildActionModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &cfg)...)
	if resp.Diagnostics.HasError() {
		return
	}
	id := cfg.InstanceID.ValueString()

	client, err := a.config.ForRegion(cfg.Region.ValueString()).ComputeV2Client()
	if err != nil {
		resp.Diagnostics.AddError("compute: building v2 client", err.Error())
		return
	}
	before, err := getRebuildable(ctx, client, id)
	if err != nil {
		resp.Diagnostics.AddError("compute: rebuilding instance", withRebuildAdvice(err,
			"Once the task finishes, "+rebuildActionRerun,
			"Unpause or resume it first (with power_state = \"active\" on its pcd_compute_instance in a "+
				"separate apply, for example), then "+rebuildActionRerun,
			"Unrescue it first, by removing its pcd_compute_instance_rescue or with Unrescue in the PCD UI, "+
				"then "+rebuildActionRerun))
		return
	}
	image := serverImageID(before)
	if image == "" {
		resp.Diagnostics.AddError("compute: rebuilding instance",
			fmt.Sprintf("Instance %s boots from a volume; this action reimages an instance booted from an image.", id))
		return
	}
	imgClient, err := a.config.ForRegion(cfg.Region.ValueString()).ImageV2Client()
	if err != nil {
		resp.Diagnostics.AddError("compute: building image v2 client", err.Error())
		return
	}
	if err := checkRebuildTarget(ctx, imgClient, image); err != nil {
		resp.Diagnostics.AddError("compute: rebuilding instance", err.Error())
		return
	}

	resp.SendProgress(action.InvokeProgressEvent{Message: fmt.Sprintf("Rebuilding instance %s from image %s", id, image)})
	if err := sendRebuild(ctx, client, id, image, ""); err != nil {
		resp.Diagnostics.AddError("compute: rebuilding instance", err.Error()+"\n\n"+rebuildRefusalHint)
		return
	}
	server, err := settleWithProgress(ctx, client, id, rebuildSettleTarget(before.Status), rebuildTimeout, resp.SendProgress)
	if err != nil {
		resp.Diagnostics.AddError("compute: waiting for instance rebuild", err.Error())
		return
	}
	resp.SendProgress(action.InvokeProgressEvent{Message: fmt.Sprintf("Instance %s rebuilt; status %s", id, server.Status)})
}

// settleWithProgress waits like waitForServerSettled and, while it polls,
// tells Terraform every actionProgressInterval that the action is still
// running, so a rebuild that takes minutes does not look stalled. send is
// called only from this goroutine.
func settleWithProgress(ctx context.Context, client *gophercloud.ServiceClient, id string, want []string, timeout time.Duration, send func(action.InvokeProgressEvent)) (*servers.Server, error) {
	type result struct {
		server *servers.Server
		err    error
	}
	done := make(chan result, 1)
	go func() {
		server, err := waitForServerSettled(ctx, client, id, want, timeout)
		done <- result{server, err}
	}()
	tick := time.NewTicker(actionProgressInterval)
	defer tick.Stop()
	start := time.Now()
	for {
		select {
		case res := <-done:
			return res.server, res.err
		case <-tick.C:
			send(action.InvokeProgressEvent{Message: fmt.Sprintf("Waiting for instance %s to reach %v (%s elapsed)",
				id, want, time.Since(start).Round(time.Second))})
		}
	}
}
