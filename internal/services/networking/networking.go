// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

// Package networking implements the pcd_networking_* resources and data sources
// (Neutron v2), ported from terraform-provider-openstack v3.4.0.
package networking

import (
	"context"
	"fmt"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/extensions/attributestags"
	"github.com/hashicorp/terraform-plugin-framework/diag"

	"github.com/platform9/terraform-provider-pcd/internal/clients"
)

// configureClient extracts the shared *clients.Config from ProviderData.
func configureClient(providerData any, diags *diag.Diagnostics) *clients.Config {
	if providerData == nil {
		return nil
	}
	config, ok := providerData.(*clients.Config)
	if !ok {
		diags.AddError(
			"Unexpected provider data type",
			fmt.Sprintf("Expected *clients.Config, got %T. This is a bug in the provider.", providerData),
		)
		return nil
	}
	return config
}

// replaceTags sets the full tag list on a Neutron resource (networks, subnets,
// ports, ...) via the standard attributes-tags extension.
func replaceTags(ctx context.Context, client *gophercloud.ServiceClient, resourceType, id string, tags []string) error {
	if tags == nil {
		tags = []string{}
	}
	_, err := attributestags.ReplaceAll(ctx, client, resourceType, id, attributestags.ReplaceAllOpts{Tags: tags}).Extract()
	return err
}
