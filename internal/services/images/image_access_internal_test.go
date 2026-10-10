// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package images

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"

	"github.com/platform9/terraform-provider-pcd/internal/clients"
)

// imageAccessCall is one request the fake Glance saw.
type imageAccessCall struct {
	method, path, query string
	body                map[string]any
}

// imageAccessGlance answers the member endpoints of image i1, the image itself
// and Keystone's token read. Each response is a status and a body; a zero
// status answers 200 with the body. It records every call so tests can assert
// what was sent.
type imageAccessGlance struct {
	mu sync.Mutex

	createCode int // POST /v2/images/i1/members
	createBody string
	getCode    int // GET /v2/images/i1/members/<member>
	getBody    string
	putCode    int // PUT /v2/images/i1/members/<member>
	deleteCode int // DELETE /v2/images/i1/members/<member>
	listCode   int // GET /v2/images/i1/members
	listBody   string
	tokenBody  string // GET /v3/auth/tokens
	imagesBody string // GET /v2/images
	imageCode  int    // GET /v2/images/i1
	imageBody  string

	calls []imageAccessCall
}

func imageMemberJSON(member, status string) string {
	return fmt.Sprintf(`{"image_id": "i1", "member_id": %q, "status": %q, "schema": "/v2/schemas/member",
		"created_at": "2026-10-06T10:00:00Z", "updated_at": "2026-10-06T10:05:00Z"}`, member, status)
}

// imageAccessImageJSON is image i1 with the given visibility.
func imageAccessImageJSON(visibility string) string {
	return fmt.Sprintf(`{"id": "i1", "name": "shared-img", "status": "active", "visibility": %q, "owner": "p-owner",
		"container_format": "bare", "disk_format": "raw", "tags": [], "created_at": "2026-10-06T10:00:00Z",
		"updated_at": "2026-10-06T10:00:00Z"}`, visibility)
}

func (g *imageAccessGlance) serve(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		defer g.mu.Unlock()
		call := imageAccessCall{method: r.Method, path: r.URL.Path, query: r.URL.RawQuery}
		if raw, _ := io.ReadAll(r.Body); len(raw) > 0 {
			if err := json.Unmarshal(raw, &call.body); err != nil {
				t.Errorf("%s %s body is not JSON: %s", r.Method, r.URL.Path, raw)
			}
		}
		g.calls = append(g.calls, call)
		w.Header().Set("Content-Type", "application/json")
		reply := func(code int, body string) {
			if code == 0 {
				code = http.StatusOK
			}
			w.WriteHeader(code)
			if code >= 400 {
				fmt.Fprintf(w, `{"code": %d, "title": "refused"}`, code)
				return
			}
			fmt.Fprint(w, body)
		}
		const memberPath = "/v2/images/i1/members/"
		member, isMember := strings.CutPrefix(r.URL.Path, memberPath)
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v2/images/i1/members":
			reply(g.createCode, g.createBody)
		case r.Method == http.MethodGet && r.URL.Path == "/v2/images/i1/members":
			reply(g.listCode, g.listBody)
		case r.Method == http.MethodGet && isMember:
			reply(g.getCode, g.getBody)
		case r.Method == http.MethodPut && isMember:
			status, _ := call.body["status"].(string)
			reply(g.putCode, imageMemberJSON(member, status))
		case r.Method == http.MethodDelete && isMember:
			code := g.deleteCode
			if code == 0 {
				code = http.StatusNoContent
			}
			w.WriteHeader(code)
		case r.Method == http.MethodGet && r.URL.Path == "/v3/auth/tokens":
			reply(0, g.tokenBody)
		case r.Method == http.MethodGet && r.URL.Path == "/v2/images":
			reply(0, g.imagesBody)
		case r.Method == http.MethodGet && r.URL.Path == "/v2/images/i1":
			reply(g.imageCode, g.imageBody)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotImplemented)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func (g *imageAccessGlance) seen() []imageAccessCall {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]imageAccessCall(nil), g.calls...)
}

