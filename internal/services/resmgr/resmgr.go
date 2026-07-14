// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

// Package resmgr implements the PCD resource-manager (resmgr) resources and data
// sources: cluster blueprints, host configurations, and host role / host-config
// assignment. resmgr is a Platform9-specific REST API (not OpenStack), reached
// through clients.Config.ResmgrV2Client(), which resolves the `resmgr` catalog
// endpoint and reuses the shared authenticated ProviderClient for tokens.
package resmgr

import (
	"context"
	"fmt"
	"net/http"

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

// getJSON issues an authenticated GET and decodes the JSON response into out.
func getJSON(ctx context.Context, client *gophercloud.ServiceClient, url string, out any) error {
	_, err := client.Get(ctx, url, out, &gophercloud.RequestOpts{OkCodes: []int{200}})
	return err
}

// isNotFound reports whether err is an HTTP 404.
func isNotFound(err error) bool {
	return gophercloud.ResponseCodeIs(err, http.StatusNotFound)
}
