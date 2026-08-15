// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

// Package resmgr implements the PCD resource-manager (resmgr) resources and data
// sources: cluster blueprints, host configurations, and host role / host-config
// assignment. resmgr is a Platform9-specific REST API (not OpenStack), reached
// through the clients.Config resmgr constructors, which resolve the `resmgr`
// catalog endpoint and reuse the shared authenticated ProviderClient for tokens.
//
// The API version differs by resource and the two are not interchangeable:
//
//	ResmgrV2Client  blueprints, host configs, host-config assignment
//	ResmgrV1Client  host roles — v2 exposes no writable roles sub-resource and
//	                reports mapped "uber-roles" on read, so both assignment and
//	                verification must go through v1.
package resmgr

import (
	"context"
	"encoding/json"
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

// postJSON issues an authenticated POST with a JSON body, optionally decoding
// the response into out (pass nil to ignore the body).
func postJSON(ctx context.Context, client *gophercloud.ServiceClient, url string, body, out any) error {
	_, err := client.Post(ctx, url, body, out, &gophercloud.RequestOpts{OkCodes: []int{200, 201, 202}})
	return err
}

// putJSON issues an authenticated PUT with a JSON body, optionally decoding the
// response into out (pass nil to ignore the body).
func putJSON(ctx context.Context, client *gophercloud.ServiceClient, url string, body, out any) error {
	_, err := client.Put(ctx, url, body, out, &gophercloud.RequestOpts{OkCodes: []int{200, 201, 202, 204}})
	return err
}

// isNotFound reports whether err is an HTTP 404.
func isNotFound(err error) bool {
	return gophercloud.ResponseCodeIs(err, http.StatusNotFound)
}

// canonicalJSON re-marshals raw JSON into Go's canonical encoding: compact,
// with object keys sorted. That is byte-for-byte what Terraform's jsonencode()
// produces, so a value read back from the API compares equal to the configured
// one.
//
// Without this, a JSON blob stored as an opaque string diffs on every plan:
// resmgr echoes storage backends with its own spacing and in insertion order
// (`{"a": 1, "c": 2, "b": 3}`) while jsonencode() emits compact and sorted
// (`{"a":1,"b":3,"c":2}`). The two are semantically identical and textually
// different, which Terraform reports as a perpetual in-place update.
//
// A configured value that is valid JSON but not canonical (hand-written with
// custom spacing rather than built by jsonencode) still diffs; jsonencode is
// the documented pattern. Input that is not valid JSON is passed through
// unchanged rather than dropped.
func canonicalJSON(raw json.RawMessage) string {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return string(raw)
	}
	b, err := json.Marshal(v)
	if err != nil {
		return string(raw)
	}
	return string(b)
}
