// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package compute

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
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

// quotaNova is a fake Nova that records every request with its query, and the
// body of each, and answers from routes keyed by "<METHOD> <path>?<query>".
// fakeNova (fake_nova_internal_test.go) routes on the path alone, which cannot
// tell a user's quotas from the project's: they differ only by ?user_id=.
type quotaNova struct {
	mu     sync.Mutex
	calls  []string
	bodies map[string]string
	srv    *httptest.Server
}

func newQuotaNova(t *testing.T, routes map[string]func() (int, string)) *quotaNova {
	t.Helper()
	n := &quotaNova{bodies: map[string]string{}}
	n.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Method + " " + r.URL.Path
		if r.URL.RawQuery != "" {
			key += "?" + r.URL.RawQuery
		}
		b, _ := io.ReadAll(r.Body)
		n.mu.Lock()
		n.calls = append(n.calls, key)
		n.bodies[key] = string(b)
		route, ok := routes[key]
		n.mu.Unlock()
		if !ok {
			t.Errorf("unexpected request %s", key)
			w.WriteHeader(http.StatusNotImplemented)
			return
		}
		status, body := route()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		fmt.Fprint(w, body)
	}))
	t.Cleanup(n.srv.Close)
	return n
}

// resource returns a quota set resource whose Nova client reaches n, through
// fakeConfig.
func (n *quotaNova) resource() *quotasetResource {
	return &quotasetResource{config: fakeConfig(n.srv.URL)}
}

func (n *quotaNova) requests() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]string(nil), n.calls...)
}

const quotaSetBody = `{"quota_set": {"id": "p-1", "cores": %d, "instances": 4, "ram": 16384, "fixed_ips": -1,
	"floating_ips": -1, "injected_file_content_bytes": 10240, "injected_file_path_bytes": 255, "injected_files": 5,
	"key_pairs": 100, "metadata_items": 128, "security_group_rules": -1, "security_groups": -1,
	"server_groups": 10, "server_group_members": 10}}`

func quotaPlanModel(userID types.String, cores int64) quotasetModel {
	return quotasetModel{
		ID:                       types.StringUnknown(),
		ProjectID:                types.StringValue("p-1"),
		UserID:                   userID,
		Region:                   types.StringUnknown(),
		FixedIPs:                 types.Int64Unknown(),
		FloatingIPs:              types.Int64Unknown(),
		InjectedFileContentBytes: types.Int64Unknown(),
		InjectedFilePathBytes:    types.Int64Unknown(),
		InjectedFiles:            types.Int64Unknown(),
		KeyPairs:                 types.Int64Unknown(),
		MetadataItems:            types.Int64Unknown(),
		RAM:                      types.Int64Value(16384),
		SecurityGroupRules:       types.Int64Unknown(),
		SecurityGroups:           types.Int64Unknown(),
		Cores:                    types.Int64Value(cores),
		Instances:                types.Int64Value(4),
		ServerGroups:             types.Int64Unknown(),
		ServerGroupMembers:       types.Int64Unknown(),
	}
}

// The quotas of one user live at the project's quota URL with ?user_id=; a
// project's quotas keep the URL gophercloud builds.
func TestQuotaSetURL(t *testing.T) {
	client := &gophercloud.ServiceClient{ProviderClient: &gophercloud.ProviderClient{}, Endpoint: "https://nova.example/v2.1/"}
	for _, tc := range []struct{ project, user, want string }{
		{project: "p-1", want: "https://nova.example/v2.1/os-quota-sets/p-1"},
		{project: "p-1", user: "u-1", want: "https://nova.example/v2.1/os-quota-sets/p-1?user_id=u-1"},
		{project: "p-1", user: "u 1&x", want: "https://nova.example/v2.1/os-quota-sets/p-1?user_id=u+1%26x"},
	} {
		if got := quotaSetURL(client, tc.project, tc.user); got != tc.want {
			t.Errorf("quotaSetURL(%q, %q) = %s, want %s", tc.project, tc.user, got, tc.want)
		}
	}
}

