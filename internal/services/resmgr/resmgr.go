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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

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

// errAbsent reports a resource resmgr answered for without describing: 200 with a
// body of literal null. isNotFound treats it exactly like a 404.
var errAbsent = errors.New("resmgr: 200 with a null body — the object does not exist")

// isNullJSON reports whether a response body carries no object at all.
func isNullJSON(raw []byte) bool {
	t := bytes.TrimSpace(raw)
	return len(t) == 0 || bytes.Equal(t, []byte("null"))
}

// getJSON issues an authenticated GET and decodes the JSON response into out.
//
// resmgr does not 404 for an object that is gone: `GET /resmgr/v2/clusters/<name>`
// answers 200 with a body of `null` for a cluster that never existed or has been
// deleted (observed on 2026.4). Decoded straight into a struct that is a nil
// unmarshal — no error, every field left at its zero value — so a Read would
// report the resource as present-but-blank, never call RemoveResource, and leave
// Terraform believing a destroyed object still exists. A region could then never
// be rebuilt: the next apply plans no create for something the API says is gone.
//
// The body is therefore read first and an empty one reported as absence. Every
// caller fetches a single object by name or id, so no list response reaches here.
func getJSON(ctx context.Context, client *gophercloud.ServiceClient, url string, out any) error {
	var raw json.RawMessage
	if _, err := client.Get(ctx, url, &raw, &gophercloud.RequestOpts{OkCodes: []int{200}}); err != nil {
		// A 200 carrying no body at all describes no object either, and the decoder
		// reports that as io.EOF before the body ever reaches isNullJSON.
		if errors.Is(err, io.EOF) {
			return errAbsent
		}
		return err
	}
	if isNullJSON(raw) {
		return errAbsent
	}
	return json.Unmarshal(raw, out)
}

// getJSONList issues an authenticated GET for a collection. Unlike getJSON, a null or
// empty body is an empty collection rather than an absence: the distinction getJSON
// draws only makes sense for a request naming one object.
func getJSONList(ctx context.Context, client *gophercloud.ServiceClient, url string, out any) error {
	var raw json.RawMessage
	if _, err := client.Get(ctx, url, &raw, &gophercloud.RequestOpts{OkCodes: []int{200}}); err != nil {
		if errors.Is(err, io.EOF) {
			return nil
		}
		return err
	}
	if isNullJSON(raw) {
		return nil
	}
	return json.Unmarshal(raw, out)
}

// hostsAssignedTo returns the ids of the hosts resmgr still reports as carrying the
// given host configuration. The host *list* is the authority: the per-host endpoints
// answer 404 for minutes after a host is deauthorised, while the list keeps reporting
// the host and its hostconfig_id throughout.
func hostsAssignedTo(ctx context.Context, client *gophercloud.ServiceClient, hostConfigID string) ([]string, error) {
	var hosts []hostAPI
	if err := getJSONList(ctx, client, client.ServiceURL("hosts"), &hosts); err != nil {
		return nil, err
	}
	var assigned []string
	for _, h := range hosts {
		if h.HostConfigID == hostConfigID {
			assigned = append(assigned, h.ID)
		}
	}
	return assigned, nil
}

// unassignPollInterval / unassignPollTimeout bound the wait for an unassign to show up
// in the host list. Short: this confirms a write resmgr has already accepted.
// var, not const, so a test can drive the clock instead of sleeping through it.
var (
	unassignPollInterval = 5 * time.Second
	unassignPollTimeout  = 60 * time.Second
)

// waitUnassigned blocks until the host stops reporting the host configuration.
//
// resmgr answers the unassign 204 whether or not it unbound anything, and 404 while a
// freshly deauthorised host is not describable, so the status code proves nothing. A
// binding Terraform believes is gone while resmgr still holds it is what later strands
// the host: the next apply cannot re-create the assignment (409) and deleting the host
// configuration in that state makes it permanent.
func waitUnassigned(ctx context.Context, client *gophercloud.ServiceClient, hostID, hostConfigID string) error {
	deadline := time.Now().Add(unassignPollTimeout)
	for {
		assigned, err := hostsAssignedTo(ctx, client, hostConfigID)
		if err != nil {
			return fmt.Errorf("checking whether host %s still carries host config %s: %w", hostID, hostConfigID, err)
		}
		still := false
		for _, h := range assigned {
			if h == hostID {
				still = true
				break
			}
		}
		if !still {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("host %s still reports host config %s %s after the unassign was accepted; "+
				"resmgr did not apply it, and deleting the host config while this stands would leave the "+
				"host unable to be assigned one again", hostID, hostConfigID, unassignPollTimeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(unassignPollInterval):
		}
	}
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

// isNotFound reports whether err says the object is not there — either an HTTP 404
// or resmgr's 200-with-a-null-body way of saying the same thing.
func isNotFound(err error) bool {
	return errors.Is(err, errAbsent) || gophercloud.ResponseCodeIs(err, http.StatusNotFound)
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
