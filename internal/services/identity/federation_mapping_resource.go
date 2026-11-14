// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package identity

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/identity/v3/federation"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/platform9/terraform-provider-pcd/internal/clients"
)

var (
	_ resource.Resource                = (*federationMappingResource)(nil)
	_ resource.ResourceWithConfigure   = (*federationMappingResource)(nil)
	_ resource.ResourceWithImportState = (*federationMappingResource)(nil)
)

// federationNamePattern is Keystone's mapping_id_string, which a protocol's
// mapping_id must match. PCD's identity provider and SSO configuration names
// follow the same rule: the PCD UI replaces every other character with "_".
var federationNamePattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

const federationNameRule = "1 to 64 letters, digits, underscores or hyphens"

// NewFederationMappingResource is the factory registered with the provider.
func NewFederationMappingResource() resource.Resource {
	return &federationMappingResource{}
}

type federationMappingResource struct {
	config *clients.Config
}

type federationMappingModel struct {
	ID    types.String `tfsdk:"id"`
	Name  types.String `tfsdk:"name"`
	Rules types.String `tfsdk:"rules"`
}

// federationMappingOpts sends the rules exactly as configured. gophercloud's
// typed MappingRule covers Keystone's mapping schema 1.0 and would drop any
// key it does not know; the raw JSON keeps the whole mapping language.
type federationMappingOpts struct {
	rules json.RawMessage
}

func (o federationMappingOpts) body() map[string]any {
	return map[string]any{"mapping": map[string]any{"rules": o.rules}}
}

func (o federationMappingOpts) ToMappingCreateMap() (map[string]any, error) { return o.body(), nil }
func (o federationMappingOpts) ToMappingUpdateMap() (map[string]any, error) { return o.body(), nil }

// federationCanonicalJSON re-marshals raw JSON compactly with sorted object
// keys, which is byte for byte what jsonencode() produces. Input that is not
// valid JSON is returned unchanged. resmgr has the same helper for blueprint
// storage backends; this copy keeps that validated package untouched.
func federationCanonicalJSON(raw []byte) string {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return string(raw)
	}
	b, err := json.Marshal(v)
	if err != nil {
		return string(raw)
	}
	return string(b)
}

// federationRulesForState returns what state holds for the rules Keystone
// reports. When the prior value encodes the same JSON, it is kept as written,
// so a hand-formatted value does not plan a change on every refresh; otherwise
// the server's rules, in jsonencode's form, show the drift.
func federationRulesForState(prior types.String, server json.RawMessage) types.String {
	canonical := federationCanonicalJSON(server)
	if !prior.IsNull() && !prior.IsUnknown() && federationCanonicalJSON([]byte(prior.ValueString())) == canonical {
		return prior
	}
	return types.StringValue(canonical)
}

// federationRulesValidator accepts a JSON array with at least one rule, which
// is the least Keystone accepts.
type federationRulesValidator struct{}

func (federationRulesValidator) Description(context.Context) string {
	return "must be a JSON array of at least one mapping rule"
}

func (v federationRulesValidator) MarkdownDescription(ctx context.Context) string {
	return v.Description(ctx)
}

func (federationRulesValidator) ValidateString(_ context.Context, req validator.StringRequest, resp *validator.StringResponse) {
	if req.ConfigValue.IsNull() || req.ConfigValue.IsUnknown() {
		return
	}
	var rules []json.RawMessage
	if err := json.Unmarshal([]byte(req.ConfigValue.ValueString()), &rules); err != nil {
		resp.Diagnostics.AddAttributeError(req.Path, "Invalid mapping rules",
			fmt.Sprintf("rules must be a JSON array of mapping rules; build it with jsonencode([...]): %v", err))
		return
	}
	if len(rules) == 0 {
		resp.Diagnostics.AddAttributeError(req.Path, "Invalid mapping rules",
			"rules must hold at least one rule: Keystone refuses an empty mapping. To remove every rule, destroy the mapping.")
	}
}

func (r *federationMappingResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_identity_federation_mapping"
}

