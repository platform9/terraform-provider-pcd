// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0
//
// Ported from terraform-provider-openstack v3.4.0
// (openstack/resource_openstack_networking_network_v2.go), adapted for the
// terraform-plugin-framework and PCD.

package networking

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/extensions/dns"
	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/extensions/external"
	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/extensions/portsecurity"
	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/extensions/provider"
	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/networks"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/listplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/setplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringdefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/platform9/terraform-provider-pcd/internal/clients"
)

var (
	_ resource.Resource                   = (*networkResource)(nil)
	_ resource.ResourceWithConfigure      = (*networkResource)(nil)
	_ resource.ResourceWithImportState    = (*networkResource)(nil)
	_ resource.ResourceWithValidateConfig = (*networkResource)(nil)
)

// NewNetworkResource is the factory registered with the provider.
func NewNetworkResource() resource.Resource {
	return &networkResource{}
}

type networkResource struct {
	config *clients.Config
}

type networkModel struct {
	ID           types.String `tfsdk:"id"`
	Name         types.String `tfsdk:"name"`
	Description  types.String `tfsdk:"description"`
	AdminStateUp types.Bool   `tfsdk:"admin_state_up"`
	Shared       types.Bool   `tfsdk:"shared"`
	External     types.Bool   `tfsdk:"external"`
	TenantID     types.String `tfsdk:"tenant_id"`
	Tags         types.Set    `tfsdk:"tags"`
	Region       types.String `tfsdk:"region"`
	Segments     types.List   `tfsdk:"segments"`
	PortSecurity types.Bool   `tfsdk:"port_security_enabled"`
	DNSDomain    types.String `tfsdk:"dns_domain"`
}

type segmentModel struct {
	PhysicalNetwork types.String `tfsdk:"physical_network"`
	NetworkType     types.String `tfsdk:"network_type"`
	SegmentationID  types.Int64  `tfsdk:"segmentation_id"`
}

// networkExtended embeds the base network plus the external-router extension so
// a single Get returns everything.
type networkExtended struct {
	networks.Network
	external.NetworkExternalExt
	portsecurity.PortSecurityExt
	dns.NetworkDNSExt
}

func (r *networkResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_networking_network"
}

func (r *networkResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Manages a Neutron network in PCD.",
		Attributes: map[string]schema.Attribute{
			"id":   schema.StringAttribute{Computed: true, MarkdownDescription: "The network ID.", PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()}},
			"name": schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "The name of the network.", PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()}},
			"description": schema.StringAttribute{
				Optional: true, Computed: true, MarkdownDescription: "A description of the network.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"admin_state_up": schema.BoolAttribute{Optional: true, Computed: true, Default: booldefault.StaticBool(true), MarkdownDescription: "The administrative state of the network."},
			"shared":         schema.BoolAttribute{Optional: true, Computed: true, MarkdownDescription: "Whether the network is shared across projects."},
			"external":       schema.BoolAttribute{Optional: true, Computed: true, MarkdownDescription: "Whether the network has an external routing facility."},
			"tenant_id": schema.StringAttribute{
				Optional: true, Computed: true, MarkdownDescription: "The owning project. Changing this forces a new resource.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace(), stringplanmodifier.UseStateForUnknown()},
			},
			"tags":   schema.SetAttribute{Optional: true, Computed: true, ElementType: types.StringType, MarkdownDescription: "Tags applied to the network.", PlanModifiers: []planmodifier.Set{setplanmodifier.UseStateForUnknown()}},
			"region": schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "The region. Defaults to the provider's region.", PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()}},
			"port_security_enabled": schema.BoolAttribute{
				Optional: true, Computed: true,
				MarkdownDescription: "Whether port security (security groups and anti-spoofing) is enforced on ports of this network. " +
					"Defaults to `true`. Set `false` for a Layer 2 / \"Simple\" network, where the VM manages its own addressing and " +
					"security groups do not apply — mirrors the PCD UI's Simple Network option.",
			},
			"dns_domain": schema.StringAttribute{
				Optional: true, Computed: true, Default: stringdefault.StaticString(""),
				MarkdownDescription: "The Designate zone that ports on this network publish DNS records to, as a fully " +
					"qualified name ending in a dot (typically `pcd_dns_zone.example.name`), in lowercase: Neutron stores " +
					"the value lower-cased, so a mixed-case literal is refused at plan time. A network maps to at most one " +
					"zone, which must already exist. Defaults to `\"\"`, which is also how an association is removed: omit " +
					"the attribute and the next apply clears it, so add it to the configuration of any network whose zone " +
					"was set outside Terraform. Which fixed IPs get records depends on the subnets' `dns_publish_fixed_ip` " +
					"and on whether the network is external; the DNS guide explains the rules. Records are created when a " +
					"port is created, so set this before booting instances.",
			},
			"segments": schema.ListNestedAttribute{
				Optional: true,
				MarkdownDescription: "Provider-network segments (admin only). One segment creates a physical network " +
					"(e.g. `network_type = \"flat\"` / `\"vlan\"` on a `physical_network` label from the host config); " +
					"multiple segments create a multi-provider network. Create-only: segments are not refreshed from " +
					"the API and cannot be imported. Changing this forces a new resource.",
				PlanModifiers: []planmodifier.List{listplanmodifier.RequiresReplace()},
				NestedObject: schema.NestedAttributeObject{
					Attributes: map[string]schema.Attribute{
						"physical_network": schema.StringAttribute{Optional: true, MarkdownDescription: "The physical network label (e.g. `physnet1`, as mapped in the host configuration's `network_labels`)."},
						"network_type":     schema.StringAttribute{Optional: true, MarkdownDescription: "The segment type: `flat`, `vlan`, `vxlan`, or `geneve`."},
						"segmentation_id":  schema.Int64Attribute{Optional: true, MarkdownDescription: "The segmentation ID (e.g. VLAN ID). Omit for `flat`."},
					},
				},
			},
		},
	}
}

