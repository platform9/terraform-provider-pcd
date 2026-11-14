// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package clients

import (
	"errors"
	"testing"

	"github.com/gophercloud/gophercloud/v2"
)

// hagrid lives at /hagrid/ on the host that serves Keystone, whatever path
// Keystone itself has; its ingress routes by host, so the host must be kept
// exactly, port included.
func TestHagridBaseURL(t *testing.T) {
	for _, tc := range []struct {
		name, in, want string
		wantErr        bool
	}{
		{name: "PCD catalog URL", in: "https://pcd.example.com/keystone/v3", want: "https://pcd.example.com/hagrid/"},
		{name: "trailing slash", in: "https://pcd.example.com/keystone/v3/", want: "https://pcd.example.com/hagrid/"},
		{name: "port kept", in: "https://pcd.example.com:8443/keystone", want: "https://pcd.example.com:8443/hagrid/"},
		{name: "host only", in: "https://pcd.example.com", want: "https://pcd.example.com/hagrid/"},
		{name: "relative", in: "/keystone/v3", wantErr: true},
		{name: "empty", in: "", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := hagridBaseURL(tc.in)
			if (err != nil) != tc.wantErr {
				t.Fatalf("hagridBaseURL(%q) err = %v, wantErr %v", tc.in, err, tc.wantErr)
			}
			if got != tc.want {
				t.Fatalf("hagridBaseURL(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// The catalog's Infra identity endpoint wins, the provider's region is the
// fallback, and an override replaces both.
func TestHagridClientEndpoint(t *testing.T) {
	catalog := map[string]string{
		"Infra":      "https://infra.example.com/keystone/v3/",
		"region-one": "https://region-one.example.com/keystone/v3/",
	}
	cfg := &Config{
		Region: "region-one",
		Provider: &gophercloud.ProviderClient{
			EndpointLocator: func(o gophercloud.EndpointOpts) (string, error) {
				if o.Type != "identity" || o.Availability != gophercloud.AvailabilityPublic {
					t.Errorf("lookup %+v, want the public identity endpoint", o)
				}
				if u, ok := catalog[o.Region]; ok {
					return u, nil
				}
				return "", errors.New("no endpoint")
			},
		},
	}
	client, err := cfg.HagridClient()
	if err != nil || client.Endpoint != "https://infra.example.com/hagrid/" {
		t.Fatalf("with an Infra endpoint: %v, %v; want https://infra.example.com/hagrid/", client, err)
	}

	delete(catalog, "Infra")
	client, err = cfg.HagridClient()
	if err != nil || client.Endpoint != "https://region-one.example.com/hagrid/" {
		t.Fatalf("without an Infra endpoint: %v, %v; want the provider region's host", client, err)
	}

	delete(catalog, "region-one")
	if _, err := cfg.HagridClient(); err == nil {
		t.Fatal("no identity endpoint at all: no error")
	}

	cfg.EndpointOverrides = map[string]string{"hagrid": "https://sso.example.com/hagrid"}
	client, err = cfg.HagridClient()
	if err != nil || client.Endpoint != "https://sso.example.com/hagrid/" {
		t.Fatalf("with an override: %v, %v; want the override with a trailing slash", client, err)
	}
}
