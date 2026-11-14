// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package clients

import (
	"fmt"
	"net/url"

	"github.com/gophercloud/gophercloud/v2"
)

// hagridIdentityRegion is the region PCD registers its Keystone under. hagrid,
// PCD's SSO configuration service, is served from the same host as that
// Keystone, and the PCD UI finds it the same way.
const hagridIdentityRegion = "Infra"

// HagridClient returns a client for hagrid, PCD's SSO configuration service.
// hagrid is not in the Keystone catalog: it sits beside Keystone on the same
// host, at /hagrid/, and its ingress routes by host name. The base URL is
// therefore the origin of the catalog's public identity endpoint in the
// `Infra` region, or in the provider's region when the catalog has none in
// `Infra`, plus `/hagrid/`.
//
// The origin of auth_url is deliberately not used: an auth_url that reaches
// Keystone through an IP address or another host name does not reach hagrid.
// An endpoint_overrides entry for "hagrid" names the full base URL instead,
// for example `https://pcd.example.com/hagrid/`.
func (c *Config) HagridClient() (*gophercloud.ServiceClient, error) {
	if override := c.EndpointOverrides["hagrid"]; override != "" {
		return &gophercloud.ServiceClient{ProviderClient: c.Provider, Endpoint: normalizeEndpoint(override), Type: "hagrid"}, nil
	}
	identity, err := c.Provider.EndpointLocator(gophercloud.EndpointOpts{
		Type:         "identity",
		Region:       hagridIdentityRegion,
		Availability: gophercloud.AvailabilityPublic,
	})
	if err != nil {
		identity, err = c.Provider.EndpointLocator(gophercloud.EndpointOpts{
			Type:         "identity",
			Region:       c.Region,
			Availability: gophercloud.AvailabilityPublic,
		})
		if err != nil {
			return nil, fmt.Errorf("pcd: locating the identity endpoint that hagrid is served beside "+
				"(set endpoint_overrides.hagrid to hagrid's URL): %w", err)
		}
	}
	base, err := hagridBaseURL(identity)
	if err != nil {
		return nil, err
	}
	return &gophercloud.ServiceClient{ProviderClient: c.Provider, Endpoint: base, Type: "hagrid"}, nil
}

// hagridBaseURL returns the scheme and host of identityURL plus /hagrid/.
func hagridBaseURL(identityURL string) (string, error) {
	u, err := url.Parse(identityURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "", fmt.Errorf("pcd: the identity endpoint %q is not an absolute URL, so hagrid's URL cannot be "+
			"derived from it (set endpoint_overrides.hagrid)", identityURL)
	}
	return u.Scheme + "://" + u.Host + "/hagrid/", nil
}