// segmentsCreateOptsExt injects provider-network attributes into the create
// body. A single segment is sent as top-level `provider:*` keys — the form
// Neutron's provider extension expects for ordinary physical networks (and the
// one OVN accepts); multiple segments use the multi-provider `segments` list.
type segmentsCreateOptsExt struct {
	networks.CreateOptsBuilder
	segments []provider.Segment
}

func (o segmentsCreateOptsExt) ToNetworkCreateMap() (map[string]any, error) {
	base, err := o.CreateOptsBuilder.ToNetworkCreateMap()
	if err != nil {
		return nil, err
	}
	net, ok := base["network"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("networking: unexpected create body shape (no network object)")
	}
	switch len(o.segments) {
	case 0:
	case 1:
		s := o.segments[0]
		if s.NetworkType != "" {
			net["provider:network_type"] = s.NetworkType
		}
		if s.PhysicalNetwork != "" {
			net["provider:physical_network"] = s.PhysicalNetwork
		}
		if s.SegmentationID != 0 {
			net["provider:segmentation_id"] = s.SegmentationID
		}
	default:
		net["segments"] = o.segments
	}
	return base, nil
}

// networkCreateOpts builds the create body: the base options, then each
// extension the plan uses wrapped around it. Kept apart from Create so the
// wire body can be unit-tested without a lab.
func networkCreateOpts(ctx context.Context, plan *networkModel, diags *diag.Diagnostics) networks.CreateOptsBuilder {
	adminUp := plan.AdminStateUp.ValueBool()
	base := networks.CreateOpts{
		Name:         plan.Name.ValueString(),
		Description:  plan.Description.ValueString(),
		AdminStateUp: &adminUp,
		TenantID:     plan.TenantID.ValueString(),
	}
	if !plan.Shared.IsNull() && !plan.Shared.IsUnknown() {
		shared := plan.Shared.ValueBool()
		base.Shared = &shared
	}

	var createOpts networks.CreateOptsBuilder = base
	if !plan.External.IsNull() && !plan.External.IsUnknown() {
		ext := plan.External.ValueBool()
		createOpts = external.CreateOptsExt{CreateOptsBuilder: createOpts, External: &ext}
	}
	if !plan.PortSecurity.IsNull() && !plan.PortSecurity.IsUnknown() {
		ps := plan.PortSecurity.ValueBool()
		createOpts = portsecurity.NetworkCreateOptsExt{CreateOptsBuilder: createOpts, PortSecurityEnabled: &ps}
	}
	if !plan.Segments.IsNull() && !plan.Segments.IsUnknown() {
		var segs []segmentModel
		diags.Append(plan.Segments.ElementsAs(ctx, &segs, false)...)
		if diags.HasError() {
			return createOpts
		}
		providerSegs := make([]provider.Segment, 0, len(segs))
		for _, s := range segs {
			providerSegs = append(providerSegs, provider.Segment{
				PhysicalNetwork: s.PhysicalNetwork.ValueString(),
				NetworkType:     s.NetworkType.ValueString(),
				SegmentationID:  int(s.SegmentationID.ValueInt64()),
			})
		}
		createOpts = segmentsCreateOptsExt{CreateOptsBuilder: createOpts, segments: providerSegs}
	}
	// The dns extension, only when a zone is named: "" is the server default,
	// and sending the key at all is a 400 on a Neutron without the extension.
	if v := plan.DNSDomain.ValueString(); v != "" {
		createOpts = dns.NetworkCreateOptsExt{CreateOptsBuilder: createOpts, DNSDomain: v}
	}
	return createOpts
}

