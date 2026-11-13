// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

// Package mors implements the resources for mors, PCD's VM lease service: a
// project's lease policy (pcd_compute_lease_policy) and a VM's own lease
// (pcd_compute_instance_lease). mors is a Platform9 REST API, not an OpenStack
// one, reached through clients.Config.MorsV1Client, which reuses the shared
// authenticated ProviderClient for tokens.
//
// mors answers every successful write with 200, where gophercloud's defaults
// expect 201 or 202, so each call here names its codes. Writes are synchronous
// database updates: nothing here waits.
package mors

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

// getJSON issues an authenticated GET and decodes the 200 response into out.
// mors answers 404 for an object it does not hold; isNotFound recognizes it.
func getJSON(ctx context.Context, client *gophercloud.ServiceClient, url string, out any) error {
	_, err := client.Get(ctx, url, out, &gophercloud.RequestOpts{OkCodes: []int{200}})
	return err
}

// writeJSON sends body with POST (create) or PUT (update). mors answers both
// with 200 and a body of {"success": true}, which is not decoded.
func writeJSON(ctx context.Context, client *gophercloud.ServiceClient, method, url string, body any) error {
	opts := &gophercloud.RequestOpts{OkCodes: []int{200}}
	var err error
	switch method {
	case http.MethodPost:
		_, err = client.Post(ctx, url, body, nil, opts)
	case http.MethodPut:
		_, err = client.Put(ctx, url, body, nil, opts)
	default:
		err = fmt.Errorf("mors: unsupported write method %s", method)
	}
	return err
}

// deleteObject issues an authenticated DELETE. mors answers 200 whether or not
// it held the object; a 404 from anything in between also means there is
// nothing left to delete, which is what destroy wants.
func deleteObject(ctx context.Context, client *gophercloud.ServiceClient, url string) error {
	_, err := client.Delete(ctx, url, &gophercloud.RequestOpts{OkCodes: []int{200, 202, 204}})
	if isNotFound(err) {
		return nil
	}
	return err
}

// isNotFound reports whether err is mors saying it holds no such object.
func isNotFound(err error) bool {
	return gophercloud.ResponseCodeIs(err, http.StatusNotFound)
}

// isConflict reports whether err is mors refusing a create because the
// object already exists.
func isConflict(err error) bool {
	return gophercloud.ResponseCodeIs(err, http.StatusConflict)
}
