// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package clients

import (
	"errors"
	"strings"
	"testing"

	"github.com/gophercloud/gophercloud/v2"
)

// The catalog lists mors as https://<region-fqdn>/mors and every route is
// under /v1/. An override may name the root or the versioned URL; neither may
// end up with the version twice, or every call 404s.
func TestMorsVersionedURL(t *testing.T) {
	for _, tc := range []struct {
		name, base, want string
	}{
		{"catalog root", "https://pcd.example.com/mors", "https://pcd.example.com/mors/v1/"},
		{"trailing slash", "https://pcd.example.com/mors/", "https://pcd.example.com/mors/v1/"},
		{"override with version", "https://pcd.example.com/mors/v1", "https://pcd.example.com/mors/v1/"},
		{"override with version and slash", "https://pcd.example.com/mors/v1/", "https://pcd.example.com/mors/v1/"},
		{"host-only base", "https://pcd.example.com", "https://pcd.example.com/v1/"},
		// Only a whole trailing "v1" segment is a version.
		{"segment that merely ends in v1", "https://pcd.example.com/morsv1", "https://pcd.example.com/morsv1/v1/"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := morsVersionedURL(tc.base); got != tc.want {
				t.Errorf("morsVersionedURL(%q)\n got: %s\nwant: %s", tc.base, got, tc.want)
			}
		})
	}
}

// The catalog is the default; an override replaces it even when the catalog
// has no mors entry at all.
func TestMorsV1ClientEndpoint(t *testing.T) {
	var asked gophercloud.EndpointOpts
	cfg := &Config{
		Region: "region-one",
		Provider: &gophercloud.ProviderClient{
			EndpointLocator: func(o gophercloud.EndpointOpts) (string, error) {
				asked = o
				return "https://region-one.example.com/mors/", nil
			},
		},
	}
	client, err := cfg.MorsV1Client()
	if err != nil {
		t.Fatalf("MorsV1Client: %v", err)
	}
	if client.Endpoint != "https://region-one.example.com/mors/v1/" {
		t.Fatalf("endpoint = %s, want the catalog URL plus v1/", client.Endpoint)
	}
	if asked.Type != "mors" || asked.Region != "region-one" || asked.Availability != gophercloud.AvailabilityPublic {
		t.Fatalf("catalog lookup = %+v, want type mors, the provider's region, public", asked)
	}

	cfg.Provider.EndpointLocator = func(gophercloud.EndpointOpts) (string, error) {
		return "", errors.New("no mors in this catalog")
	}
	if _, err := cfg.MorsV1Client(); err == nil || !strings.Contains(err.Error(), "not in the service catalog") {
		t.Fatalf("catalog miss: err = %v, want one that says mors is not in the catalog", err)
	}

	cfg.EndpointOverrides = map[string]string{"mors": "https://mors.example.com/mors"}
	client, err = cfg.MorsV1Client()
	if err != nil {
		t.Fatalf("MorsV1Client with an override: %v", err)
	}
	if client.Endpoint != "https://mors.example.com/mors/v1/" {
		t.Fatalf("endpoint = %s, want the override plus v1/", client.Endpoint)
	}
}