// Import accepts the three forms; IDs and imports of existing project quota
// sets keep working because user_id is appended last.
func TestSplitQuotaImportID(t *testing.T) {
	for id, want := range map[string][3]string{
		"p-1":                {"p-1", "", ""},
		"p-1/":               {"p-1", "", ""},
		"p-1/region-one":     {"p-1", "region-one", ""},
		"p-1/region-one/u-1": {"p-1", "region-one", "u-1"},
	} {
		p, r, u := splitQuotaImportID(id)
		if got := [3]string{p, r, u}; got != want {
			t.Errorf("splitQuotaImportID(%q) = %v, want %v", id, got, want)
		}
	}
	if got := quotaSetID("p-1", "region-one", ""); got != "p-1/region-one" {
		t.Errorf("project quota set id = %s, want p-1/region-one", got)
	}
	if got := quotaSetID("p-1", "region-one", "u-1"); got != "p-1/region-one/u-1" {
		t.Errorf("user quota set id = %s, want p-1/region-one/u-1", got)
	}
}

// A user quota set writes and reads at ?user_id=, sends only the fields that
// are set, records the three-part ID, and on destroy removes the user's
// overrides.
func TestQuotasetUserModeLifecycle(t *testing.T) {
	ctx := context.Background()
	const key = "/os-quota-sets/p-1?user_id=u-1"
	n := newQuotaNova(t, map[string]func() (int, string){
		"PUT " + key:    func() (int, string) { return 200, fmt.Sprintf(quotaSetBody, 8) },
		"GET " + key:    func() (int, string) { return 200, fmt.Sprintf(quotaSetBody, 8) },
		"DELETE " + key: func() (int, string) { return 202, "" },
	})
	r := n.resource()
	s := schemaOf(t, r)

	planned := quotaPlanModel(types.StringValue("u-1"), 8)
	createResp := runCreate(r, newPlan(t, s, &planned))
	if createResp.Diagnostics.HasError() {
		t.Fatalf("create: %v", createResp.Diagnostics)
	}
	var put map[string]map[string]any
	if err := json.Unmarshal([]byte(n.bodies["PUT "+key]), &put); err != nil {
		t.Fatalf("PUT body: %v", err)
	}
	if fmt.Sprint(put["quota_set"]) != "map[cores:8 instances:4 ram:16384]" {
		t.Fatalf("PUT quota_set = %v; want only the three configured fields, since each one sent pins a per-user override", put["quota_set"])
	}
	var got quotasetModel
	createResp.State.Get(ctx, &got)
	if got.ID.ValueString() != "p-1/region-one/u-1" || got.UserID.ValueString() != "u-1" || got.Cores.ValueInt64() != 8 {
		t.Fatalf("create state id=%s user_id=%s cores=%d; want p-1/region-one/u-1, u-1, 8", got.ID, got.UserID, got.Cores.ValueInt64())
	}

	readResp := runRead(r, createResp.State)
	if readResp.Diagnostics.HasError() {
		t.Fatalf("read: %v", readResp.Diagnostics)
	}

	deleteResp := runDelete(r, readResp.State)
	if deleteResp.Diagnostics.HasError() {
		t.Fatalf("delete: %v", deleteResp.Diagnostics)
	}
	want := []string{"PUT " + key, "GET " + key, "GET " + key, "DELETE " + key}
	if fmt.Sprint(n.requests()) != fmt.Sprint(want) {
		t.Fatalf("requests = %v, want %v", n.requests(), want)
	}
}

