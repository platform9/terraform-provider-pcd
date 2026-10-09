// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package images

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

// These tests reuse imageAccessGlance and the helpers in image_access_internal_test.go.

const imageAccessToken = `{"token": {"project": {"id": "p-self", "name": "team-b", "domain": {"id": "default", "name": "Default"}},
	"user": {"id": "u1", "name": "admin", "domain": {"id": "default", "name": "Default"}}}}`

// member_id detection follows upstream (the only visible member) and falls
// back to the token's project when the list does not single one out.
func TestImageAccessAcceptDetectsMember(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name       string
		listCode   int
		listBody   string
		wantMember string
		wantToken  bool
	}{
		{name: "one visible member", listBody: `{"members": [` + imageMemberJSON("p-other", "pending") + `]}`, wantMember: "p-other"},
		{name: "none visible", listBody: `{"members": []}`, wantMember: "p-self", wantToken: true},
		{name: "admin sees several", listBody: `{"members": [` + imageMemberJSON("p-a", "pending") + `, ` + imageMemberJSON("p-b", "pending") + `]}`, wantMember: "p-self", wantToken: true},
		{name: "image not visible", listCode: http.StatusNotFound, wantMember: "p-self", wantToken: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := &imageAccessGlance{listCode: tc.listCode, listBody: tc.listBody, tokenBody: imageAccessToken}
			srv := g.serve(t)
			r := &imageAccessAcceptResource{config: imageAccessTestConfig(srv.URL)}
			resp := imageAccessCreate(ctx, t, r, imageAccessPlan(types.StringUnknown(), types.StringValue("accepted")))
			if resp.Diagnostics.HasError() {
				t.Fatalf("create: %v", resp.Diagnostics)
			}
			if n := g.count(http.MethodPut, "/v2/images/i1/members/"+tc.wantMember); n != 1 {
				t.Fatalf("PUT for %s sent %d times, want 1 (calls %+v)", tc.wantMember, n, g.seen())
			}
			if got := g.count(http.MethodGet, "/v3/auth/tokens") > 0; got != tc.wantToken {
				t.Fatalf("token read = %v, want %v", got, tc.wantToken)
			}
			if got := imageAccessStateModel(ctx, t, resp.State); got.MemberID.ValueString() != tc.wantMember || got.ID.ValueString() != "i1/"+tc.wantMember {
				t.Fatalf("state member_id=%s id=%s, want %s", got.MemberID, got.ID, tc.wantMember)
			}
		})
	}
}

// An explicit member_id is used as given: no list, no token read.
func TestImageAccessAcceptUsesExplicitMember(t *testing.T) {
	ctx := context.Background()
	g := &imageAccessGlance{}
	srv := g.serve(t)
	r := &imageAccessAcceptResource{config: imageAccessTestConfig(srv.URL)}
	resp := imageAccessCreate(ctx, t, r, imageAccessPlan(types.StringValue("p2"), types.StringValue("rejected")))
	if resp.Diagnostics.HasError() {
		t.Fatalf("create: %v", resp.Diagnostics)
	}
	calls := g.seen()
	if len(calls) != 1 || calls[0].path != "/v2/images/i1/members/p2" || calls[0].body["status"] != "rejected" {
		t.Fatalf("calls = %+v, want only PUT /v2/images/i1/members/p2 {\"status\": \"rejected\"}", calls)
	}
}

// A token scoped to no project, with no single visible member, leaves nothing
// to detect: the provider asks for member_id and sends no status change.
func TestImageAccessAcceptNeedsAProjectScopedToken(t *testing.T) {
	ctx := context.Background()
	g := &imageAccessGlance{listBody: `{"members": []}`,
		tokenBody: `{"token": {"domain": {"id": "default", "name": "Default"}, "user": {"id": "u1", "name": "admin"}}}`}
	srv := g.serve(t)
	r := &imageAccessAcceptResource{config: imageAccessTestConfig(srv.URL)}
	resp := imageAccessCreate(ctx, t, r, imageAccessPlan(types.StringUnknown(), types.StringValue("accepted")))
	if !resp.Diagnostics.HasError() {
		t.Fatal("create succeeded without a member to decide for")
	}
	if d := resp.Diagnostics.Errors()[0]; d.Summary() != "Cannot determine the member project" || !strings.Contains(d.Detail(), "set member_id") {
		t.Fatalf("diagnostic = %q: %q, want %q asking to set member_id", d.Summary(), d.Detail(), "Cannot determine the member project")
	}
	for _, c := range g.seen() {
		if c.method == http.MethodPut {
			t.Fatalf("calls = %+v, want no PUT", g.seen())
		}
	}
}

