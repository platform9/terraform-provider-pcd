// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package networking

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
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

// A security group whose region is region-two must be created, read, updated
// and deleted there, not in the provider's region-one. region used to be
// stored and never used, so every call went to the provider's region.
func TestSecgroupLivesInItsRegion(t *testing.T) {
	t.Parallel()
	r := &secgroupResource{config: onlyRegionTwo(newFakeNeutron(t, secgroupRoutes()).config)}
	s := schemaOf(t, r)

	planned := secgroupModelFor(t, true)
	planned.ID = types.StringUnknown()
	planned.Stateful = types.BoolUnknown()
	planned.TenantID = types.StringUnknown()
	planned.Tags = types.SetUnknown(types.StringType)
	planned.Region = types.StringValue("region-two")
	created := runCreate(r, newPlan(t, s, &planned))
	if created.Diagnostics.HasError() {
		t.Fatalf("create in region-two: %v", created.Diagnostics)
	}
	if got := secgroupRow(t, created.State); got.Region.ValueString() != "region-two" {
		t.Fatalf("create state region = %s, want region-two", got.Region)
	}

	if read := runRead(r, created.State); read.Diagnostics.HasError() {
		t.Fatalf("read in region-two: %v", read.Diagnostics)
	}
	prior := secgroupRow(t, created.State)
	renamed := prior
	renamed.Name = types.StringValue(secgroupName + "-renamed")
	if updated := runUpdate(r, newPlan(t, s, &renamed), created.State); updated.Diagnostics.HasError() {
		t.Fatalf("update in region-two: %v", updated.Diagnostics)
	}
	if deleted := runDelete(r, created.State); deleted.Diagnostics.HasError() {
		t.Fatalf("delete in region-two: %v", deleted.Diagnostics)
	}
}

// A data source whose region is region-two must look the object up there.
func TestSecgroupDataSourceLooksInItsRegion(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	d := &secgroupDataSource{config: onlyRegionTwo(newFakeNeutron(t, neutronRoutes{
		"GET " + secgroupPath: reply(http.StatusOK, `{"security_group": `+secgroupJSON+`}`),
	}).config)}
	var sch datasource.SchemaResponse
	d.Schema(ctx, datasource.SchemaRequest{}, &sch)
	config := tfsdk.Config{Schema: sch.Schema, Raw: tftypes.NewValue(sch.Schema.Type().TerraformType(ctx), nil)}
	state := tfsdk.State{Schema: sch.Schema, Raw: tftypes.NewValue(sch.Schema.Type().TerraformType(ctx), nil)}
	model := secgroupDataSourceModel{
		ID: types.StringNull(), SecgroupID: types.StringValue(secgroupID), Name: types.StringNull(),
		Description: types.StringNull(), TenantID: types.StringNull(), Region: types.StringValue("region-two"),
	}
	if diags := state.Set(ctx, &model); diags.HasError() {
		t.Fatalf("building the config: %v", diags)
	}
	config.Raw = state.Raw
	resp := datasource.ReadResponse{State: state}
	d.Read(ctx, datasource.ReadRequest{Config: config}, &resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("data source read in region-two: %v", resp.Diagnostics)
	}
}
