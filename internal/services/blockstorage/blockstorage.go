// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

// Package blockstorage implements the pcd_blockstorage_* resources and data
// sources (Cinder v3), ported from terraform-provider-openstack v3.4.0.
package blockstorage

import (
	"fmt"

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
