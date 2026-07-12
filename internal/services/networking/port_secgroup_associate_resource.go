// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0
//
// Ported from terraform-provider-openstack v3.4.0
// (openstack/resource_openstack_networking_port_secgroup_associate_v2.go),
// adapted for the terraform-plugin-framework and PCD.

package networking

import (
	"context"
	"net/http"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/ports"
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
	_ resource.Resource                = (*portSecgroupAssociateResource)(nil)
	_ resource.ResourceWithConfigure   = (*portSecgroupAssociateResource)(nil)
	_ resource.ResourceWithImportState = (*portSecgroupAssociateResource)(nil)
)

// NewPortSecgroupAssociateResource is the factory registered with the provider.
func NewPortSecgroupAssociateResource() resource.Resource {
	return &portSecgroupAssociateResource{}
}

type portSecgroupAssociateResource struct {
	config *clients.Config
}

type portSecgroupAssociateModel struct {
	ID                  types.String `tfsdk:"id"`
	PortID              types.String `tfsdk:"port_id"`
	SecurityGroupIDs    types.Set    `tfsdk:"security_group_ids"`
	Enforce             types.Bool   `tfsdk:"enforce"`
	AllSecurityGroupIDs types.Set    `tfsdk:"all_security_group_ids"`
	Region              types.String `tfsdk:"region"`
}

func (r *portSecgroupAssociateResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_networking_port_secgroup_associate"
}

func (r *portSecgroupAssociateResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Manages the security groups on a port that is not itself managed by this provider " +
			"(for example a port Nova created for an instance). With `enforce = false` (default) the listed groups " +
			"are added to whatever the port already has and only those are removed on destroy. With `enforce = true` " +
			"the port's groups become exactly the listed set, and destroy removes all of them.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{Computed: true, MarkdownDescription: "The port ID (same as `port_id`).", PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()}},
			"port_id": schema.StringAttribute{Required: true, MarkdownDescription: "The port to manage security groups on. Changing this forces a new resource.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()}},
			"security_group_ids":     schema.SetAttribute{Required: true, ElementType: types.StringType, MarkdownDescription: "The security group IDs this resource manages on the port."},
			"enforce":                schema.BoolAttribute{Optional: true, Computed: true, Default: booldefault.StaticBool(false), MarkdownDescription: "If true, the port's security groups become exactly `security_group_ids` (exclusive). If false, they are added to the port's existing groups."},
			"all_security_group_ids": schema.SetAttribute{Computed: true, ElementType: types.StringType, MarkdownDescription: "All security group IDs currently on the port."},
			"region":                 schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "The region. Defaults to the provider's region.", PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()}},
		},
	}
}

func (r *portSecgroupAssociateResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.config = configureClient(req.ProviderData, &resp.Diagnostics)
}