// count returns how many calls had this method and path.
func (g *imageAccessGlance) count(method, path string) int {
	n := 0
	for _, c := range g.seen() {
		if c.method == method && c.path == path {
			n++
		}
	}
	return n
}

// imageAccessTestConfig points the resources' clients at the fake. Its client
// gets a transport of its own, not http.DefaultTransport: every
// httptest.Server.Close closes the default transport's idle connections,
// which can cut a request another parallel test is sending.
func imageAccessTestConfig(url string) *clients.Config {
	return &clients.Config{
		Region: "region-one",
		Provider: &gophercloud.ProviderClient{
			HTTPClient:      http.Client{Transport: &http.Transport{}},
			EndpointLocator: func(gophercloud.EndpointOpts) (string, error) { return url + "/", nil },
		},
	}
}

func imageAccessSchema(ctx context.Context, t *testing.T, r resource.Resource) schema.Schema {
	t.Helper()
	var sch resource.SchemaResponse
	r.Schema(ctx, resource.SchemaRequest{}, &sch)
	if sch.Diagnostics.HasError() {
		t.Fatalf("schema: %v", sch.Diagnostics)
	}
	return sch.Schema
}

// imageAccessPlan is the plan a first apply produces for image i1, with the given
// member_id and status (null or unknown where the configuration omits them).
func imageAccessPlan(member, status types.String) imageAccessModel {
	return imageAccessModel{
		ID:        types.StringUnknown(),
		ImageID:   types.StringValue("i1"),
		MemberID:  member,
		Status:    status,
		CreatedAt: types.StringUnknown(),
		UpdatedAt: types.StringUnknown(),
		Schema:    types.StringUnknown(),
		Region:    types.StringUnknown(),
	}
}

func imageAccessCreate(ctx context.Context, t *testing.T, r resource.Resource, planned imageAccessModel) resource.CreateResponse {
	t.Helper()
	s := imageAccessSchema(ctx, t, r)
	plan := tfsdk.Plan{Schema: s, Raw: tftypes.NewValue(s.Type().TerraformType(ctx), nil)}
	if d := plan.Set(ctx, &planned); d.HasError() {
		t.Fatalf("building the plan: %v", d)
	}
	resp := resource.CreateResponse{State: tfsdk.State{Schema: s, Raw: tftypes.NewValue(s.Type().TerraformType(ctx), nil)}}
	r.Create(ctx, resource.CreateRequest{Plan: plan}, &resp)
	return resp
}

// imageAccessState is a stored membership of project p1 in image i1.
func imageAccessState(ctx context.Context, t *testing.T, r resource.Resource, status string) tfsdk.State {
	t.Helper()
	s := imageAccessSchema(ctx, t, r)
	st := tfsdk.State{Schema: s, Raw: tftypes.NewValue(s.Type().TerraformType(ctx), nil)}
	if d := st.Set(ctx, &imageAccessModel{
		ID:        types.StringValue("i1/p1"),
		ImageID:   types.StringValue("i1"),
		MemberID:  types.StringValue("p1"),
		Status:    types.StringValue(status),
		CreatedAt: types.StringValue("2026-10-06T10:00:00Z"),
		UpdatedAt: types.StringValue("2026-10-06T10:05:00Z"),
		Schema:    types.StringValue("/v2/schemas/member"),
		Region:    types.StringValue("region-one"),
	}); d.HasError() {
		t.Fatalf("building the state: %v", d)
	}
	return st
}

func imageAccessStateModel(ctx context.Context, t *testing.T, st tfsdk.State) imageAccessModel {
	t.Helper()
	var m imageAccessModel
	if d := st.Get(ctx, &m); d.HasError() {
		t.Fatalf("reading state: %v", d)
	}
	return m
}

