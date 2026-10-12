// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

// Package blockstorage implements the pcd_blockstorage_* resources and data
// sources (Cinder v3), ported from terraform-provider-openstack v3.4.0.
package blockstorage

import (
	"fmt"

	"github.com/gophercloud/gophercloud/v2"
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

// blockstorageMicroversionTransferResponse is the volume API version the
// transfer resource speaks: from 3.57 a transfer reports source_project_id,
// the project the volume belonged to when it was offered.
const blockstorageMicroversionTransferResponse = "3.57"

// withMicroversion returns a shallow copy of client pinned to mv, so one call
// speaks a newer volume API version without raising it for the whole client.
func withMicroversion(client *gophercloud.ServiceClient, mv string) *gophercloud.ServiceClient {
	c := *client
	c.Microversion = mv
	return &c
}