func (r *federationMappingResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Manages a Keystone federation mapping: the rules that turn the attributes a SAML " +
			"identity provider asserts into a local user name, email and group. In PCD single sign-on, one mapping " +
			"holds one rule per SSO group, and the `saml2` protocol of the identity provider points at it " +
			"(`pcd_identity_federation_protocol`); the PCD UI names it `<identity provider, lowercase>_mapping`, " +
			"for example `idp1_mapping`. The SSO guide shows the whole setup.\n\n" +
			"The resource owns the whole rule list: editing the SAML groups in the PCD UI rewrites the same " +
			"mapping, and the next apply puts the configured rules back. Keystone also stores a `schema_version` " +
			"for each mapping, which this resource does not manage: every create and update gets Keystone's " +
			"default. Destroying a mapping that a protocol still names succeeds (Keystone does not check), and " +
			"single sign-on through that protocol then fails until it names another mapping.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "The mapping name.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"name": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "The mapping's ID in Keystone, " + federationNameRule + ": a protocol's " +
					"`mapping_id` can name nothing else. Changing this forces a new resource.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
				Validators:    []validator.String{stringvalidator.RegexMatches(federationNamePattern, federationNameRule)},
			},
			"rules": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "The mapping rules, a JSON array in Keystone's mapping language; build it with " +
					"`jsonencode([...])`. Every key is sent as written. Name groups as " +
					"`pcd_identity_group.<name>.id`, not as literal IDs: the reference makes Terraform destroy the " +
					"mapping before the groups it names. Updated in place.",
				Validators: []validator.String{federationRulesValidator{}},
			},
		},
	}
}

func (r *federationMappingResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.config = configureClient(req.ProviderData, &resp.Diagnostics)
}

func (r *federationMappingResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan federationMappingModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := r.config.IdentityV3Client()
	if err != nil {
		resp.Diagnostics.AddError("identity: building v3 client", err.Error())
		return
	}

	name := plan.Name.ValueString()
	opts := federationMappingOpts{rules: json.RawMessage(plan.Rules.ValueString())}
	if err := federation.CreateMapping(ctx, client, name, opts).Err; err != nil {
		if gophercloud.ResponseCodeIs(err, http.StatusConflict) {
			resp.Diagnostics.AddError("identity: creating federation mapping", fmt.Sprintf(
				"A mapping named %s already exists (the PCD UI creates one when SAML groups are added). Import it "+
					"to manage it here: terraform import <resource address> %s", name, name))
			return
		}
		resp.Diagnostics.AddError("identity: creating federation mapping", err.Error())
		return
	}

	plan.ID = types.StringValue(name)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

// federationMappingRules reads a mapping's rules as raw JSON. A success answer
// without the mapping, or with null rules, is clients.ErrNoObject.
func federationMappingRules(ctx context.Context, client *gophercloud.ServiceClient, name string) (json.RawMessage, error) {
	var out struct {
		Mapping struct {
			Rules json.RawMessage `json:"rules"`
		} `json:"mapping"`
	}
	if err := federation.GetMapping(ctx, client, name).ExtractInto(&out); err != nil {
		return nil, err
	}
	if len(out.Mapping.Rules) == 0 || string(out.Mapping.Rules) == "null" {
		return nil, clients.ErrNoObject
	}
	return out.Mapping.Rules, nil
}

func (r *federationMappingResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state federationMappingModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := r.config.IdentityV3Client()
	if err != nil {
		resp.Diagnostics.AddError("identity: building v3 client", err.Error())
		return
	}

	rules, err := federationMappingRules(ctx, client, state.Name.ValueString())
	if err != nil {
		if gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
			resp.State.RemoveResource(ctx)
			return
		}
		resp.Diagnostics.AddError("identity: reading federation mapping", err.Error())
		return
	}

	state.ID = state.Name
	state.Rules = federationRulesForState(state.Rules, rules)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *federationMappingResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan federationMappingModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := r.config.IdentityV3Client()
	if err != nil {
		resp.Diagnostics.AddError("identity: building v3 client", err.Error())
		return
	}

	opts := federationMappingOpts{rules: json.RawMessage(plan.Rules.ValueString())}
	if err := federation.UpdateMapping(ctx, client, plan.Name.ValueString(), opts).Err; err != nil {
		resp.Diagnostics.AddError("identity: updating federation mapping", err.Error())
		return
	}

	plan.ID = plan.Name
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *federationMappingResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state federationMappingModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := r.config.IdentityV3Client()
	if err != nil {
		resp.Diagnostics.AddError("identity: building v3 client", err.Error())
		return
	}

	if err := federation.DeleteMapping(ctx, client, state.Name.ValueString()).ExtractErr(); err != nil {
		if gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
			return
		}
		resp.Diagnostics.AddError("identity: deleting federation mapping", err.Error())
	}
}

// ImportState takes the mapping name. An empty ID names no mapping, so it is
// refused before anything is read.
func (r *federationMappingResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	if req.ID == "" {
		resp.Diagnostics.AddError("Import needs a federation mapping ID",
			"Pass the name of the mapping to import, which `pcdctl mapping list` shows.")
		return
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), req.ID)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("name"), req.ID)...)
}
