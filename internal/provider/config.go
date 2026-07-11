// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"context"
	"os"
	"strconv"

	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-framework/provider/schema"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/platform9/terraform-provider-pcd/internal/clients"
)

// pcdProviderModel mirrors the provider configuration schema. Attribute names
// deliberately match terraform-provider-openstack so migrating a config is
// mechanical.
type pcdProviderModel struct {
	AuthURL           types.String `tfsdk:"auth_url"`
	Region            types.String `tfsdk:"region"`
	UserName          types.String `tfsdk:"user_name"`
	UserID            types.String `tfsdk:"user_id"`
	Password          types.String `tfsdk:"password"`
	TenantName        types.String `tfsdk:"tenant_name"`
	TenantID          types.String `tfsdk:"tenant_id"`
	UserDomainID      types.String `tfsdk:"user_domain_id"`
	UserDomainName    types.String `tfsdk:"user_domain_name"`
	ProjectDomainID   types.String `tfsdk:"project_domain_id"`
	ProjectDomainName types.String `tfsdk:"project_domain_name"`
	Token             types.String `tfsdk:"token"`
	AppCredID         types.String `tfsdk:"application_credential_id"`
	AppCredName       types.String `tfsdk:"application_credential_name"`
	AppCredSecret     types.String `tfsdk:"application_credential_secret"`
	Insecure          types.Bool   `tfsdk:"insecure"`
	CACertFile        types.String `tfsdk:"cacert_file"`
	Cert              types.String `tfsdk:"cert"`
	Key               types.String `tfsdk:"key"`
	Cloud             types.String `tfsdk:"cloud"`
	EndpointOverrides types.Map    `tfsdk:"endpoint_overrides"`
	MaxRetries        types.Int64  `tfsdk:"max_retries"`
	AllowReauth       types.Bool   `tfsdk:"allow_reauth"`
}

func (p *pcdProvider) Schema(_ context.Context, _ provider.SchemaRequest, resp *provider.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Interact with Platform9 Private Cloud Director (PCD). The provider " +
			"authenticates to Keystone v3 and talks to the OpenStack services PCD exposes " +
			"(Nova, Neutron, Cinder, Glance, Keystone) plus PCD-specific services (resmgr, hamgr, mors).",
		Attributes: map[string]schema.Attribute{
			"auth_url": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "Keystone v3 auth URL, e.g. `https://pcd.example.com/keystone/v3`. Falls back to `OS_AUTH_URL`.",
			},
			"region": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "Region to operate in (e.g. `Infra` on CE). Falls back to `OS_REGION_NAME`.",
			},
			"user_name": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "Username for password auth. Falls back to `OS_USERNAME`.",
			},
			"user_id": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "User ID for password auth (alternative to `user_name`).",
			},
			"password": schema.StringAttribute{
				Optional:            true,
				Sensitive:           true,
				MarkdownDescription: "Password for password auth. Falls back to `OS_PASSWORD`.",
			},
			"tenant_name": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "Project (tenant) name to scope to. Falls back to `OS_PROJECT_NAME`/`OS_TENANT_NAME`.",
			},
			"tenant_id": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "Project (tenant) ID to scope to. Falls back to `OS_PROJECT_ID`/`OS_TENANT_ID`.",
			},
			"user_domain_id": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "Domain ID that the user belongs to. Falls back to `OS_USER_DOMAIN_ID`.",
			},
			"user_domain_name": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "Domain name that the user belongs to. Falls back to `OS_USER_DOMAIN_NAME`.",
			},
			"project_domain_id": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "Domain ID of the scoped project. Falls back to `OS_PROJECT_DOMAIN_ID`.",
			},
			"project_domain_name": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "Domain name of the scoped project. Falls back to `OS_PROJECT_DOMAIN_NAME`.",
			},
			"token": schema.StringAttribute{
				Optional:            true,
				Sensitive:           true,
				MarkdownDescription: "Pre-issued Keystone token for token auth. Falls back to `OS_TOKEN`/`OS_AUTH_TOKEN`.",
			},
			"application_credential_id": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "Application credential ID. Falls back to `OS_APPLICATION_CREDENTIAL_ID`.",
			},
			"application_credential_name": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "Application credential name. Falls back to `OS_APPLICATION_CREDENTIAL_NAME`.",
			},
			"application_credential_secret": schema.StringAttribute{
				Optional:            true,
				Sensitive:           true,
				MarkdownDescription: "Application credential secret. Falls back to `OS_APPLICATION_CREDENTIAL_SECRET`.",
			},
			"insecure": schema.BoolAttribute{
				Optional:            true,
				MarkdownDescription: "Skip TLS certificate verification (required for CE's self-signed cert). Falls back to `OS_INSECURE`.",
			},
			"cacert_file": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "Path to a custom CA certificate bundle (PEM). Falls back to `OS_CACERT`.",
			},
			"cert": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "Path to a client certificate (PEM) for mutual TLS. Falls back to `OS_CERT`.",
			},
			"key": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "Path to the client private key (PEM) for mutual TLS. Falls back to `OS_KEY`.",
			},
			"cloud": schema.StringAttribute{
				Optional: true,
				MarkdownDescription: "Name of a `clouds.yaml` entry to source configuration from. Falls back to `OS_CLOUD`. " +
					"**Not yet implemented in this pre-release** — use explicit `auth_url`/credentials for now.",
			},
			"endpoint_overrides": schema.MapAttribute{
				Optional:            true,
				ElementType:         types.StringType,
				MarkdownDescription: "Map of Keystone service type to endpoint URL, overriding the catalog (escape hatch for labs).",
			},
			"max_retries": schema.Int64Attribute{
				Optional:            true,
				MarkdownDescription: "Number of times to retry on transient (429/5xx) errors.",
			},
			"allow_reauth": schema.BoolAttribute{
				Optional:            true,
				MarkdownDescription: "Re-authenticate automatically when a token expires mid-operation. Defaults to true.",
			},
		},
	}
}

