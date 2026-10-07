// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package images

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"

	"github.com/platform9/terraform-provider-pcd/internal/clients"
)

// newFakeGlance starts a test server that answers from routes, each keyed
// "METHOD /path", and returns a clients.Config whose Glance client reaches it.
// ImageV2Client appends v2/ to the endpoint, so every path carries a /v2/
// prefix. A request no route names fails the test. The client gets a transport
// of its own, so closing this server cannot cut a request another parallel
// test is sending.
func newFakeGlance(t *testing.T, routes map[string]http.HandlerFunc) *clients.Config {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if handle, ok := routes[r.Method+" "+r.URL.Path]; ok {
			handle(w, r)
			return
		}
		t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusNotImplemented)
	}))
	t.Cleanup(srv.Close)
	return &clients.Config{
		Region: "region-one",
		Provider: &gophercloud.ProviderClient{
			HTTPClient:      http.Client{Transport: &http.Transport{}},
			EndpointLocator: func(gophercloud.EndpointOpts) (string, error) { return srv.URL + "/", nil },
		},
	}
}

// reply answers with status and, unless body is empty, a JSON body.
func reply(status int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		if body != "" {
			w.Header().Set("Content-Type", "application/json")
		}
		w.WriteHeader(status)
		fmt.Fprint(w, body)
	}
}

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

// reportsNoObject reports whether diags holds an error that names the answer
// without the object.
func reportsNoObject(diags diag.Diagnostics) bool {
	for _, d := range diags.Errors() {
		if strings.Contains(d.Detail(), clients.ErrNoObject.Error()) {
			return true
		}
	}
	return false
}

// imageRow is an active image as Terraform recorded it.
func imageRow() *imageModel {
	return &imageModel{
		ID:              types.StringValue("img-1"),
		Name:            types.StringValue("ubuntu-24.04"),
		ContainerFormat: types.StringValue("bare"),
		DiskFormat:      types.StringValue("qcow2"),
		LocalFilePath:   types.StringNull(),
		ImageSourceURL:  types.StringValue("https://example.invalid/ubuntu.qcow2"),
		MinDiskGB:       types.Int64Value(0),
		MinRAMMB:        types.Int64Value(0),
		Protected:       types.BoolValue(false),
		Visibility:      types.StringValue("shared"),
		Hidden:          types.BoolValue(false),
		Tags:            types.SetValueMust(types.StringType, []attr.Value{}),
		VerifyChecksum:  types.BoolValue(true),
		Properties:      types.MapValueMust(types.StringType, map[string]attr.Value{}),
		Checksum:        types.StringValue(""),
		SizeBytes:       types.Int64Value(0),
		Status:          types.StringValue("active"),
		Owner:           types.StringValue("proj-1"),
		CreatedAt:       types.StringValue("2026-09-19T00:00:00Z"),
		UpdatedAt:       types.StringValue("2026-09-19T00:00:00Z"),
		Region:          types.StringValue("region-one"),
	}
}

// newImageState returns a state of r's schema that holds m.
func newImageState(t *testing.T, r *imageResource, m *imageModel) tfsdk.State {
	t.Helper()
	ctx := context.Background()
	var sch resource.SchemaResponse
	r.Schema(ctx, resource.SchemaRequest{}, &sch)
	s := sch.Schema
	state := tfsdk.State{Schema: s, Raw: tftypes.NewValue(s.Type().TerraformType(ctx), nil)}
	if d := state.Set(ctx, m); d.HasError() {
		t.Fatalf("building the state: %v", d)
	}
	return state
}

// gophercloud decodes Glance's answer as the image itself, so a 200 whose body
// is JSON null decodes to a nil image and no error. Read used to dereference
// it and crash the provider. It must report an error and keep the row: the
// image may well exist, so the answer is no reason to drop it from state.
func TestImageReadRefusesAnAnswerWithoutTheImage(t *testing.T) {
	t.Parallel()
	r := &imageResource{config: newFakeGlance(t, map[string]http.HandlerFunc{
		"GET /v2/images/img-1": reply(http.StatusOK, `null`),
	})}
	prior := newImageState(t, r, imageRow())
	resp := resource.ReadResponse{State: prior}
	noPanic(t, func() { r.Read(context.Background(), resource.ReadRequest{State: prior}, &resp) })
	if !reportsNoObject(resp.Diagnostics) {
		t.Fatalf("read diagnostics = %v; want an error for the answer without the image", resp.Diagnostics)
	}
	if !resp.State.Raw.Equal(prior.Raw) {
		t.Fatalf("read changed the row to %v; want it kept as %v", resp.State.Raw, prior.Raw)
	}
}