// Sharing sends the UI's body and, with status unset, leaves the decision to
// the member: no status change follows and state reports pending.
func TestImageAccessCreateShares(t *testing.T) {
	ctx := context.Background()
	g := &imageAccessGlance{createBody: imageMemberJSON("p1", "pending")}
	srv := g.serve(t)
	r := &imageAccessResource{config: imageAccessTestConfig(srv.URL)}

	resp := imageAccessCreate(ctx, t, r, imageAccessPlan(types.StringValue("p1"), types.StringUnknown()))
	if resp.Diagnostics.HasError() {
		t.Fatalf("create: %v", resp.Diagnostics)
	}
	calls := g.seen()
	if len(calls) != 1 || calls[0].body["member"] != "p1" || len(calls[0].body) != 1 {
		t.Fatalf("calls = %+v, want one POST with body {\"member\": \"p1\"}", calls)
	}
	got := imageAccessStateModel(ctx, t, resp.State)
	if got.ID.ValueString() != "i1/p1" || got.Status.ValueString() != "pending" || got.Region.ValueString() != "region-one" {
		t.Fatalf("state id=%s status=%s region=%s, want i1/p1, pending, region-one", got.ID, got.Status, got.Region)
	}
	if !resp.State.Raw.IsFullyKnown() {
		t.Fatalf("state holds unknown values: %v", resp.State.Raw)
	}
}

// An admin-set status is applied right after the share.
func TestImageAccessCreateSetsStatus(t *testing.T) {
	ctx := context.Background()
	g := &imageAccessGlance{createBody: imageMemberJSON("p1", "pending")}
	srv := g.serve(t)
	r := &imageAccessResource{config: imageAccessTestConfig(srv.URL)}

	resp := imageAccessCreate(ctx, t, r, imageAccessPlan(types.StringValue("p1"), types.StringValue("accepted")))
	if resp.Diagnostics.HasError() {
		t.Fatalf("create: %v", resp.Diagnostics)
	}
	calls := g.seen()
	if len(calls) != 2 || calls[1].method != http.MethodPut || calls[1].path != "/v2/images/i1/members/p1" || calls[1].body["status"] != "accepted" {
		t.Fatalf("calls = %+v, want POST then PUT /v2/images/i1/members/p1 {\"status\": \"accepted\"}", calls)
	}
	if got := imageAccessStateModel(ctx, t, resp.State); got.Status.ValueString() != "accepted" {
		t.Fatalf("state status = %s, want accepted", got.Status)
	}
}

// A refused status change leaves the member in state, so Terraform taints it
// instead of losing track of a member Glance kept.
func TestImageAccessCreateKeepsMemberWhenStatusFails(t *testing.T) {
	ctx := context.Background()
	g := &imageAccessGlance{createBody: imageMemberJSON("p1", "pending"), putCode: http.StatusForbidden}
	srv := g.serve(t)
	r := &imageAccessResource{config: imageAccessTestConfig(srv.URL)}

	resp := imageAccessCreate(ctx, t, r, imageAccessPlan(types.StringValue("p1"), types.StringValue("accepted")))
	if !resp.Diagnostics.HasError() {
		t.Fatal("create succeeded although the status change was refused")
	}
	if resp.State.Raw.IsNull() {
		t.Fatal("create returned no state: the member Glance kept is lost to Terraform")
	}
	got := imageAccessStateModel(ctx, t, resp.State)
	if got.ID.ValueString() != "i1/p1" {
		t.Fatalf("recorded id = %s, want i1/p1", got.ID)
	}
	if got.Status.ValueString() != "pending" {
		t.Fatalf("recorded status = %s, want Glance's pending, not the planned accepted", got.Status)
	}
}

