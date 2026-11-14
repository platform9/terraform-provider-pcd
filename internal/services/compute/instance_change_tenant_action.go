// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package compute

import (
	"context"
	"errors"
	"fmt"
	"regexp"

	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/action"
	"github.com/hashicorp/terraform-plugin-framework/action/schema"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/platform9/terraform-provider-pcd/internal/clients"
)

var (
	_ action.Action              = (*instanceChangeTenantAction)(nil)
	_ action.ActionWithConfigure = (*instanceChangeTenantAction)(nil)
)

// NewInstanceChangeTenantAction is the factory registered with the provider.
func NewInstanceChangeTenantAction() action.Action {
	return &instanceChangeTenantAction{}
}

type instanceChangeTenantAction struct {
	config *clients.Config
}

type instanceChangeTenantModel struct {
	InstanceID types.String `tfsdk:"instance_id"`
	ProjectID  types.String `tfsdk:"project_id"`
	Region     types.String `tfsdk:"region"`
}

func (a *instanceChangeTenantAction) Metadata(_ context.Context, req action.MetadataRequest, resp *action.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_compute_instance_change_tenant"
}

func (a *instanceChangeTenantAction) Schema(_ context.Context, _ action.SchemaRequest, resp *action.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Moves an instance to another project, with its ports and attached volumes: the PCD UI's " +
			"**Change Tenant**. The instance keeps its ID, addresses and owner. Nova checks everything before it moves " +
			"anything and refuses with its reason, for example an instance with a floating IP, one in a server group, " +
			"a security group the target project does not own, or a volume with snapshots; the action then fails with " +
			"Nova's message. After Nova answers, the action reads the instance until it reports the new project.\n\n" +
			"Needs an admin token, and a PCD release whose Nova has the `changeTenant` action (PCD 2026.8 has it). " +
			"Afterwards only an admin token can read the instance from a configuration scoped to the old project, " +
			"so a configuration that manages the instance hands it over to the target project's configuration: see " +
			"\"Moving an instance to another project\" in the importing guide. The instance must be active, shut " +
			"off, paused or suspended, with no task in progress, and not locked.",
		Attributes: map[string]schema.Attribute{
			"instance_id": schema.StringAttribute{Required: true, MarkdownDescription: "The ID of the instance to move.",
				Validators: []validator.String{
					stringvalidator.RegexMatches(changeTenantIDShape, "must be an instance ID made of letters, digits, '-' and '_'"),
				}},
			"project_id": schema.StringAttribute{Required: true,
				MarkdownDescription: "The ID of the project to move the instance to. When the instance is already " +
					"there, the action moves nothing and warns.",
				Validators: []validator.String{
					stringvalidator.RegexMatches(changeTenantProjectIDShape, "must be a project ID in lower case, made of lower-case letters, digits, '-' and '_'"),
				}},
			"region": schema.StringAttribute{Optional: true,
				MarkdownDescription: "The region the instance is in. Defaults to the provider's region."},
		},
	}
}

func (a *instanceChangeTenantAction) Configure(_ context.Context, req action.ConfigureRequest, resp *action.ConfigureResponse) {
	a.config = configureClient(req.ProviderData, &resp.Diagnostics)
}