func (r *portSecgroupAssociateResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan portSecgroupAssociateModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := r.config.NetworkV2Client()
	if err != nil {
		resp.Diagnostics.AddError("networking: building v2 client", err.Error())
		return
	}

	portID := plan.PortID.ValueString()
	var managed []string
	resp.Diagnostics.Append(plan.SecurityGroupIDs.ElementsAs(ctx, &managed, false)...)
	if resp.Diagnostics.HasError() {
		return
	}

	neutronParentMu.Lock(portID)
	defer neutronParentMu.Unlock(portID)

	port, err := ports.Get(ctx, client, portID).Extract()
	if err != nil {
		resp.Diagnostics.AddError("networking: reading port", err.Error())
		return
	}

	var newSGs []string
	if plan.Enforce.ValueBool() {
		newSGs = managed
	} else {
		newSGs = unionStrings(port.SecurityGroups, managed)
	}
	if err := updatePortSecurityGroups(ctx, client, portID, newSGs); err != nil {
		resp.Diagnostics.AddError("networking: setting port security groups", err.Error())
		return
	}

	plan.ID = types.StringValue(portID)
	all, d := types.SetValueFrom(ctx, types.StringType, newSGs)
	resp.Diagnostics.Append(d...)
	plan.AllSecurityGroupIDs = all
	if plan.Region.IsNull() || plan.Region.IsUnknown() {
		plan.Region = types.StringValue(r.config.Region)
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *portSecgroupAssociateResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state portSecgroupAssociateModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := r.config.NetworkV2Client()
	if err != nil {
		resp.Diagnostics.AddError("networking: building v2 client", err.Error())
		return
	}

	port, err := ports.Get(ctx, client, state.PortID.ValueString()).Extract()
	if err != nil {
		if gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
			resp.State.RemoveResource(ctx)
			return
		}
		resp.Diagnostics.AddError("networking: reading port", err.Error())
		return
	}
	actual := port.SecurityGroups

	var managed []string
	if state.SecurityGroupIDs.IsNull() {
		// Fresh import: adopt all groups currently on the port.
		managed = actual
	} else {
		resp.Diagnostics.Append(state.SecurityGroupIDs.ElementsAs(ctx, &managed, false)...)
		if resp.Diagnostics.HasError() {
			return
		}
	}

	var reported []string
	if state.Enforce.ValueBool() {
		reported = actual
	} else {
		reported = intersectStrings(managed, actual)
	}

	state.ID = types.StringValue(port.ID)
	sgSet, d := types.SetValueFrom(ctx, types.StringType, reported)
	resp.Diagnostics.Append(d...)
	state.SecurityGroupIDs = sgSet
	allSet, d := types.SetValueFrom(ctx, types.StringType, actual)
	resp.Diagnostics.Append(d...)
	state.AllSecurityGroupIDs = allSet
	if state.Enforce.IsNull() {
		state.Enforce = types.BoolValue(false)
	}
	if state.Region.IsNull() || state.Region.IsUnknown() {
		state.Region = types.StringValue(r.config.Region)
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *portSecgroupAssociateResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state portSecgroupAssociateModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := r.config.NetworkV2Client()
	if err != nil {
		resp.Diagnostics.AddError("networking: building v2 client", err.Error())
		return
	}

	portID := plan.PortID.ValueString()
	var oldManaged, newManaged []string
	resp.Diagnostics.Append(state.SecurityGroupIDs.ElementsAs(ctx, &oldManaged, false)...)
	resp.Diagnostics.Append(plan.SecurityGroupIDs.ElementsAs(ctx, &newManaged, false)...)
	if resp.Diagnostics.HasError() {
		return
	}

	neutronParentMu.Lock(portID)
	defer neutronParentMu.Unlock(portID)

	port, err := ports.Get(ctx, client, portID).Extract()
	if err != nil {
		resp.Diagnostics.AddError("networking: reading port", err.Error())
		return
	}

	var newSGs []string
	if plan.Enforce.ValueBool() {
		newSGs = newManaged
	} else {
		removed := subtractStrings(oldManaged, newManaged)
		newSGs = unionStrings(subtractStrings(port.SecurityGroups, removed), newManaged)
	}
	if err := updatePortSecurityGroups(ctx, client, portID, newSGs); err != nil {
		resp.Diagnostics.AddError("networking: updating port security groups", err.Error())
		return
	}

	plan.ID = types.StringValue(portID)
	all, d := types.SetValueFrom(ctx, types.StringType, newSGs)
	resp.Diagnostics.Append(d...)
	plan.AllSecurityGroupIDs = all
	if plan.Region.IsNull() || plan.Region.IsUnknown() {
		plan.Region = types.StringValue(r.config.Region)
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *portSecgroupAssociateResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state portSecgroupAssociateModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := r.config.NetworkV2Client()
	if err != nil {
		resp.Diagnostics.AddError("networking: building v2 client", err.Error())
		return
	}

	portID := state.PortID.ValueString()
	var managed []string
	resp.Diagnostics.Append(state.SecurityGroupIDs.ElementsAs(ctx, &managed, false)...)
	if resp.Diagnostics.HasError() {
		return
	}

	neutronParentMu.Lock(portID)
	defer neutronParentMu.Unlock(portID)

	port, err := ports.Get(ctx, client, portID).Extract()
	if err != nil {
		if gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
			return
		}
		resp.Diagnostics.AddError("networking: reading port", err.Error())
		return
	}

	var newSGs []string
	if state.Enforce.ValueBool() {
		newSGs = []string{}
	} else {
		newSGs = subtractStrings(port.SecurityGroups, managed)
	}
	if err := updatePortSecurityGroups(ctx, client, portID, newSGs); err != nil {
		resp.Diagnostics.AddError("networking: clearing port security groups", err.Error())
		return
	}
}

func (r *portSecgroupAssociateResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), req.ID)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("port_id"), req.ID)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("enforce"), false)...)
}

// updatePortSecurityGroups replaces the full security-group list on a port.
func updatePortSecurityGroups(ctx context.Context, client *gophercloud.ServiceClient, portID string, sgs []string) error {
	if sgs == nil {
		sgs = []string{}
	}
	_, err := ports.Update(ctx, client, portID, ports.UpdateOpts{SecurityGroups: &sgs}).Extract()
	return err
}

func unionStrings(a, b []string) []string {
	seen := make(map[string]bool, len(a)+len(b))
	out := make([]string, 0, len(a)+len(b))
	for _, list := range [][]string{a, b} {
		for _, s := range list {
			if !seen[s] {
				seen[s] = true
				out = append(out, s)
			}
		}
	}
	return out
}

func subtractStrings(a, remove []string) []string {
	rm := make(map[string]bool, len(remove))
	for _, s := range remove {
		rm[s] = true
	}
	out := make([]string, 0, len(a))
	for _, s := range a {
		if !rm[s] {
			out = append(out, s)
		}
	}
	return out
}

func intersectStrings(a, b []string) []string {
	bs := make(map[string]bool, len(b))
	for _, s := range b {
		bs[s] = true
	}
	out := make([]string, 0, len(a))
	for _, s := range a {
		if bs[s] {
			out = append(out, s)
		}
	}
	return out
}