func (p *pcdProvider) Configure(ctx context.Context, req provider.ConfigureRequest, resp *provider.ConfigureResponse) {
	var m pcdProviderModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() {
		return
	}

	// clouds.yaml support is declared in the schema for config parity but not yet
	// wired. Fail loudly rather than silently ignore a user who sets it.
	if strval(m.Cloud, "OS_CLOUD") != "" {
		resp.Diagnostics.AddAttributeError(
			path.Root("cloud"),
			"clouds.yaml (cloud) not yet supported",
			"This pre-release does not yet source configuration from clouds.yaml. "+
				"Configure auth_url and credentials explicitly (or via OS_* env vars).",
		)
		return
	}

	cfg := &clients.Config{
		AuthURL:           strval(m.AuthURL, "OS_AUTH_URL"),
		Region:            strval(m.Region, "OS_REGION_NAME"),
		Username:          strval(m.UserName, "OS_USERNAME"),
		UserID:            strval(m.UserID, "OS_USER_ID"),
		Password:          strval(m.Password, "OS_PASSWORD"),
		TenantName:        strval(m.TenantName, "OS_PROJECT_NAME", "OS_TENANT_NAME"),
		TenantID:          strval(m.TenantID, "OS_PROJECT_ID", "OS_TENANT_ID"),
		UserDomainID:      strval(m.UserDomainID, "OS_USER_DOMAIN_ID"),
		UserDomainName:    strval(m.UserDomainName, "OS_USER_DOMAIN_NAME"),
		ProjectDomainID:   strval(m.ProjectDomainID, "OS_PROJECT_DOMAIN_ID"),
		ProjectDomainName: strval(m.ProjectDomainName, "OS_PROJECT_DOMAIN_NAME"),
		Token:             strval(m.Token, "OS_TOKEN", "OS_AUTH_TOKEN"),
		AppCredID:         strval(m.AppCredID, "OS_APPLICATION_CREDENTIAL_ID"),
		AppCredName:       strval(m.AppCredName, "OS_APPLICATION_CREDENTIAL_NAME"),
		AppCredSecret:     strval(m.AppCredSecret, "OS_APPLICATION_CREDENTIAL_SECRET"),
		Insecure:          boolval(m.Insecure, false, "OS_INSECURE"),
		CACertFile:        strval(m.CACertFile, "OS_CACERT"),
		ClientCertFile:    strval(m.Cert, "OS_CERT"),
		ClientKeyFile:     strval(m.Key, "OS_KEY"),
		AllowReauth:       boolval(m.AllowReauth, true),
		MaxRetries:        int(intval(m.MaxRetries, 0)),
	}

	if !m.EndpointOverrides.IsNull() && !m.EndpointOverrides.IsUnknown() {
		overrides := make(map[string]string, len(m.EndpointOverrides.Elements()))
		resp.Diagnostics.Append(m.EndpointOverrides.ElementsAs(ctx, &overrides, false)...)
		if resp.Diagnostics.HasError() {
			return
		}
		cfg.EndpointOverrides = overrides
	}

	if cfg.AuthURL == "" {
		resp.Diagnostics.AddAttributeError(
			path.Root("auth_url"),
			"Missing auth_url",
			"auth_url must be set (or OS_AUTH_URL exported) to reach the PCD Keystone endpoint.",
		)
		return
	}

	if err := cfg.Authenticate(ctx); err != nil {
		resp.Diagnostics.AddError("Authentication to PCD failed", err.Error())
		return
	}

	// Share the authenticated config with every resource and data source.
	resp.DataSourceData = cfg
	resp.ResourceData = cfg
}

// strval returns the configured value if set, otherwise the first non-empty env var.
func strval(v types.String, envVars ...string) string {
	if !v.IsNull() && !v.IsUnknown() {
		return v.ValueString()
	}
	for _, e := range envVars {
		if x := os.Getenv(e); x != "" {
			return x
		}
	}
	return ""
}

// boolval returns the configured value if set, otherwise the first parseable env
// var, otherwise def.
func boolval(v types.Bool, def bool, envVars ...string) bool {
	if !v.IsNull() && !v.IsUnknown() {
		return v.ValueBool()
	}
	for _, e := range envVars {
		if x := os.Getenv(e); x != "" {
			if b, err := strconv.ParseBool(x); err == nil {
				return b
			}
		}
	}
	return def
}

// intval returns the configured value if set, otherwise def.
func intval(v types.Int64, def int64) int64 {
	if !v.IsNull() && !v.IsUnknown() {
		return v.ValueInt64()
	}
	return def
}
