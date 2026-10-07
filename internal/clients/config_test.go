// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package clients

import (
	"encoding/json"
	"testing"
)

// Keystone takes a project scope as either an ID alone or a name plus its
// domain; gophercloud refuses any other mix before sending the request. These
// cases pin the scope the token request carries for the credential shapes a
// provider block, an openrc, or a clouds.yaml entry produce.
func TestAuthOptionsScope(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  Config
		want string // scope JSON in the token request; empty when none is sent
	}{
		{
			name: "project ID with user and project domain IDs",
			cfg:  Config{Username: "admin", Password: "pw", TenantID: "p-123", UserDomainID: "default", ProjectDomainID: "default"},
			want: `{"project":{"id":"p-123"}}`,
		},
		{
			name: "project ID with domain names",
			cfg:  Config{Username: "admin", Password: "pw", TenantID: "p-123", UserDomainName: "Default", ProjectDomainName: "Default"},
			want: `{"project":{"id":"p-123"}}`,
		},
		{
			name: "project name with project domain ID",
			cfg:  Config{Username: "admin", Password: "pw", TenantName: "service", UserDomainID: "default", ProjectDomainID: "d-1"},
			want: `{"project":{"domain":{"id":"d-1"},"name":"service"}}`,
		},
		{
			name: "project name falls back to the user domain",
			cfg:  Config{Username: "admin", Password: "pw", TenantName: "service", UserDomainName: "Default"},
			want: `{"project":{"domain":{"name":"Default"},"name":"service"}}`,
		},
		{
			// An openrc commonly exports OS_PROJECT_ID and OS_PROJECT_NAME together.
			name: "project ID and name both set, ID wins",
			cfg:  Config{Username: "admin", Password: "pw", TenantID: "p-123", TenantName: "service", UserDomainName: "Default", ProjectDomainName: "Default"},
			want: `{"project":{"id":"p-123"}}`,
		},
		{
			name: "application credential ID carries no scope",
			cfg:  Config{AppCredID: "ac-1", AppCredSecret: "s", TenantID: "p-123", ProjectDomainID: "default"},
			want: "",
		},
		{
			name: "application credential name carries no scope",
			cfg:  Config{AppCredName: "ci", AppCredSecret: "s", Username: "admin", UserDomainID: "default", TenantName: "service"},
			want: "",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ao := tc.cfg.authOptions()
			scope, err := ao.ToTokenV3ScopeMap()
			if err != nil {
				t.Fatalf("building the token scope: %v", err)
			}
			got := ""
			if len(scope) != 0 {
				b, err := json.Marshal(scope)
				if err != nil {
					t.Fatalf("encoding the token scope: %v", err)
				}
				got = string(b)
			}
			if got != tc.want {
				t.Errorf("token scope\n got: %s\nwant: %s", got, tc.want)
			}
		})
	}
}