// A project quota set keeps today's behavior: no query on the URL, and a
// destroy that sends nothing, leaving the project's quotas as they are.
func TestQuotasetProjectModeKeepsQuotasOnDestroy(t *testing.T) {
	ctx := context.Background()
	const key = "/os-quota-sets/p-1"
	n := newQuotaNova(t, map[string]func() (int, string){
		"PUT " + key: func() (int, string) { return 200, fmt.Sprintf(quotaSetBody, 32) },
		"GET " + key: func() (int, string) { return 200, fmt.Sprintf(quotaSetBody, 32) },
	})
	r := n.resource()
	s := schemaOf(t, r)

	planned := quotaPlanModel(types.StringNull(), 32)
	createResp := runCreate(r, newPlan(t, s, &planned))
	if createResp.Diagnostics.HasError() {
		t.Fatalf("create: %v", createResp.Diagnostics)
	}
	var got quotasetModel
	createResp.State.Get(ctx, &got)
	if got.ID.ValueString() != "p-1/region-one" || !got.UserID.IsNull() {
		t.Fatalf("create state id=%s user_id=%v; want p-1/region-one and a null user_id", got.ID, got.UserID)
	}

	deleteResp := runDelete(r, createResp.State)
	if deleteResp.Diagnostics.HasError() {
		t.Fatalf("delete: %v", deleteResp.Diagnostics)
	}
	if want := []string{"PUT " + key, "GET " + key}; fmt.Sprint(n.requests()) != fmt.Sprint(want) {
		t.Fatalf("requests = %v, want %v (destroy must not reset a project's quotas)", n.requests(), want)
	}
}

// quotaStateModel is a quota set as Nova last reported it: every field known.
func quotaStateModel(userID types.String, cores int64) quotasetModel {
	id := "p-1/region-one"
	if !userID.IsNull() {
		id += "/" + userID.ValueString()
	}
	return quotasetModel{
		ID:                       types.StringValue(id),
		ProjectID:                types.StringValue("p-1"),
		UserID:                   userID,
		Region:                   types.StringValue("region-one"),
		FixedIPs:                 types.Int64Value(-1),
		FloatingIPs:              types.Int64Value(-1),
		InjectedFileContentBytes: types.Int64Value(10240),
		InjectedFilePathBytes:    types.Int64Value(255),
		InjectedFiles:            types.Int64Value(5),
		KeyPairs:                 types.Int64Value(100),
		MetadataItems:            types.Int64Value(128),
		RAM:                      types.Int64Value(16384),
		SecurityGroupRules:       types.Int64Value(-1),
		SecurityGroups:           types.Int64Value(-1),
		Cores:                    types.Int64Value(cores),
		Instances:                types.Int64Value(4),
		ServerGroups:             types.Int64Value(10),
		ServerGroupMembers:       types.Int64Value(10),
	}
}

// quotaConfigModel is the configuration quotaStateModel came from: cores,
// instances and ram set, everything else null.
func quotaConfigModel(userID types.String, cores int64) quotasetModel {
	return quotasetModel{
		ID:                       types.StringNull(),
		ProjectID:                types.StringValue("p-1"),
		UserID:                   userID,
		Region:                   types.StringNull(),
		FixedIPs:                 types.Int64Null(),
		FloatingIPs:              types.Int64Null(),
		InjectedFileContentBytes: types.Int64Null(),
		InjectedFilePathBytes:    types.Int64Null(),
		InjectedFiles:            types.Int64Null(),
		KeyPairs:                 types.Int64Null(),
		MetadataItems:            types.Int64Null(),
		RAM:                      types.Int64Value(16384),
		SecurityGroupRules:       types.Int64Null(),
		SecurityGroups:           types.Int64Null(),
		Cores:                    types.Int64Value(cores),
		Instances:                types.Int64Value(4),
		ServerGroups:             types.Int64Null(),
		ServerGroupMembers:       types.Int64Null(),
	}
}

