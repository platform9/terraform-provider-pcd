// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package dns

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"

	"github.com/platform9/terraform-provider-pcd/internal/clients"
)

// noPanic runs f and fails the test if f panics. A panic in a resource method
// crashes the provider plugin, which fails the whole apply.
func noPanic(t *testing.T, f func()) {
	t.Helper()
	defer func() {
		if p := recover(); p != nil {
			t.Fatalf("panicked: %v", p)
		}
	}()
	f()
}

// gophercloud decodes a Designate answer as the object itself, so a 2xx whose
// body is JSON null decodes to a nil object and no error. The zone and
// recordset creates used to dereference it and crash the provider. Each must
// report an error and leave nothing in state.
func TestCreateRefusesAnAnswerWithoutTheObject(t *testing.T) {
	t.Parallel()
	records, d := types.SetValueFrom(context.Background(), types.StringType, []string{"10.1.0.1"})
	if d.HasError() {
		t.Fatalf("building the records set: %v", d)
	}
	creates := []struct {
		name, route string
		newResource func(*clients.Config) resource.Resource
		planned     any
	}{
		{"zone", "POST " + zonesPath,
			func(c *clients.Config) resource.Resource { return &zoneResource{config: c} },
			&zoneModel{ID: types.StringUnknown(), Name: types.StringValue("tf-acc-example.com."),
				Type: types.StringValue("PRIMARY"), Email: types.StringValue("dns@example.com"),
				TTL: types.Int64Unknown(), Description: types.StringUnknown(), Masters: types.ListUnknown(types.StringType),
				Attributes: types.MapUnknown(types.StringType), Serial: types.Int64Unknown(), Status: types.StringUnknown(),
				PoolID: types.StringUnknown(), ProjectID: types.StringUnknown(), Region: types.StringUnknown()}},
		{"recordset", "POST " + recordSetsPath,
			func(c *clients.Config) resource.Resource { return &recordSetResource{config: c} },
			&recordSetModel{ID: types.StringUnknown(), ZoneID: types.StringValue("zone-1"),
				Name: types.StringValue("www.tf-acc-example.com."), Type: types.StringValue("A"), Records: records,
				TTL: types.Int64Unknown(), Description: types.StringUnknown(), Status: types.StringUnknown(),
				Region: types.StringUnknown()}},
	}
	for _, c := range creates {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			r := c.newResource(newFakeDesignate(t, designateRoutes{c.route: reply(http.StatusAccepted, `null`)}).config)
			s := schemaOf(t, r)
			plan := newPlan(t, s, c.planned)
			resp := resource.CreateResponse{State: tfsdk.State{Schema: s, Raw: tftypes.NewValue(s.Type().TerraformType(ctx), nil)}}
			noPanic(t, func() { r.Create(ctx, resource.CreateRequest{Plan: plan}, &resp) })
			if !resp.Diagnostics.HasError() {
				t.Fatal("create succeeded; want an error for an answer without the object")
			}
			if !resp.State.Raw.IsNull() {
				t.Fatalf("create left a row: %v", resp.State.Raw)
			}
		})
	}
}

// The waits poll the zone or recordset, and used to dereference the nil object
// a JSON null 200 decodes to. Each must fail on it instead.
func TestWaitsRefuseAnAnswerWithoutTheObject(t *testing.T) {
	t.Parallel()
	waits := []struct {
		name, route string
		wait        func(context.Context, *gophercloud.ServiceClient) error
	}{
		{"zone active", "GET " + zonePath, func(ctx context.Context, c *gophercloud.ServiceClient) error {
			return waitForZoneActive(ctx, c, "zone-1", 2*time.Second)
		}},
		{"zone deleted", "GET " + zonePath, func(ctx context.Context, c *gophercloud.ServiceClient) error {
			return waitForZoneDeleted(ctx, c, "zone-1", 2*time.Second)
		}},
		{"recordset active", "GET " + recordSetPath, func(ctx context.Context, c *gophercloud.ServiceClient) error {
			return waitForRecordSetActive(ctx, c, "zone-1", "rr-1", 2*time.Second)
		}},
	}
	for _, w := range waits {
		t.Run(w.name, func(t *testing.T) {
			t.Parallel()
			client, err := newFakeDesignate(t, designateRoutes{w.route: reply(http.StatusOK, `null`)}).config.DNSV2Client()
			if err != nil {
				t.Fatalf("building the client: %v", err)
			}
			noPanic(t, func() { err = w.wait(context.Background(), client) })
			if !errors.Is(err, clients.ErrNoObject) {
				t.Fatalf("wait error = %v; want it to report the answer without the object", err)
			}
		})
	}
}
