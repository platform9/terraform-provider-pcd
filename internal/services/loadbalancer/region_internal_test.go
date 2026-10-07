// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package loadbalancer

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

// A listener whose region is region-two must be created and read there, along
// with the waits on its load balancer, not in the provider's region-one.
func TestListenerLivesInItsRegion(t *testing.T) {
	t.Parallel()
	for _, c := range lbChildren() {
		if c.name != "listener" {
			continue
		}
		r := c.resource(onlyRegionTwo(newFakeOctavia(t, c.routes()).config))
		planned := *c.planned.(*listenerModel)
		planned.Region = types.StringValue("region-two")
		created := runCreate(r, newPlan(t, schemaOf(t, r), &planned))
		if created.Diagnostics.HasError() {
			t.Fatalf("create in region-two: %v", created.Diagnostics)
		}
		if read := runRead(r, created.State); read.Diagnostics.HasError() {
			t.Fatalf("read in region-two: %v", read.Diagnostics)
		}
		return
	}
	t.Fatal("no listener among lbChildren")
}