// Update reads the image back after patching it, and crashed the same way. It
// must report an error and keep the prior row.
func TestImageUpdateRefusesAReadBackWithoutTheImage(t *testing.T) {
	t.Parallel()
	r := &imageResource{config: newFakeGlance(t, map[string]http.HandlerFunc{
		"PATCH /v2/images/img-1": reply(http.StatusOK, imageBody("active", nil)),
		"GET /v2/images/img-1":   reply(http.StatusOK, `null`),
	})}
	prior := newImageState(t, r, imageRow())
	renamed := imageRow()
	renamed.Name = types.StringValue("ubuntu-24.04-lts")
	plan := tfsdk.Plan(newImageState(t, r, renamed))
	resp := resource.UpdateResponse{State: prior}
	noPanic(t, func() { r.Update(context.Background(), resource.UpdateRequest{Plan: plan, State: prior}, &resp) })
	if !reportsNoObject(resp.Diagnostics) {
		t.Fatalf("update diagnostics = %v; want an error for the answer without the image", resp.Diagnostics)
	}
	if !resp.State.Raw.Equal(prior.Raw) {
		t.Fatalf("update changed the row to %v; want the prior row %v", resp.State.Raw, prior.Raw)
	}
}

// The checksum check after an upload and the wait for an active image read the
// image too, and used to dereference the nil image as well. Each must fail on
// it instead.
func TestImageChecksAndWaitsRefuseAnAnswerWithoutTheImage(t *testing.T) {
	t.Parallel()
	localPath := filepath.Join(t.TempDir(), "ubuntu.qcow2")
	if err := os.WriteFile(localPath, []byte("not a real qcow2, just test data"), 0o600); err != nil {
		t.Fatalf("writing the local image file: %v", err)
	}
	r := &imageResource{}
	calls := []struct {
		name   string
		routes map[string]http.HandlerFunc
		call   func(context.Context, *gophercloud.ServiceClient) error
	}{
		{"checksum after upload", map[string]http.HandlerFunc{
			"PUT /v2/images/img-1/file": reply(http.StatusNoContent, ""),
			"GET /v2/images/img-1":      reply(http.StatusOK, `null`),
		}, func(ctx context.Context, c *gophercloud.ServiceClient) error {
			return r.uploadLocalFile(ctx, c, "img-1", localPath, true)
		}},
		{"wait for active", map[string]http.HandlerFunc{
			"GET /v2/images/img-1": reply(http.StatusOK, `null`),
		}, func(ctx context.Context, c *gophercloud.ServiceClient) error {
			_, err := waitForImageActive(ctx, c, "img-1", 2*time.Second)
			return err
		}},
	}
	for _, c := range calls {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			client, err := newFakeGlance(t, c.routes).ImageV2Client()
			if err != nil {
				t.Fatalf("building the client: %v", err)
			}
			noPanic(t, func() { err = c.call(context.Background(), client) })
			if !errors.Is(err, clients.ErrNoObject) {
				t.Fatalf("error = %v; want it to report the answer without the image", err)
			}
		})
	}
}

// The image data source's lookup by ID used to dereference the nil image as
// well. It must report an error and set nothing.
func TestImageDataSourceRefusesAnAnswerWithoutTheImage(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	d := &imageDataSource{config: newFakeGlance(t, map[string]http.HandlerFunc{
		"GET /v2/images/img-1": reply(http.StatusOK, `null`),
	})}
	var schemaResp datasource.SchemaResponse
	d.Schema(ctx, datasource.SchemaRequest{}, &schemaResp)
	s := schemaResp.Schema
	typ := s.Type().TerraformType(ctx).(tftypes.Object)
	vals := make(map[string]tftypes.Value, len(typ.AttributeTypes))
	for name, at := range typ.AttributeTypes {
		vals[name] = tftypes.NewValue(at, nil)
	}
	vals["image_id"] = tftypes.NewValue(tftypes.String, "img-1")
	config := tfsdk.Config{Schema: s, Raw: tftypes.NewValue(typ, vals)}
	resp := datasource.ReadResponse{State: tfsdk.State{Schema: s, Raw: tftypes.NewValue(typ, nil)}}
	noPanic(t, func() { d.Read(ctx, datasource.ReadRequest{Config: config}, &resp) })
	if !reportsNoObject(resp.Diagnostics) {
		t.Fatalf("read diagnostics = %v; want an error for the answer without the image", resp.Diagnostics)
	}
	if !resp.State.Raw.IsNull() {
		t.Fatalf("read set %v; want nothing set", resp.State.Raw)
	}
}