// A membership left behind (the image went private and back) answers 409; it
// is adopted with Glance's record.
func TestImageAccessCreateAdoptsAnExistingMember(t *testing.T) {
	ctx := context.Background()
	g := &imageAccessGlance{createCode: http.StatusConflict, getBody: imageMemberJSON("p1", "accepted")}
	srv := g.serve(t)
	r := &imageAccessResource{config: imageAccessTestConfig(srv.URL)}

	resp := imageAccessCreate(ctx, t, r, imageAccessPlan(types.StringValue("p1"), types.StringUnknown()))
	if resp.Diagnostics.HasError() {
		t.Fatalf("create with an existing member: %v", resp.Diagnostics)
	}
	if got := imageAccessStateModel(ctx, t, resp.State); got.Status.ValueString() != "accepted" {
		t.Fatalf("adopted status = %s, want Glance's accepted", got.Status)
	}
}

// Glance's 403 on create does not say why; the provider's message does.
func TestImageAccessCreateExplainsForbidden(t *testing.T) {
	ctx := context.Background()
	g := &imageAccessGlance{createCode: http.StatusForbidden}
	srv := g.serve(t)
	r := &imageAccessResource{config: imageAccessTestConfig(srv.URL)}

	resp := imageAccessCreate(ctx, t, r, imageAccessPlan(types.StringValue("p1"), types.StringUnknown()))
	if !resp.Diagnostics.HasError() {
		t.Fatal("create succeeded on a 403")
	}
	if summary := resp.Diagnostics.Errors()[0].Summary(); summary != "Image cannot be shared" {
		t.Fatalf("summary = %q, want %q (acceptance tests match it)", summary, "Image cannot be shared")
	}
	if !resp.State.Raw.IsNull() {
		t.Fatal("a refused share left state behind")
	}
}

// Read follows the member's decision when status is unmanaged, drops a
// membership Glance no longer shows, and keeps state on other errors,
// including a 200 whose body holds no member record: that is not a
// not-found, and copying it would save empty IDs.
func TestImageAccessRead(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name        string
		code        int
		body        string // the member record; "" answers p1 accepted
		wantStatus  string
		wantRemoved bool
		wantErr     bool
	}{
		{name: "member accepted", code: http.StatusOK, wantStatus: "accepted"},
		{name: "gone or no longer shared", code: http.StatusNotFound, wantRemoved: true},
		{name: "server error", code: http.StatusInternalServerError, wantErr: true},
		{name: "answer without the member", code: http.StatusOK, body: "{}", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := tc.body
			if body == "" {
				body = imageMemberJSON("p1", "accepted")
			}
			g := &imageAccessGlance{getCode: tc.code, getBody: body}
			srv := g.serve(t)
			r := &imageAccessResource{config: imageAccessTestConfig(srv.URL)}
			st := imageAccessState(ctx, t, r, "pending")
			resp := resource.ReadResponse{State: st}
			r.Read(ctx, resource.ReadRequest{State: st}, &resp)
			if got := resp.Diagnostics.HasError(); got != tc.wantErr {
				t.Fatalf("read error = %v, want %v: %v", got, tc.wantErr, resp.Diagnostics)
			}
			if got := resp.State.Raw.IsNull(); got != tc.wantRemoved {
				t.Fatalf("removed = %v, want %v", got, tc.wantRemoved)
			}
			if tc.wantStatus != "" {
				if got := imageAccessStateModel(ctx, t, resp.State); got.Status.ValueString() != tc.wantStatus {
					t.Fatalf("status = %s, want %s", got.Status, tc.wantStatus)
				}
			}
		})
	}
}

// imageAccessUpdatePlan is the plan for changing the stored membership's
// status to status (unknown when the configuration leaves it unset):
// updated_at is unknown, the other attributes keep their stored values.
func imageAccessUpdatePlan(status types.String) imageAccessModel {
	return imageAccessModel{
		ID:        types.StringValue("i1/p1"),
		ImageID:   types.StringValue("i1"),
		MemberID:  types.StringValue("p1"),
		Status:    status,
		CreatedAt: types.StringValue("2026-10-06T10:00:00Z"),
		UpdatedAt: types.StringUnknown(),
		Schema:    types.StringValue("/v2/schemas/member"),
		Region:    types.StringValue("region-one"),
	}
}

