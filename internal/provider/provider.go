// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"context"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-framework/resource"

	"github.com/platform9/terraform-provider-pcd/internal/services/compute"
	"github.com/platform9/terraform-provider-pcd/internal/services/identity"
	"github.com/platform9/terraform-provider-pcd/internal/services/images"
	"github.com/platform9/terraform-provider-pcd/internal/services/networking"
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
	return []func() resource.Resource{
		identity.NewProjectResource,
		identity.NewRoleResource,
		identity.NewUserResource,
		identity.NewRoleAssignmentResource,
		identity.NewApplicationCredentialResource,
		images.NewImageResource,
		networking.NewNetworkResource,
		networking.NewSubnetResource,
		networking.NewSecgroupResource,
		networking.NewSecgroupRuleResource,
		networking.NewRouterResource,
		networking.NewRouterInterfaceResource,
		compute.NewKeypairResource,
		compute.NewInstanceResource,
		compute.NewFlavorResource,
		compute.NewServergroupResource,
	}
}

func (p *pcdProvider) DataSources(_ context.Context) []func() datasource.DataSource {
	return []func() datasource.DataSource{
		identity.NewAuthScopeDataSource,
		identity.NewProjectDataSource,
		identity.NewUserDataSource,
		identity.NewRoleDataSource,
		images.NewImageDataSource,
		images.NewImageIDsDataSource,
		networking.NewNetworkDataSource,
		networking.NewSubnetDataSource,
		networking.NewSecgroupDataSource,
		compute.NewFlavorDataSource,
		compute.NewKeypairDataSource,
		compute.NewAvailabilityZonesDataSource,
	}
}