// ModifyPlan plans a changed user quota set's unset fields as unknown (see
// TestQuotasetPlanUserMode for the plan itself), so an update sends only the
// changed field and records whatever Nova reports for the rest, here a
// key_pairs value the project's quota set just raised in the same apply.
func TestQuotasetUserModeUpdateFollowsTheProject(t *testing.T) {
	ctx := context.Background()
	const key = "/os-quota-sets/p-1?user_id=u-1"
	n := newQuotaNova(t, map[string]func() (int, string){
		"PUT " + key: func() (int, string) { return 200, fmt.Sprintf(quotaSetBody, 12) },
		"GET " + key: func() (int, string) {
			return 200, strings.Replace(fmt.Sprintf(quotaSetBody, 12), `"key_pairs": 100`, `"key_pairs": 200`, 1)
		},
	})
	r := n.resource()
	s := schemaOf(t, r)
	userID := types.StringValue("u-1")
	prior, configured, proposed := quotaStateModel(userID, 8), quotaConfigModel(userID, 12), quotaStateModel(userID, 12)
	state := newState(t, s, &prior)
	mpReq := resource.ModifyPlanRequest{
		Config: tfsdk.Config{Schema: s, Raw: newPlan(t, s, &configured).Raw},
		Plan:   newPlan(t, s, &proposed),
		State:  state,
	}
	mpResp := resource.ModifyPlanResponse{Plan: mpReq.Plan}
	r.ModifyPlan(ctx, mpReq, &mpResp)
	if mpResp.Diagnostics.HasError() {
		t.Fatalf("modify plan: %v", mpResp.Diagnostics)
	}

	updateResp := runUpdate(r, mpResp.Plan, state)
	if updateResp.Diagnostics.HasError() {
		t.Fatalf("update: %v", updateResp.Diagnostics)
	}
	if got := n.bodies["PUT "+key]; got != `{"quota_set":{"cores":12}}` {
		t.Fatalf("PUT body = %s; want only the changed cores", got)
	}
	var got quotasetModel
	updateResp.State.Get(ctx, &got)
	if got.KeyPairs.ValueInt64() != 200 || got.Cores.ValueInt64() != 12 {
		t.Fatalf("state key_pairs=%d cores=%d; want the project's new 200 and 12", got.KeyPairs.ValueInt64(), got.Cores.ValueInt64())
	}
	// Terraform core fails an apply whose result differs from a known planned value.
	var planned quotasetModel
	mpResp.Plan.Get(ctx, &planned)
	if !planned.KeyPairs.IsUnknown() && !planned.KeyPairs.Equal(got.KeyPairs) {
		t.Fatalf("planned key_pairs %v, applied %v: Terraform would report an inconsistent result", planned.KeyPairs, got.KeyPairs)
	}
}

// Import refuses an ID whose user part is empty ("p-1/region-one/") or holds
// another slash ("p-1/region-one/u-1/x"): the first would import the project's
// quota set under a resource meant for a user, the second a user Nova does not
// check. The project and legacy forms still import.
func TestQuotasetImportRefusesAnEmptyOrSlashedUser(t *testing.T) {
	ctx := context.Background()
	for id, wantUser := range map[string]string{
		"p-1/region-one/":      "error",
		"p-1/region-one/u-1/x": "error",
		"p-1/region-one/u-1":   "u-1",
		"p-1/region-one":       "",
		"p-1/":                 "",
		"p-1":                  "",
	} {
		t.Run(id, func(t *testing.T) {
			resp := importQuotaset(t, &quotasetResource{}, id)
			if wantUser == "error" {
				var got []string
				for _, d := range resp.Diagnostics.Errors() {
					got = append(got, d.Summary())
				}
				if fmt.Sprint(got) != "[Invalid import ID]" || !resp.State.Raw.IsNull() {
					t.Fatalf("import errors = %v, state null %v; want [Invalid import ID] and no state", got, resp.State.Raw.IsNull())
				}
				return
			}
			if resp.Diagnostics.HasError() {
				t.Fatalf("import: %v", resp.Diagnostics)
			}
			var got quotasetModel
			resp.State.Get(ctx, &got)
			if got.ProjectID.ValueString() != "p-1" || got.UserID.ValueString() != wantUser {
				t.Fatalf("imported project_id=%s user_id=%s; want p-1, %q", got.ProjectID, got.UserID, wantUser)
			}
		})
	}
}
