// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package clients

import (
	"errors"
	"strings"
	"testing"

	"github.com/gophercloud/gophercloud/v2"
)

// The catalog lists hamgr as https://<region-fqdn>/hamgr/ and its routes are
// under /v1/. An override may name the root or the versioned URL; neither may
// end up with the version twice, or every call 404s.
func TestHamgrVersionedURL(t *testing.T) {
	for _, tc := range []struct {
		name, base, want string
	}{
		{"catalog root", "https://pcd.example.com/hamgr", "https://pcd.example.com/hamgr/v1/"},
		{"trailing slash", "https://pcd.example.com/hamgr/", "https://pcd.example.com/hamgr/v1/"},
		{"override with version", "https://pcd.example.com/hamgr/v1", "https://pcd.example.com/hamgr/v1/"},
		{"override with version and slash", "https://pcd.example.com/hamgr/v1/", "https://pcd.example.com/hamgr/v1/"},
		{"host-only base", "https://pcd.example.com", "https://pcd.example.com/v1/"},
		// Only a whole trailing "v1" segment is a version.
		{"segment that merely ends in v1", "https://pcd.example.com/hamgrv1", "https://pcd.example.com/hamgrv1/v1/"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := hamgrVersionedURL(tc.base); got != tc.want {
				t.Errorf("hamgrVersionedURL(%q)\n got: %s\nwant: %s", tc.base, got, tc.want)
			}
		})
	}
}

// The catalog is the default; an override replaces it even when the catalog
// has no hamgr entry at all.
func TestHamgrV1ClientEndpoint(t *testing.T) {
	var asked gophercloud.EndpointOpts
	cfg := &Config{
		Region: "region-one",
		Provider: &gophercloud.ProviderClient{
			EndpointLocator: func(o gophercloud.EndpointOpts) (string, error) {
				asked = o
				return "https://region-one.example.com/hamgr/", nil
			},
		},
	}
	client, err := cfg.HamgrV1Client()
	if err != nil {
		t.Fatalf("HamgrV1Client: %v", err)
	}
	if client.Endpoint != "https://region-one.example.com/hamgr/v1/" {
		t.Fatalf("endpoint = %s, want the catalog URL plus v1/", client.Endpoint)
	}
	if asked.Type != "hamgr" || asked.Region != "region-one" || asked.Availability != gophercloud.AvailabilityPublic {
		t.Fatalf("catalog lookup = %+v, want type hamgr, the provider's region, public", asked)
	}

	errNoHamgr := errors.New("no hamgr in this catalog")
	cfg.Provider.EndpointLocator = func(gophercloud.EndpointOpts) (string, error) {
		return "", errNoHamgr
	}
	_, err = cfg.HamgrV1Client()
	if err == nil || !strings.Contains(err.Error(), "not in the service catalog") ||
		!strings.Contains(err.Error(), "endpoint_overrides.hamgr") || !errors.Is(err, errNoHamgr) {
		t.Fatalf("catalog miss: err = %v, want one that says hamgr is not in the catalog, names "+
			"endpoint_overrides.hamgr and wraps the catalog's error", err)
	}

	cfg.EndpointOverrides = map[string]string{"hamgr": "https://hamgr.example.com/hamgr"}
	client, err = cfg.HamgrV1Client()
	if err != nil {
		t.Fatalf("HamgrV1Client with an override: %v", err)
	}
	if client.Endpoint != "https://hamgr.example.com/hamgr/v1/" {
		t.Fatalf("endpoint = %s, want the override plus v1/", client.Endpoint)
	}
}

// endpoint_overrides.hamgr replaces the catalog lookup even when the catalog
// lists hamgr: the catalog may name a host Terraform cannot reach.
func TestHamgrV1ClientOverrideWinsOverTheCatalog(t *testing.T) {
	cfg := &Config{
		Region:            "region-one",
		EndpointOverrides: map[string]string{"hamgr": "https://hamgr.example.com/hamgr/v1"},
		Provider: &gophercloud.ProviderClient{
			EndpointLocator: func(o gophercloud.EndpointOpts) (string, error) {
				t.Errorf("the catalog was consulted (%+v) although endpoint_overrides.hamgr is set", o)
				return "https://region-one.example.com/hamgr/", nil
			},
		},
	}
	client, err := cfg.HamgrV1Client()
	if err != nil || client.Endpoint != "https://hamgr.example.com/hamgr/v1/" {
		t.Fatalf("got %v, %v; want the override with exactly one v1/", client, err)
	}
}