func (a *instanceChangeTenantAction) Invoke(ctx context.Context, req action.InvokeRequest, resp *action.InvokeResponse) {
	var cfg instanceChangeTenantModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &cfg)...)
	if resp.Diagnostics.HasError() {
		return
	}
	id, target := cfg.InstanceID.ValueString(), cfg.ProjectID.ValueString()
	// The validator skips a value that was unknown at plan time; the ID goes
	// into the URL path, so it is checked before any request.
	if err := checkInstanceIDShape(id); err != nil {
		resp.Diagnostics.AddAttributeError(path.Root("instance_id"), "Invalid instance ID", err.Error())
		return
	}

	if !changeTenantProjectIDShape.MatchString(target) {
		resp.Diagnostics.AddAttributeError(path.Root("project_id"), "Invalid project ID", fmt.Sprintf(
			"project_id %q is not a project ID: a project ID in lower case is expected, made of lower-case letters, digits, '-' or '_'.", target))
		return
	}

	client, err := a.config.ForRegion(cfg.Region.ValueString()).ComputeV2Client()
	if err != nil {
		resp.Diagnostics.AddError("compute: building v2 client", err.Error())
		return
	}
	server, err := readInstanceForMove(ctx, client, id)
	if errors.Is(err, errInstanceIDMismatch) {
		resp.Diagnostics.AddAttributeError(path.Root("instance_id"), "Answer for another instance", err.Error())
		return
	}
	if err != nil {
		resp.Diagnostics.AddError("compute: reading instance", err.Error())
		return
	}
	// Nova refuses a move to the project the instance is in; re-running the
	// action after a move it already made does nothing instead.
	if server.TenantID == target {
		detail := fmt.Sprintf("Instance %s is already in project %s, with status %s; nothing was moved.", id, target, server.Status)
		if server.Status == "ERROR" {
			detail += fmt.Sprintf(" Because it is in ERROR, its volumes and ports may not all be in project %s: "+
				"check the project of each.", target)
		}
		resp.Diagnostics.AddWarning("Instance already in the target project", detail)
		return
	}

	resp.SendProgress(action.InvokeProgressEvent{Message: fmt.Sprintf(
		"Moving instance %s from project %s to project %s, with its ports and volumes", id, server.TenantID, target)})
	if err := moveInstanceToProject(ctx, client, id, target); err != nil {
		source := server.TenantID
		recovery := changeTenantRecovery(id, source, target)
		// Only Nova's own refusals can leave the instance where it was. Any
		// other failure (a transport error, an ingress's 502 or 504, a 500)
		// may come after Nova moved the instance.
		hint := changeTenantRefusalHint(err)
		if hint == "" {
			resp.Diagnostics.AddError("Move outcome unknown", fmt.Sprintf("%s\n\nNova may have moved the instance, "+
				"or part of it, before the request failed. %s", err, recovery))
			return
		}
		// A refusal can also come from a failure part way through, after
		// which Nova moves back what it moved, and sets ERROR when it cannot.
		// So read the instance again before saying that nothing moved.
		after, rerr := readInstanceForMove(ctx, client, id)
		switch {
		case rerr != nil:
			resp.Diagnostics.AddError("Move outcome unknown", fmt.Sprintf("%s\n\nNova refused the move, but instance %s "+
				"could not be read afterward: %v. %s", err, id, rerr, recovery))
		// An instance already in ERROR before the move is refused by Nova
		// before it touches anything, so ERROR afterward says nothing then.
		case after.TenantID != source || (after.Status == "ERROR" && server.Status != "ERROR"):
			resp.Diagnostics.AddError("Move outcome unknown", fmt.Sprintf("%s\n\nNova refused the move, but instance %s "+
				"now reports project %s, status %s. %s", err, id, after.TenantID, after.Status, recovery))
		default:
			resp.Diagnostics.AddError("Instance not moved", err.Error()+"\n\n"+hint)
		}
		return
	}
	resp.SendProgress(action.InvokeProgressEvent{Message: fmt.Sprintf("Nova moved instance %s; confirming its project", id)})
	moved, err := waitForInstanceProject(ctx, client, id, target, func(msg string) {
		resp.SendProgress(action.InvokeProgressEvent{Message: msg})
	})
	if err != nil {
		resp.Diagnostics.AddError("Move not confirmed", fmt.Sprintf("%s\n\nNova accepted the move, which it does only "+
			"after it has moved the instance. Read instance %s's project and status under an admin token, for example "+
			"with the pcd_compute_instance data source: when it reports project %s, the move finished, so go on with "+
			"the handoff described in the importing guide. When it reports another project or is in ERROR, "+
			"something changed it after Nova accepted this move, and the steps under \"Move outcome unknown\" in "+
			"the importing guide apply.", err, id, target))
		return
	}
	resp.SendProgress(action.InvokeProgressEvent{Message: fmt.Sprintf("Instance %s is in project %s", id, moved.TenantID)})
}

// changeTenantProjectIDShape is the shape of a Keystone project ID: Keystone
// writes its IDs in lower case.
var changeTenantProjectIDShape = regexp.MustCompile(`^[a-z0-9_-]+$`)

// changeTenantRecovery says how to find out where a move whose outcome is
// unknown left the instance, and what to do in each case.
func changeTenantRecovery(id, source, target string) string {
	return fmt.Sprintf("If the request timed out, Nova may still be working on the move, so wait a few minutes "+
		"before you read the instance. Then read instance %s's project and status under an admin token, for "+
		"example with the pcd_compute_instance data source. In project %s, the move finished: go on with the "+
		"handoff described in the importing guide. In project %s and not in ERROR, nothing moved: run the action "+
		"again. In ERROR, Nova could not move every volume and port back, and it refuses another move while the "+
		"instance is in ERROR: an administrator has to put each attached volume and port back in the instance's "+
		"project and reset the instance's state before the action can run again.", id, target, source)
}