// networkUpdateOpts builds the update body from what changed between plan and
// state. dns_domain is sent whenever it differs, including a change to "",
// which is how the association with a zone is removed.
func networkUpdateOpts(plan, state *networkModel) networks.UpdateOptsBuilder {
	name := plan.Name.ValueString()
	description := plan.Description.ValueString()
	adminUp := plan.AdminStateUp.ValueBool()
	base := networks.UpdateOpts{Name: &name, Description: &description, AdminStateUp: &adminUp}
	if !plan.Shared.IsNull() && !plan.Shared.IsUnknown() {
		shared := plan.Shared.ValueBool()
		base.Shared = &shared
	}

	var updateOpts networks.UpdateOptsBuilder = base
	if !plan.External.Equal(state.External) && !plan.External.IsNull() && !plan.External.IsUnknown() {
		ext := plan.External.ValueBool()
		updateOpts = external.UpdateOptsExt{UpdateOptsBuilder: updateOpts, External: &ext}
	}
	if !plan.PortSecurity.Equal(state.PortSecurity) && !plan.PortSecurity.IsNull() && !plan.PortSecurity.IsUnknown() {
		ps := plan.PortSecurity.ValueBool()
		updateOpts = portsecurity.NetworkUpdateOptsExt{UpdateOptsBuilder: updateOpts, PortSecurityEnabled: &ps}
	}
	if !plan.DNSDomain.Equal(state.DNSDomain) && !plan.DNSDomain.IsUnknown() {
		v := plan.DNSDomain.ValueString()
		updateOpts = dns.NetworkUpdateOptsExt{UpdateOptsBuilder: updateOpts, DNSDomain: &v}
	}
	return updateOpts
}

// dnsLabel is neutron-lib's DNS_LABEL_REGEX: it runs after Neutron lower-cases
// the value, so uppercase never reaches it.
var dnsLabel = regexp.MustCompile(`^[a-z0-9-]{1,63}$`)

// invalidDNSDomain reports why s is not an acceptable dns_domain, or "" when
// it is. It applies the rules of neutron-lib's validate_dns_domain, plus one
// of its own: Neutron lower-cases the value before storing it, which Terraform
// would report as "inconsistent result after apply", so mixed case is refused
// here with the value the user should write. A bad value therefore fails at
// plan time with a message that names the attribute, not as a 400 mid-apply.
func invalidDNSDomain(s string) string {
	if s == "" {
		return ""
	}
	if lower := strings.ToLower(s); lower != s {
		return fmt.Sprintf("Neutron stores dns_domain lower-cased; write %q.", lower)
	}
	if !strings.HasSuffix(s, ".") {
		return fmt.Sprintf("%q must be a fully qualified domain name ending in a dot, for example %q.", s, s+".")
	}
	// neutron-lib caps the value two short of the 255-character FQDN size so a
	// record name can still be prefixed.
	if len(s) > 253 {
		return fmt.Sprintf("%q is longer than 253 characters.", s)
	}
	labels := strings.Split(strings.TrimSuffix(s, "."), ".")
	for _, label := range labels {
		switch {
		case label == "":
			return fmt.Sprintf("%q has an empty label.", s)
		case strings.HasPrefix(label, "-") || strings.HasSuffix(label, "-"):
			return fmt.Sprintf("label %q of %q must not start or end with a hyphen.", label, s)
		case !dnsLabel.MatchString(label):
			return fmt.Sprintf("label %q of %q must be 1 to 63 characters, each a lowercase letter, a digit or a hyphen.", label, s)
		}
	}
	if last := labels[len(labels)-1]; len(labels) > 1 && strings.Trim(last, "0123456789") == "" {
		return fmt.Sprintf("the top-level label %q of %q must not be all numeric.", last, s)
	}
	return ""
}

// ValidateConfig rejects a dns_domain Neutron would reject, at plan time.
func (r *networkResource) ValidateConfig(ctx context.Context, req resource.ValidateConfigRequest, resp *resource.ValidateConfigResponse) {
	var cfg networkModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &cfg)...)
	if resp.Diagnostics.HasError() || cfg.DNSDomain.IsNull() || cfg.DNSDomain.IsUnknown() {
		return
	}
	if msg := invalidDNSDomain(cfg.DNSDomain.ValueString()); msg != "" {
		resp.Diagnostics.AddAttributeError(path.Root("dns_domain"), "Invalid dns_domain", msg)
	}
}

