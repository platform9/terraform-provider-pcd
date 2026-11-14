// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package clients

import (
	"fmt"
	"strings"

	"github.com/gophercloud/gophercloud/v2"
)

// HamgrV1Client returns a client for hamgr, PCD's VM high-availability
// manager, at its v1 API. hamgr is a Platform9 REST service with no
// gophercloud constructor. Each region runs its own, registered in the
// Keystone catalog as service type `hamgr`, so the client resolves the
// provider's region.
//
// An endpoint_overrides entry for "hamgr" replaces the catalog lookup, so a
// deployment whose catalog lacks hamgr can still name it. The override names
// the service, and the version is applied to it the way the catalog URL gets
// it, so `https://pcd.example.com/hamgr` and `.../hamgr/v1` both work.
func (c *Config) HamgrV1Client() (*gophercloud.ServiceClient, error) {
	base := c.EndpointOverrides["hamgr"]
	if base == "" {
		url, err := c.Provider.EndpointLocator(gophercloud.EndpointOpts{
			Type:         "hamgr",
			Region:       c.Region,
			Availability: gophercloud.AvailabilityPublic,
		})
		if err != nil {
			return nil, fmt.Errorf("pcd: hamgr, the VM high-availability service, is not in the service catalog for "+
				"region %q (set endpoint_overrides.hamgr if it is reachable elsewhere): %w", c.Region, err)
		}
		base = url
	}
	return &gophercloud.ServiceClient{
		ProviderClient: c.Provider,
		Endpoint:       hamgrVersionedURL(base),
		Type:           "hamgr",
	}, nil
}

// hamgrVersionedURL returns base carrying exactly one trailing `v1/`, whether
// base is the bare service root the catalog lists or an override that already
// names the version.
func hamgrVersionedURL(base string) string {
	b := strings.TrimSuffix(strings.TrimRight(base, "/"), "/v1")
	return b + "/v1/"
}