// imageAccessUpdate runs Update from the stored membership of p1 (status
// pending, last changed at 10:01, before Glance's 10:05 answer) to planned.
// The response starts from the prior state, as the framework's does, so an
// Update that fails without writing state leaves the prior state in place.
func imageAccessUpdate(ctx context.Context, t *testing.T, r resource.Resource, planned imageAccessModel) (tfsdk.State, resource.UpdateResponse) {
	t.Helper()
	prior := imageAccessState(ctx, t, r, "pending")
	if d := prior.SetAttribute(ctx, path.Root("updated_at"), "2026-10-06T10:01:00Z"); d.HasError() {
		t.Fatalf("building the prior state: %v", d)
	}
	s := imageAccessSchema(ctx, t, r)
	plan := tfsdk.Plan{Schema: s, Raw: tftypes.NewValue(s.Type().TerraformType(ctx), nil)}
	if d := plan.Set(ctx, &planned); d.HasError() {
		t.Fatalf("building the plan: %v", d)
	}
	resp := resource.UpdateResponse{State: tfsdk.State{Schema: s, Raw: prior.Raw.Copy()}}
	r.Update(ctx, resource.UpdateRequest{Plan: plan, State: prior}, &resp)
	return prior, resp
}

// Update sends the planned status and records Glance's answer, updated_at
// included. With status unset there is nothing to send, so it reads the
// member instead; there is no other read after a PUT. A refused change is an
// error and leaves state as it was.
func TestImageAccessUpdate(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name       string
		status     types.String // the planned status
		putCode    int
		wantMethod string // the one member call Update sends
		wantStatus string // in state afterward; "" when the update fails
	}{
		{name: "status set", status: types.StringValue("accepted"), wantMethod: http.MethodPut, wantStatus: "accepted"},
		{name: "status unset", status: types.StringUnknown(), wantMethod: http.MethodGet, wantStatus: "rejected"},
		{name: "refused", status: types.StringValue("accepted"), putCode: http.StatusForbidden, wantMethod: http.MethodPut},
		{name: "server error", status: types.StringValue("rejected"), putCode: http.StatusInternalServerError, wantMethod: http.MethodPut},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := &imageAccessGlance{putCode: tc.putCode, getBody: imageMemberJSON("p1", "rejected")}
			srv := g.serve(t)
			r := &imageAccessResource{config: imageAccessTestConfig(srv.URL)}
			prior, resp := imageAccessUpdate(ctx, t, r, imageAccessUpdatePlan(tc.status))

			calls := g.seen()
			if len(calls) != 1 || calls[0].method != tc.wantMethod || calls[0].path != "/v2/images/i1/members/p1" {
				t.Fatalf("calls = %+v, want one %s /v2/images/i1/members/p1", calls, tc.wantMethod)
			}
			if tc.wantMethod == http.MethodPut && (calls[0].body["status"] != tc.status.ValueString() || len(calls[0].body) != 1) {
				t.Fatalf("PUT body = %v, want {\"status\": %q}", calls[0].body, tc.status.ValueString())
			}
			if tc.wantStatus == "" {
				if !resp.Diagnostics.HasError() {
					t.Fatal("update succeeded although Glance refused the change")
				}
				if !resp.State.Raw.Equal(prior.Raw) {
					t.Fatalf("a failed update changed state to %v", resp.State.Raw)
				}
				return
			}
			if resp.Diagnostics.HasError() {
				t.Fatalf("update: %v", resp.Diagnostics)
			}
			got := imageAccessStateModel(ctx, t, resp.State)
			if got.Status.ValueString() != tc.wantStatus || got.UpdatedAt.ValueString() != "2026-10-06T10:05:00Z" {
				t.Fatalf("state status=%s updated_at=%s, want %s and Glance's 2026-10-06T10:05:00Z",
					got.Status, got.UpdatedAt, tc.wantStatus)
			}
			if got.ID.ValueString() != "i1/p1" || got.Region.ValueString() != "region-one" || !resp.State.Raw.IsFullyKnown() {
				t.Fatalf("state id=%s region=%s fully known=%v, want i1/p1, region-one, true",
					got.ID, got.Region, resp.State.Raw.IsFullyKnown())
			}
		})
	}
}

