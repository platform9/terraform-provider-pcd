// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"context"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-framework/resource"

	"github.com/platform9/terraform-provider-pcd/internal/services/identity"
)

// Ensure pcdProvider satisfies the provider.Provider interface.
var _ provider.Provider = (*pcdProvider)(nil)

// pcdProvider is the Terraform provider for Platform9 Private Cloud Director.
// Schema and Configure live in config.go.
type pcdProvider struct {
	// version is the build/release version, surfaced in user-agent strings.
	version string
}

// New returns a provider factory bound to the given build version.
func New(version string) func() provider.Provider {
	return func() provider.Provider {
		return &pcdProvider{version: version}
	}
}

func (p *pcdProvider) Metadata(_ context.Context, _ provider.MetadataRequest, resp *provider.MetadataResponse) {
	resp.TypeName = "pcd"
	resp.Version = p.version
}

func (p *pcdProvider) Resources(_ context.Context) []func() resource.Resource {
	return nil
}

func (p *pcdProvider) DataSources(_ context.Context) []func() datasource.DataSource {
	return []func() datasource.DataSource{
		identity.NewAuthScopeDataSource,
	}
}
