// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package blockstorage

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

// A snapshot and a volume whose region is region-two must be updated and read
// back there, not in the provider's region-one.
func TestSnapshotAndVolumeLiveInTheirRegion(t *testing.T) {
	t.Parallel()
	cinder := &fakeCinderMetadata{snapName: "nightly", snapMeta: map[string]string{}, volName: "data", volMeta: map[string]string{}}
	config := onlyRegionTwo(fakeConfig(cinder.serve(t).URL))

	prior := snapshotWith(t, "nightly", map[string]string{})
	prior.Region = types.StringValue("region-two")
	planned := prior
	planned.Name = types.StringValue("nightly-renamed")
	if resp := runModelUpdate(t, &snapshotResource{config: config}, &prior, &planned); resp.Diagnostics.HasError() {
		t.Fatalf("snapshot update in region-two: %v", resp.Diagnostics)
	}

	vol := volumeModel{
		ID: types.StringValue("vol-1"), Name: types.StringValue("data"), Size: types.Int64Value(1),
		Description: types.StringValue(""), VolumeType: types.StringValue("nfs"), AvailabilityZone: types.StringValue("nova"),
		SnapshotID: types.StringNull(), SourceVolID: types.StringNull(), ImageID: types.StringNull(),
		Metadata: stringMapValue(t, map[string]string{}), Bootable: types.BoolValue(false),
		Encrypted: types.BoolValue(false), Status: types.StringValue("available"), Region: types.StringValue("region-two"),
	}
	renamed := vol
	renamed.Name = types.StringValue("data-renamed")
	if resp := runModelUpdate(t, &volumeResource{config: config}, &vol, &renamed); resp.Diagnostics.HasError() {
		t.Fatalf("volume update in region-two: %v", resp.Diagnostics)
	}
}
