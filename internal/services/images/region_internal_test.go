// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package images

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"

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

// An image whose region is region-two must be read there, not in the
// provider's region-one.
func TestImageLivesInItsRegion(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	glance := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method+" "+r.URL.Path != "GET /v2/images/img-1" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotImplemented)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id": "img-1", "name": "ubuntu-24.04", "status": "active", "container_format": "bare",
			"disk_format": "qcow2", "min_disk": 0, "min_ram": 0, "protected": false, "visibility": "private",
			"os_hidden": false, "tags": [], "checksum": "abc", "size": 1, "owner": "proj-1",
			"created_at": "2026-10-02T00:00:00Z", "updated_at": "2026-10-02T00:00:00Z"}`)
	}))
	defer glance.Close()

	r := &imageResource{config: onlyRegionTwo(fakeConfig(glance.URL))}
	var sch resource.SchemaResponse
	r.Schema(ctx, resource.SchemaRequest{}, &sch)
	s := sch.Schema
	m := baseImagePlan()
	m.ID = types.StringValue("img-1")
	m.ImageSourceURL = types.StringValue("https://example.invalid/ubuntu.qcow2")
	m.Region = types.StringValue("region-two")
	state := tfsdk.State{Schema: s, Raw: tftypes.NewValue(s.Type().TerraformType(ctx), nil)}
	if d := state.Set(ctx, &m); d.HasError() {
		t.Fatalf("building the state: %v", d)
	}
	resp := resource.ReadResponse{State: state}
	r.Read(ctx, resource.ReadRequest{State: state}, &resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("read in region-two: %v", resp.Diagnostics)
	}
}
