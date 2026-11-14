// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package clients

import (
	"fmt"
	"strings"

	"github.com/gophercloud/gophercloud/v2"
)

// MorsV1Client returns a client for mors, PCD's VM lease service, at its v1
// API. mors is a Platform9 REST service with no gophercloud constructor. Each
// region runs its own, registered in the Keystone catalog as service type
// `mors`, so the client resolves the provider's region.
//
// An endpoint_overrides entry for "mors" replaces the catalog lookup, so a
// deployment whose catalog lacks mors can still name it. The override names
// the service, and the version is applied to it the way the catalog URL gets
// it, so `https://pcd.example.com/mors` and `.../mors/v1` both work.
func (c *Config) MorsV1Client() (*gophercloud.ServiceClient, error) {
	base := c.EndpointOverrides["mors"]
	if base == "" {
		url, err := c.Provider.EndpointLocator(gophercloud.EndpointOpts{
			Type:         "mors",
			Region:       c.Region,
			Availability: gophercloud.AvailabilityPublic,
		})
		if err != nil {
			return nil, fmt.Errorf("pcd: mors, the VM lease service, is not in the service catalog for region %q "+
				"(set endpoint_overrides.mors if it is reachable elsewhere): %w", c.Region, err)
		}
		base = url
	}
	return &gophercloud.ServiceClient{
		ProviderClient: c.Provider,
		Endpoint:       morsVersionedURL(base),
		Type:           "mors",
	}, nil
}

// morsVersionedURL returns base carrying exactly one trailing `v1/`, whether
// base is the bare service root the catalog lists or an override that already
// names the version.
func morsVersionedURL(base string) string {
	b := strings.TrimSuffix(strings.TrimRight(base, "/"), "/v1")
	return b + "/v1/"
}
