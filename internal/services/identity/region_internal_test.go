// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package identity

import (
	"fmt"
	"net/http"
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

// A project whose region is region-two must be created there, not in the
// provider's region-one.
func TestProjectLivesInItsRegion(t *testing.T) {
	t.Parallel()
	project := `{"project": {"id": "proj-9", "name": "workload", "description": "", "domain_id": "default",
		"enabled": true, "is_domain": false, "parent_id": "default", "tags": []}}`
	r := &projectResource{config: onlyRegionTwo(newFakeKeystone(t, map[string]http.HandlerFunc{
		"POST /v3/projects":       reply(http.StatusCreated, project),
		"GET /v3/projects/proj-9": reply(http.StatusOK, project),
	}))}
	resp, err := runCreate(r, &projectModel{ID: types.StringUnknown(), Name: types.StringValue("workload"),
		Description: types.StringUnknown(), DomainID: types.StringUnknown(), Enabled: types.BoolValue(true),
		IsDomain: types.BoolValue(false), ParentID: types.StringUnknown(), Tags: types.SetUnknown(types.StringType),
		Region: types.StringValue("region-two")})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Diagnostics.HasError() {
		t.Fatalf("create in region-two: %v", resp.Diagnostics)
	}
}