func (r *networkResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.config = configureClient(req.ProviderData, &resp.Diagnostics)
}

func (r *networkResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan networkModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := r.config.NetworkV2Client()
	if err != nil {
		resp.Diagnostics.AddError("networking: building v2 client", err.Error())
		return
	}

	createOpts := networkCreateOpts(ctx, &plan, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}

	n, err := networks.Create(ctx, client, createOpts).Extract()
	if err != nil {
		resp.Diagnostics.AddError("networking: creating network", err.Error())
		return
	}

	if !plan.Tags.IsNull() && !plan.Tags.IsUnknown() {
		var tags []string
		resp.Diagnostics.Append(plan.Tags.ElementsAs(ctx, &tags, false)...)
		if resp.Diagnostics.HasError() {
			return
		}
		if err := replaceTags(ctx, client, "networks", n.ID, tags); err != nil {
			resp.Diagnostics.AddError("networking: setting network tags", err.Error())
			return
		}
	}

	_, readDiags := r.readInto(ctx, client, n.ID, &plan)
	resp.Diagnostics.Append(readDiags...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *networkResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state networkModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := r.config.NetworkV2Client()
	if err != nil {
		resp.Diagnostics.AddError("networking: building v2 client", err.Error())
		return
	}

	notFound, diags := r.readInto(ctx, client, state.ID.ValueString(), &state)
	if notFound {
		resp.Diagnostics.AddWarning("Network not found",
			fmt.Sprintf("Network %s no longer exists and was removed from state.", state.ID.ValueString()))
		resp.State.RemoveResource(ctx)
		return
	}
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *networkResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state networkModel
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

	updateOpts := networkUpdateOpts(&plan, &state)

	if _, err := networks.Update(ctx, client, plan.ID.ValueString(), updateOpts).Extract(); err != nil {
		resp.Diagnostics.AddError("networking: updating network", err.Error())
		return
	}

	if !plan.Tags.Equal(state.Tags) {
		var tags []string
		if !plan.Tags.IsNull() && !plan.Tags.IsUnknown() {
			resp.Diagnostics.Append(plan.Tags.ElementsAs(ctx, &tags, false)...)
			if resp.Diagnostics.HasError() {
				return
			}
		}
		if err := replaceTags(ctx, client, "networks", plan.ID.ValueString(), tags); err != nil {
			resp.Diagnostics.AddError("networking: updating network tags", err.Error())
			return
		}
	}

	_, readDiags := r.readInto(ctx, client, plan.ID.ValueString(), &plan)
	resp.Diagnostics.Append(readDiags...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *networkResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state networkModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := r.config.NetworkV2Client()
	if err != nil {
		resp.Diagnostics.AddError("networking: building v2 client", err.Error())
		return
	}

	if err := networks.Delete(ctx, client, state.ID.ValueString()).ExtractErr(); err != nil {
		if gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
			return
		}
		resp.Diagnostics.AddError("networking: deleting network", err.Error())
	}
}

func (r *networkResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, path.Root("id"), req, resp)
}

// readInto fetches the network (base + external ext) and populates the model.
// notFound is true when the network no longer exists (HTTP 404).
func (r *networkResource) readInto(ctx context.Context, client *gophercloud.ServiceClient, id string, m *networkModel) (notFound bool, diags diag.Diagnostics) {
	var n networkExtended
	if err := networks.Get(ctx, client, id).ExtractInto(&n); err != nil {
		if gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
			return true, diags
		}
		diags.AddError("networking: reading network", err.Error())
		return false, diags
	}

	m.ID = types.StringValue(n.ID)
	m.Name = types.StringValue(n.Name)
	m.Description = types.StringValue(n.Description)
	m.AdminStateUp = types.BoolValue(n.AdminStateUp)
	m.Shared = types.BoolValue(n.Shared)
	m.External = types.BoolValue(n.External)
	m.PortSecurity = types.BoolValue(n.PortSecurityEnabled)
	m.TenantID = types.StringValue(n.TenantID)
	m.DNSDomain = types.StringValue(n.DNSDomain)

	tagVals := n.Tags
	if tagVals == nil {
		tagVals = []string{}
	}
	tags, d := types.SetValueFrom(ctx, types.StringType, tagVals)
	diags = append(diags, d...)
	m.Tags = tags

	if m.Region.IsNull() || m.Region.IsUnknown() {
		m.Region = types.StringValue(r.config.Region)
	}
	return false, diags
}