// A project with no share to decide on gets the provider's explanation.
func TestImageAccessAcceptExplainsMissingShare(t *testing.T) {
	ctx := context.Background()
	g := &imageAccessGlance{putCode: http.StatusNotFound}
	srv := g.serve(t)
	r := &imageAccessAcceptResource{config: imageAccessTestConfig(srv.URL)}
	resp := imageAccessCreate(ctx, t, r, imageAccessPlan(types.StringValue("p2"), types.StringValue("accepted")))
	if !resp.Diagnostics.HasError() {
		t.Fatal("create succeeded with no share")
	}
	if summary := resp.Diagnostics.Errors()[0].Summary(); summary != "Image is not shared with this project" {
		t.Fatalf("summary = %q, want %q", summary, "Image is not shared with this project")
	}
}

// Destroying the accept resource rejects the image (a member cannot remove
// itself); gone is success, a 403 on an image no longer shared is a warning,
// and a 403 on an image still shared is an error.
func TestImageAccessAcceptDeleteRejects(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name       string
		code       int
		visibility string // the image's visibility, read after a 403
		wantErr    bool
		wantWarn   bool
	}{
		{name: "rejected", code: http.StatusOK},
		{name: "already gone", code: http.StatusNotFound},
		{name: "no longer shared", code: http.StatusForbidden, visibility: "public", wantWarn: true},
		{name: "still shared", code: http.StatusForbidden, visibility: "shared", wantErr: true},
		{name: "server error", code: http.StatusInternalServerError, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := &imageAccessGlance{putCode: tc.code, imageBody: imageAccessImageJSON(tc.visibility)}
			srv := g.serve(t)
			r := &imageAccessAcceptResource{config: imageAccessTestConfig(srv.URL)}
			st := imageAccessState(ctx, t, r, "accepted")
			resp := resource.DeleteResponse{State: st}
			r.Delete(ctx, resource.DeleteRequest{State: st}, &resp)
			if got := resp.Diagnostics.HasError(); got != tc.wantErr {
				t.Fatalf("delete error = %v, want %v: %v", got, tc.wantErr, resp.Diagnostics)
			}
			if got := resp.Diagnostics.WarningsCount() > 0; got != tc.wantWarn {
				t.Fatalf("delete warning = %v, want %v", got, tc.wantWarn)
			}
			calls := g.seen()
			if g.count(http.MethodPut, "/v2/images/i1/members/p1") != 1 || calls[0].method != http.MethodPut || calls[0].body["status"] != "rejected" {
				t.Fatalf("calls = %+v, want one PUT /v2/images/i1/members/p1 {\"status\": \"rejected\"} first", calls)
			}
		})
	}
}

