// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package dns

import (
	"fmt"
	"testing"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/platform9/terraform-provider-pcd/internal/clients"
)

// onlyRegionTwo makes config's catalog answer for region-two alone, while the
// provider stays in region-one, so a client built for any other region fails.
func onlyRegionTwo(config *clients.Config) *clients.Config {
	locate := config.Provider.EndpointLocator
	config.Provider.EndpointLocator = func(opts gophercloud.EndpointOpts) (string, error) {
		if opts.Region != "region-two" {
			return "", fmt.Errorf("no %s endpoint in region %q", opts.Type, opts.Region)
		}
		return locate(opts)
	}
	return config
}

// A zone whose region is region-two must be updated and read there, not in the
// provider's region-one.
func TestZoneLivesInItsRegion(t *testing.T) {
	t.Parallel()
	r := &zoneResource{config: onlyRegionTwo(newFakeDesignate(t, zoneRoutes()).config)}
	s := schemaOf(t, r)

	prior := activeZone(t)
	prior.Region = types.StringValue("region-two")
	planned := prior
	planned.Description = types.StringValue("updated")
	planned.Serial = types.Int64Unknown()
	if resp := runUpdate(r, newPlan(t, s, &planned), newState(t, s, &prior)); resp.Diagnostics.HasError() {
		t.Fatalf("update in region-two: %v", resp.Diagnostics)
	}
	if resp := runRead(r, newState(t, s, &prior)); resp.Diagnostics.HasError() {
		t.Fatalf("read in region-two: %v", resp.Diagnostics)
	}
}
