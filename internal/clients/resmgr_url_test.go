// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package clients

import "testing"

// resmgr serves host roles only on v1 and blueprints/host configs only on v2,
// so the provider builds a client per version off one catalog entry. These
// cases pin the URL construction, including the endpoint_overrides shapes a
// user may supply (bare root, or already carrying a version).
func TestResmgrVersionedURL(t *testing.T) {
	for _, tc := range []struct {
		name    string
		base    string
		version string
		want    string
	}{
		{"catalog root", "https://pcd.example.com/resmgr", "v2", "https://pcd.example.com/resmgr/v2/"},
		{"catalog root v1", "https://pcd.example.com/resmgr", "v1", "https://pcd.example.com/resmgr/v1/"},
		{"trailing slash", "https://pcd.example.com/resmgr/", "v2", "https://pcd.example.com/resmgr/v2/"},

		// An override may already name a version. It must be replaced, not
		// appended to, or v1 calls land on /resmgr/v2/v1/ and 404.
		{"override pinned to v2, want v1", "https://pcd.example.com/resmgr/v2", "v1", "https://pcd.example.com/resmgr/v1/"},
		{"override pinned to v2, want v2", "https://pcd.example.com/resmgr/v2/", "v2", "https://pcd.example.com/resmgr/v2/"},
		{"override pinned to v1, want v2", "https://pcd.example.com/resmgr/v1", "v2", "https://pcd.example.com/resmgr/v2/"},

		// Only v1/v2 are resmgr versions. A path segment that merely looks
		// version-ish belongs to the base URL and must survive.
		{"v3 is not a resmgr version, keep it", "https://pcd.example.com/v3", "v1", "https://pcd.example.com/v3/v1/"},
		{"host-only base", "https://pcd.example.com", "v1", "https://pcd.example.com/v1/"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := resmgrVersionedURL(tc.base, tc.version); got != tc.want {
				t.Errorf("resmgrVersionedURL(%q, %q)\n got: %s\nwant: %s", tc.base, tc.version, got, tc.want)
			}
		})
	}
}
