// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package identity

import (
	"context"
	"fmt"

	"github.com/gophercloud/gophercloud/v2/openstack/identity/v3/domains"
	"github.com/hashicorp/terraform-plugin-framework-validators/datasourcevalidator"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/platform9/terraform-provider-pcd/internal/clients"
)

var (
	_ datasource.DataSource                     = (*domainDataSource)(nil)
	_ datasource.DataSourceWithConfigure        = (*domainDataSource)(nil)
	_ datasource.DataSourceWithConfigValidators = (*domainDataSource)(nil)
)

// NewDomainDataSource is the factory registered with the provider.
func NewDomainDataSource() datasource.DataSource {
	return &domainDataSource{}
}

type domainDataSource struct {
	config *clients.Config
}

type domainDataSourceModel struct {
	ID          types.String `tfsdk:"id"`
	DomainID    types.String `tfsdk:"domain_id"`
	Name        types.String `tfsdk:"name"`
	Description types.String `tfsdk:"description"`
	Enabled     types.Bool   `tfsdk:"enabled"`
	Region      types.String `tfsdk:"region"`
}

func (d *domainDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_identity_domain"
}

func (d *domainDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Look up a domain in PCD's Keystone identity service by ID or name, for example to " +
			"reference the `Default` domain or a domain the PCD UI created. `pcd_identity_project` cannot find " +
			"domains: Keystone leaves them out of the project list.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{Computed: true, MarkdownDescription: "The domain ID."},
			"domain_id": schema.StringAttribute{
				Optional:            true,
				Computed:            true,
				MarkdownDescription: "Look up the domain by ID (takes precedence over `name`).",
				Validators:          []validator.String{stringvalidator.LengthAtLeast(1)},
			},
			"name": schema.StringAttribute{
				Optional:            true,
				Computed:            true,
				MarkdownDescription: "Look up the domain by its exact name.",
				Validators:          []validator.String{stringvalidator.LengthAtLeast(1)},
			},
			"description": schema.StringAttribute{Computed: true, MarkdownDescription: "The domain description."},
			"enabled":     schema.BoolAttribute{Computed: true, MarkdownDescription: "Whether the domain is enabled."},
			"region":      schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "The region whose identity endpoint is used; defaults to the provider's region."},
		},
	}
}

func (d *domainDataSource) ConfigValidators(_ context.Context) []datasource.ConfigValidator {
	return []datasource.ConfigValidator{
		datasourcevalidator.AtLeastOneOf(path.MatchRoot("domain_id"), path.MatchRoot("name")),
	}
}

func (d *domainDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	d.config = configureClient(req.ProviderData, &resp.Diagnostics)
}

func (d *domainDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var data domainDataSourceModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := d.config.ForRegion(data.Region.ValueString()).IdentityV3Client()
	if err != nil {
		resp.Diagnostics.AddError("identity: building v3 client", err.Error())
		return
	}

	var domain *domains.Domain
	if v := data.DomainID.ValueString(); v != "" {
		domain, err = domains.Get(ctx, client, v).Extract()
		if err != nil {
			resp.Diagnostics.AddError("identity: getting domain by id", err.Error())
			return
		}
		// A 200 without the object decodes to nil.
		if domain == nil {
			resp.Diagnostics.AddError("identity: getting domain by id",
				fmt.Sprintf("The Identity API answered without a domain object for ID %q.", v))
			return
		}
	} else {
		pages, err := domains.List(client, domains.ListOpts{Name: data.Name.ValueString()}).AllPages(ctx)
		if err != nil {
			resp.Diagnostics.AddError("identity: listing domains", err.Error())
			return
		}
		all, err := domains.ExtractDomains(pages)
		if err != nil {
			resp.Diagnostics.AddError("identity: extracting domains", err.Error())
			return
		}
		domain, err = domainByName(all, data.Name.ValueString())
		if err != nil {
			resp.Diagnostics.AddAttributeError(path.Root("name"), "Domain not found", err.Error())
			return
		}
	}

	data.ID = types.StringValue(domain.ID)
	data.DomainID = types.StringValue(domain.ID)
	data.Name = types.StringValue(domain.Name)
	data.Description = types.StringValue(domain.Description)
	data.Enabled = types.BoolValue(domain.Enabled)
	if data.Region.IsNull() || data.Region.IsUnknown() {
		data.Region = types.StringValue(d.config.Region)
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}

// domainByName returns the one domain whose name is exactly name. Keystone
// filters the list by name already and domain names are unique, so anything
// other than one exact match means the domain does not exist.
func domainByName(all []domains.Domain, name string) (*domains.Domain, error) {
	for i := range all {
		if all[i].Name == name {
			return &all[i], nil
		}
	}
	return nil, fmt.Errorf("no domain is named %q", name)
}