// Delete removes the member; a member already gone is success. A 403 is a
// warning only when the image is no longer shared (or gone), so it cannot
// block a destroy; a 403 on an image that is still shared is the caller's
// project being refused, and the membership stays, so it is an error.
func TestImageAccessDelete(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name       string
		code       int
		imageCode  int    // GET /v2/images/i1, read after a 403
		visibility string // the image's visibility in that read
		wantErr    bool
		wantWarn   bool
	}{
		{name: "removed", code: http.StatusNoContent},
		{name: "already gone", code: http.StatusNotFound},
		{name: "no longer shared", code: http.StatusForbidden, visibility: "private", wantWarn: true},
		{name: "image deleted since", code: http.StatusForbidden, imageCode: http.StatusNotFound, wantWarn: true},
		{name: "still shared", code: http.StatusForbidden, visibility: "shared", wantErr: true},
		{name: "server error", code: http.StatusInternalServerError, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := &imageAccessGlance{deleteCode: tc.code, imageCode: tc.imageCode, imageBody: imageAccessImageJSON(tc.visibility)}
			srv := g.serve(t)
			r := &imageAccessResource{config: imageAccessTestConfig(srv.URL)}
			st := imageAccessState(ctx, t, r, "accepted")
			resp := resource.DeleteResponse{State: st}
			r.Delete(ctx, resource.DeleteRequest{State: st}, &resp)
			if got := resp.Diagnostics.HasError(); got != tc.wantErr {
				t.Fatalf("delete error = %v, want %v: %v", got, tc.wantErr, resp.Diagnostics)
			}
			if got := resp.Diagnostics.WarningsCount() > 0; got != tc.wantWarn {
				t.Fatalf("delete warning = %v, want %v: %v", got, tc.wantWarn, resp.Diagnostics)
			}
			if n := g.count(http.MethodDelete, "/v2/images/i1/members/p1"); n != 1 {
				t.Fatalf("DELETE sent %d times, want 1", n)
			}
			wantImageReads := 0
			if tc.code == http.StatusForbidden {
				wantImageReads = 1
			}
			if n := g.count(http.MethodGet, "/v2/images/i1"); n != wantImageReads {
				t.Fatalf("image read %d times, want %d", n, wantImageReads)
			}
		})
	}
}

func TestImageAccessImportState(t *testing.T) {
	ctx := context.Background()
	r := &imageAccessResource{}
	s := imageAccessSchema(ctx, t, r)
	resp := resource.ImportStateResponse{State: tfsdk.State{Schema: s, Raw: tftypes.NewValue(s.Type().TerraformType(ctx), nil)}}
	r.ImportState(ctx, resource.ImportStateRequest{ID: "i1/p1"}, &resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("import i1/p1: %v", resp.Diagnostics)
	}
	if got := imageAccessStateModel(ctx, t, resp.State); got.ImageID.ValueString() != "i1" || got.MemberID.ValueString() != "p1" {
		t.Fatalf("imported image_id=%s member_id=%s, want i1, p1", got.ImageID, got.MemberID)
	}
	for _, bad := range []string{"i1", "/p1", "i1/", "i1/p1/x", ""} {
		resp := resource.ImportStateResponse{State: tfsdk.State{Schema: s, Raw: tftypes.NewValue(s.Type().TerraformType(ctx), nil)}}
		r.ImportState(ctx, resource.ImportStateRequest{ID: bad}, &resp)
		if !resp.Diagnostics.HasError() {
			t.Errorf("import %q accepted; want an error", bad)
		}
	}
}