// A bare image ID imports with the detected member; image/member uses the
// member given.
func TestImageAccessAcceptImportState(t *testing.T) {
	ctx := context.Background()
	g := &imageAccessGlance{listBody: `{"members": [` + imageMemberJSON("p-other", "accepted") + `]}`}
	srv := g.serve(t)
	r := &imageAccessAcceptResource{config: imageAccessTestConfig(srv.URL)}
	s := imageAccessSchema(ctx, t, r)
	for id, want := range map[string]string{"i1": "p-other", "i1/p2": "p2"} {
		resp := resource.ImportStateResponse{State: tfsdk.State{Schema: s, Raw: tftypes.NewValue(s.Type().TerraformType(ctx), nil)}}
		r.ImportState(ctx, resource.ImportStateRequest{ID: id}, &resp)
		if resp.Diagnostics.HasError() {
			t.Fatalf("import %q: %v", id, resp.Diagnostics)
		}
		if got := imageAccessStateModel(ctx, t, resp.State); got.MemberID.ValueString() != want || got.ID.ValueString() != "i1/"+want {
			t.Fatalf("import %q: member_id=%s id=%s, want %s", id, got.MemberID, got.ID, want)
		}
	}
	for _, bad := range []string{"", "/p2", "i1/", "i1/p2/x"} {
		resp := resource.ImportStateResponse{State: tfsdk.State{Schema: s, Raw: tftypes.NewValue(s.Type().TerraformType(ctx), nil)}}
		r.ImportState(ctx, resource.ImportStateRequest{ID: bad}, &resp)
		if !resp.Diagnostics.HasError() {
			t.Errorf("import %q accepted; want an error", bad)
		}
	}
}

// imageDataSourceRead runs a data source's Read against the fake with the given
// configuration values; attributes not in set are null.
func imageDataSourceRead(ctx context.Context, t *testing.T, d datasource.DataSource, set map[string]tftypes.Value) datasource.ReadResponse {
	t.Helper()
	var sch datasource.SchemaResponse
	d.Schema(ctx, datasource.SchemaRequest{}, &sch)
	s := sch.Schema
	typ := s.Type().TerraformType(ctx).(tftypes.Object)
	vals := map[string]tftypes.Value{}
	for name, at := range typ.AttributeTypes {
		if v, ok := set[name]; ok {
			vals[name] = v
		} else {
			vals[name] = tftypes.NewValue(at, nil)
		}
	}
	cfg := tfsdk.Config{Schema: s, Raw: tftypes.NewValue(typ, vals)}
	resp := datasource.ReadResponse{State: tfsdk.State{Schema: s, Raw: tftypes.NewValue(typ, nil)}}
	d.Read(ctx, datasource.ReadRequest{Config: cfg}, &resp)
	return resp
}

// member_status reaches Glance as the member_status query filter on both
// image data sources, which is how a consumer finds a pending share by name.
func TestImageDataSourcesSendMemberStatus(t *testing.T) {
	ctx := context.Background()
	image := `{"images": [{"id": "i1", "name": "shared-img", "status": "active", "visibility": "shared", "owner": "p-owner",
		"container_format": "bare", "disk_format": "raw", "tags": [], "created_at": "2026-10-06T10:00:00Z",
		"updated_at": "2026-10-06T10:00:00Z"}]}`
	set := map[string]tftypes.Value{
		"name":          tftypes.NewValue(tftypes.String, "shared-img"),
		"visibility":    tftypes.NewValue(tftypes.String, "shared"),
		"member_status": tftypes.NewValue(tftypes.String, "pending"),
	}
	for name, d := range map[string]datasource.DataSource{
		"pcd_images_image":     &imageDataSource{},
		"pcd_images_image_ids": &imageIDsDataSource{},
	} {
		t.Run(name, func(t *testing.T) {
			g := &imageAccessGlance{imagesBody: image}
			srv := g.serve(t)
			switch ds := d.(type) {
			case *imageDataSource:
				ds.config = imageAccessTestConfig(srv.URL)
			case *imageIDsDataSource:
				ds.config = imageAccessTestConfig(srv.URL)
			}
			resp := imageDataSourceRead(ctx, t, d, set)
			if resp.Diagnostics.HasError() {
				t.Fatalf("read: %v", resp.Diagnostics)
			}
			calls := g.seen()
			if len(calls) != 1 || calls[0].path != "/v2/images" {
				t.Fatalf("calls = %+v, want one GET /v2/images", calls)
			}
			params := strings.Split(calls[0].query, "&")
			for _, want := range []string{"member_status=pending", "visibility=shared", "name=shared-img"} {
				if !slices.Contains(params, want) {
					t.Fatalf("query %q lacks %s", calls[0].query, want)
				}
			}
		})
	}
}
