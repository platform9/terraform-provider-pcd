// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package clients

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/gophercloud/gophercloud/v2"
)

// regionalConfig is a Config for region-one whose catalog has a network
// endpoint in region-one and region-two, each at its own URL.
func regionalConfig() *Config {
	return &Config{
		Region: "region-one",
		Provider: &gophercloud.ProviderClient{
			HTTPClient: http.Client{Transport: &http.Transport{}},
			EndpointLocator: func(opts gophercloud.EndpointOpts) (string, error) {
				switch opts.Region {
				case "region-one":
					return "https://one.example.test/network/", nil
				case "region-two":
					return "https://two.example.test/network/", nil
				}
				return "", fmt.Errorf("no %s endpoint in region %q", opts.Type, opts.Region)
			},
		},
	}
}

// A resource that names a region must reach that region's endpoint, not the
// provider's.
func TestForRegionReachesTheRegionsEndpoint(t *testing.T) {
	t.Parallel()
	client, err := regionalConfig().ForRegion("region-two").NetworkV2Client()
	if err != nil {
		t.Fatalf("building the client: %v", err)
	}
	if client.Endpoint != "https://two.example.test/network/" {
		t.Fatalf("endpoint = %s, want region-two's", client.Endpoint)
	}
}

// An empty region, which a resource that leaves region unset passes, and the
// provider's own region both keep the provider's configuration.
func TestForRegionKeepsTheProvidersRegion(t *testing.T) {
	t.Parallel()
	c := regionalConfig()
	for _, region := range []string{"", "region-one"} {
		if got := c.ForRegion(region); got != c {
			t.Errorf("ForRegion(%q) returned a new configuration, want the provider's", region)
		}
	}
	if c.ForRegion("region-two").Region != "region-two" || c.Region != "region-one" {
		t.Fatal("ForRegion changed the provider's own configuration")
	}
}

// A region without the service in its catalog is an error, not a silent fall
// back to the provider's region.
func TestForRegionFailsForARegionWithoutTheService(t *testing.T) {
	t.Parallel()
	if _, err := regionalConfig().ForRegion("region-three").NetworkV2Client(); err == nil {
		t.Fatal("built a client for a region with no network endpoint")
	}
}

// endpoint_overrides pins a service to one URL, which belongs to the
// provider's region. Another region's resource must use its own catalog
// endpoint instead of the provider region's override.
func TestForRegionDoesNotApplyTheProvidersOverrides(t *testing.T) {
	t.Parallel()
	c := regionalConfig()
	c.EndpointOverrides = map[string]string{"network": "https://override.example.test/network/"}

	own, err := c.NetworkV2Client()
	if err != nil {
		t.Fatalf("building the provider-region client: %v", err)
	}
	if own.Endpoint != "https://override.example.test/network/" {
		t.Fatalf("provider-region endpoint = %s, want the override", own.Endpoint)
	}
	other, err := c.ForRegion("region-two").NetworkV2Client()
	if err != nil {
		t.Fatalf("building the region-two client: %v", err)
	}
	if other.Endpoint != "https://two.example.test/network/" {
		t.Fatalf("region-two endpoint = %s, want region-two's catalog endpoint", other.Endpoint)
	}
}
