// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

// Package networking implements the pcd_networking_* resources and data sources
// (Neutron v2), ported from terraform-provider-openstack v3.4.0.
package networking

import (
	"context"
	"fmt"
	"hash/fnv"
	"sort"
	"strings"
	"sync"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/extensions/attributestags"
	"github.com/hashicorp/terraform-plugin-framework/diag"

	"github.com/platform9/terraform-provider-pcd/internal/clients"
)

// keyedMutex serializes read-modify-write access per key. The sub-resources that
// edit a shared parent's list attribute (router routes, subnet host routes, a
// port's security groups) each read the current list, splice their entry, and
// write it back; without serialization two such resources applying concurrently
// against the same parent would clobber each other. Callers Lock the parent ID
// for the duration of their read-modify-write.
type keyedMutex struct {
	m sync.Map
}

func (k *keyedMutex) Lock(key string) {
	mu, _ := k.m.LoadOrStore(key, &sync.Mutex{})
	mu.(*sync.Mutex).Lock()
}

func (k *keyedMutex) Unlock(key string) {
	if mu, ok := k.m.Load(key); ok {
		mu.(*sync.Mutex).Unlock()
	}
}

// neutronParentMu guards per-parent (router/subnet/port) list edits.
var neutronParentMu = &keyedMutex{}

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

// sortedIDsHash sorts a list of resource IDs ascending and derives a stable
// synthetic data-source ID from them, so the *_ids data sources produce a
// deterministic ordering and only churn when the matched set changes.
func sortedIDsHash(ids []string) (sorted []string, id string) {
	sorted = append([]string(nil), ids...)
	sort.Strings(sorted)
	h := fnv.New32a()
	_, _ = h.Write([]byte(strings.Join(sorted, ",")))
	return sorted, fmt.Sprintf("%d", h.Sum32())
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
