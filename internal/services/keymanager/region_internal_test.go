// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package keymanager

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

// A container whose region is region-two must be created, read and deleted
// there, not in the provider's region-one.
func TestContainerLivesInItsRegion(t *testing.T) {
	t.Parallel()
	r := &containerResource{config: onlyRegionTwo(newFakeBarbican(t, containerRoutes()).config)}
	s := schemaOf(t, r)

	var planned containerModel
	if d := containerPlan(t, r).Get(t.Context(), &planned); d.HasError() {
		t.Fatalf("reading the plan: %v", d)
	}
	planned.Region = types.StringValue("region-two")
	created := runCreate(r, newPlan(t, s, &planned))
	if created.Diagnostics.HasError() {
		t.Fatalf("create in region-two: %v", created.Diagnostics)
	}
	if read := runRead(r, created.State); read.Diagnostics.HasError() {
		t.Fatalf("read in region-two: %v", read.Diagnostics)
	}
	if deleted := runDelete(r, created.State); deleted.Diagnostics.HasError() {
		t.Fatalf("delete in region-two: %v", deleted.Diagnostics)
	}
}
