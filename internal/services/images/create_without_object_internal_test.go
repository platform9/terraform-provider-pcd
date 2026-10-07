// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package images

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

// gophercloud decodes Glance's create answer as the image itself, so a 201
// whose body is JSON null decodes to a nil image and no error. Create used to
// dereference it and crash the provider. It must report an error and leave
// nothing in state.
func TestImageCreateRefusesAnAnswerWithoutTheImage(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	glance := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method+" "+r.URL.Path != "POST /v2/images" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotImplemented)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, `null`)
	}))
	defer glance.Close()

	r := &imageResource{config: fakeConfig(glance.URL)}
	var sch resource.SchemaResponse
	r.Schema(ctx, resource.SchemaRequest{}, &sch)
	s := sch.Schema

	resp := resource.CreateResponse{State: tfsdk.State{Schema: s, Raw: tftypes.NewValue(s.Type().TerraformType(ctx), nil)}}
	func() {
		defer func() {
			if p := recover(); p != nil {
				t.Fatalf("create panicked: %v", p)
			}
		}()
		r.Create(ctx, resource.CreateRequest{Plan: imagePlan(ctx, t, s)}, &resp)
	}()
	if !resp.Diagnostics.HasError() {
		t.Fatal("create succeeded; want an error for an answer without the image")
	}
	if !resp.State.Raw.IsNull() {
		t.Fatalf("create left a row: %v", resp.State.Raw)
	}
}
