// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"context"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-framework/resource"

	"github.com/platform9/terraform-provider-pcd/internal/services/blockstorage"
	"github.com/platform9/terraform-provider-pcd/internal/services/compute"
	"github.com/platform9/terraform-provider-pcd/internal/services/dns"
	"github.com/platform9/terraform-provider-pcd/internal/services/identity"
	"github.com/platform9/terraform-provider-pcd/internal/services/images"
	"github.com/platform9/terraform-provider-pcd/internal/services/keymanager"
	"github.com/platform9/terraform-provider-pcd/internal/services/loadbalancer"
	"github.com/platform9/terraform-provider-pcd/internal/services/networking"
	"github.com/platform9/terraform-provider-pcd/internal/services/vpnaas"
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
		networking.NewPortResource,
		networking.NewFloatingIPResource,
		networking.NewFloatingIPAssociateResource,
		networking.NewPortSecgroupAssociateResource,
		networking.NewRouterRouteResource,
		networking.NewSubnetRouteResource,
		networking.NewQoSPolicyResource,
		networking.NewQoSBandwidthLimitRuleResource,
		networking.NewQoSDSCPMarkingRuleResource,
		networking.NewQoSMinimumBandwidthRuleResource,
		networking.NewQuotaResource,
		compute.NewKeypairResource,
		compute.NewInstanceResource,
		compute.NewFlavorResource,
		compute.NewServergroupResource,
		compute.NewInterfaceAttachResource,
		compute.NewVolumeAttachResource,
		compute.NewQuotasetResource,
		blockstorage.NewVolumeResource,
		blockstorage.NewQuotasetResource,
		loadbalancer.NewLoadBalancerResource,
		loadbalancer.NewListenerResource,
		loadbalancer.NewPoolResource,
		loadbalancer.NewMemberResource,
		loadbalancer.NewMonitorResource,
		loadbalancer.NewL7PolicyResource,
		loadbalancer.NewL7RuleResource,
		dns.NewZoneResource,
		dns.NewRecordSetResource,
		keymanager.NewSecretResource,
		keymanager.NewContainerResource,
		vpnaas.NewServiceResource,
		vpnaas.NewIKEPolicyResource,
		vpnaas.NewIPSecPolicyResource,
		vpnaas.NewEndpointGroupResource,
		vpnaas.NewSiteConnectionResource,
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
		networking.NewSubnetIDsDataSource,
		networking.NewSecgroupDataSource,
		networking.NewPortDataSource,
		networking.NewPortIDsDataSource,
		networking.NewRouterDataSource,
		networking.NewFloatingIPDataSource,
		networking.NewQoSPolicyDataSource,
		compute.NewFlavorDataSource,
		compute.NewKeypairDataSource,
		compute.NewAvailabilityZonesDataSource,
		blockstorage.NewVolumeDataSource,
		blockstorage.NewSnapshotDataSource,
		loadbalancer.NewLoadBalancerDataSource,
		dns.NewZoneDataSource,
		keymanager.NewSecretDataSource,
	}
}
