// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package compute

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/action"
	"github.com/hashicorp/terraform-plugin-framework/action/schema"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

// invokeChangeTenant runs the action against m for instance id and the target
// project, the way Terraform invokes it, and returns the response and the
// progress messages.
func invokeChangeTenant(t *testing.T, m *tenantMoveNova, id, target string) (action.InvokeResponse, []string) {
	t.Helper()
	ctx := context.Background()
	a := &instanceChangeTenantAction{config: m.config}
	var sch action.SchemaResponse
	a.Schema(ctx, action.SchemaRequest{}, &sch)
	typ := sch.Schema.Type().TerraformType(ctx).(tftypes.Object)
	cfg := tfsdk.Config{Schema: sch.Schema, Raw: tftypes.NewValue(typ, map[string]tftypes.Value{
		"instance_id": tftypes.NewValue(tftypes.String, id),
		"project_id":  tftypes.NewValue(tftypes.String, target),
		"region":      tftypes.NewValue(tftypes.String, nil),
	})}
	var events []string
	resp := action.InvokeResponse{SendProgress: func(e action.InvokeProgressEvent) { events = append(events, e.Message) }}
	a.Invoke(ctx, action.InvokeRequest{Config: cfg}, &resp)
	return resp, events
}

// instanceIDError reports whether diags hold exactly one error, on
// instance_id.
func instanceIDError(diags diag.Diagnostics) bool {
	if diags.ErrorsCount() != 1 {
		return false
	}
	withPath, ok := diags[0].(diag.DiagnosticWithPath)
	return ok && withPath.Path().Equal(path.Root("instance_id"))
}

// projectIDError reports whether diags hold exactly one error, on project_id.
func projectIDError(diags diag.Diagnostics) bool {
	if diags.ErrorsCount() != 1 {
		return false
	}
	withPath, ok := diags[0].(diag.DiagnosticWithPath)
	return ok && withPath.Path().Equal(path.Root("project_id"))
}

// changeTenantServer answers a read of srv-1 with the instance in project,
// with status.
func changeTenantServer(project, status string) http.HandlerFunc {
	return reply(http.StatusOK, fmt.Sprintf(
		`{"server": {"id": "srv-1", "name": "vm-1", "status": %q, "tenant_id": %q, "user_id": "u1"}}`, status, project))
}

// changeTenantInTurn answers each request with the next handler, the last one
// repeating. fakeNova serves one request at a time, so the count needs no
// lock of its own.
func changeTenantInTurn(handlers ...http.HandlerFunc) http.HandlerFunc {
	n := 0
	return func(w http.ResponseWriter, r *http.Request) {
		handle := handlers[min(n, len(handlers)-1)]
		n++
		handle(w, r)
	}
}

// changeTenantDropConnection closes the connection without answering, so the
// client gets a transport error rather than a status.
func changeTenantDropConnection(w http.ResponseWriter, _ *http.Request) {
	if conn, _, err := http.NewResponseController(w).Hijack(); err == nil {
		conn.Close()
	}
}

// changeTenantRecoveryParts are the parts of the "Move outcome unknown"
// recovery text every such error carries.
var changeTenantRecoveryParts = []string{
	"wait a few minutes", "pcd_compute_instance data source", "under an admin token",
	"In project p2, the move finished", "In project p1 and not in ERROR, nothing moved: run the action again",
	"In ERROR, Nova could not move every volume and port back", "refuses another move while the instance is in ERROR",
	"put each attached volume and port back in the instance's project and reset the instance's state",
}

// The action reads the instance, moves it, and confirms the new project.
func TestChangeTenantActionMovesTheInstance(t *testing.T) {
	shortPolls(t)
	m := newTenantMoveNova(t, http.StatusAccepted, "", "p1", "p2")
	resp, events := invokeChangeTenant(t, m, "srv-1", "p2")
	if resp.Diagnostics.HasError() {
		t.Fatalf("invoke: %v", resp.Diagnostics)
	}
	want := []string{"GET /servers/srv-1", "POST /servers/srv-1/action", "GET /servers/srv-1"}
	if got := m.received(); !slices.Equal(got, want) {
		t.Fatalf("requests = %v, want %v", got, want)
	}
	if got := m.actionLog(); len(got) != 1 || got[0] != `{"changeTenant":{"project_id":"p2"}} @` {
		t.Fatalf("actions = %q", got)
	}
	if len(events) < 3 || !strings.Contains(events[0], "from project p1 to project p2") ||
		events[len(events)-1] != "Instance srv-1 is in project p2" {
		t.Fatalf("progress = %q", events)
	}
}

// Re-running the action after the move sends nothing and warns, naming the
// instance's status; in ERROR, the warning says to check its volumes and
// ports.
func TestChangeTenantActionAlreadyThere(t *testing.T) {
	for _, tc := range []struct {
		status string
		check  bool
	}{{"ACTIVE", false}, {"ERROR", true}} {
		t.Run(tc.status, func(t *testing.T) {
			m := &tenantMoveNova{fakeNova: newFakeNova(t, novaRoutes{"GET /servers/srv-1": changeTenantServer("p2", tc.status)})}
			resp, _ := invokeChangeTenant(t, m, "srv-1", "p2")
			if resp.Diagnostics.HasError() || resp.Diagnostics.WarningsCount() != 1 ||
				resp.Diagnostics[0].Summary() != "Instance already in the target project" {
				t.Fatalf("diags = %v, want one warning", resp.Diagnostics)
			}
			detail := resp.Diagnostics[0].Detail()
			if !strings.Contains(detail, "status "+tc.status) {
				t.Fatalf("detail = %q, want the status %s", detail, tc.status)
			}
			if got := strings.Contains(detail, "volumes and ports may not all be in project p2"); got != tc.check {
				t.Fatalf("detail = %q; advice to check volumes and ports: %t, want %t", detail, got, tc.check)
			}
			if got := m.received(); !slices.Equal(got, []string{"GET /servers/srv-1"}) {
				t.Fatalf("requests = %v, want the read alone", got)
			}
		})
	}
}

// Nova's pre-check refusal is the error, with Nova's message and what Nova
// checks; nothing is read back.
func TestChangeTenantActionSurfacesNovaRefusal(t *testing.T) {
	m := newTenantMoveNova(t, http.StatusBadRequest,
		`{"badRequest": {"code": 400, "message": "Security group sg-1 on port pt-1 does not belong to project p2."}}`, "p1")
	resp, _ := invokeChangeTenant(t, m, "srv-1", "p2")
	if resp.Diagnostics.ErrorsCount() != 1 || resp.Diagnostics[0].Summary() != "Instance not moved" {
		t.Fatalf("diags = %v, want one error \"Instance not moved\"", resp.Diagnostics)
	}
	detail := resp.Diagnostics[0].Detail()
	if !strings.Contains(detail, "does not belong to project p2") || !strings.Contains(detail, "security groups do not move") {
		t.Fatalf("detail = %q, want Nova's message and the hint", detail)
	}
	want := []string{"GET /servers/srv-1", "POST /servers/srv-1/action", "GET /servers/srv-1"}
	if got := m.received(); !slices.Equal(got, want) {
		t.Fatalf("requests = %v, want %v", got, want)
	}
}

// A refusal after which the instance is in ERROR, or cannot be read, does not
// prove that nothing moved: Nova rolls back after a failure part way, and
// sets ERROR when the rollback fails. The outcome is unknown.
func TestChangeTenantActionRefusalNotConfirmed(t *testing.T) {
	for name, tc := range map[string]struct {
		reread http.HandlerFunc
		want   []string
	}{
		"error":           {changeTenantServer("p1", "ERROR"), []string{"now reports project p1, status ERROR"}},
		"another project": {changeTenantServer("p3", "ACTIVE"), []string{"now reports project p3, status ACTIVE"}},
		"read fails": {reply(http.StatusInternalServerError, `{"computeFault": {"code": 500, "message": "Unexpected API Error."}}`),
			[]string{"could not be read afterward", "Unexpected API Error"}},
	} {
		t.Run(name, func(t *testing.T) {
			m := &tenantMoveNova{fakeNova: newFakeNova(t, novaRoutes{
				"GET /servers/srv-1": changeTenantInTurn(changeTenantServer("p1", "ACTIVE"), tc.reread),
				"POST /servers/srv-1/action": reply(http.StatusBadRequest,
					`{"badRequest": {"code": 400, "message": "Failed to transfer port pt-1."}}`),
			})}
			resp, _ := invokeChangeTenant(t, m, "srv-1", "p2")
			if resp.Diagnostics.ErrorsCount() != 1 || resp.Diagnostics[0].Summary() != "Move outcome unknown" {
				t.Fatalf("diags = %v, want one error \"Move outcome unknown\"", resp.Diagnostics)
			}
			detail := resp.Diagnostics[0].Detail()
			for _, part := range append(append([]string{"Failed to transfer port pt-1"}, tc.want...), changeTenantRecoveryParts...) {
				if !strings.Contains(detail, part) {
					t.Fatalf("detail = %q, want it to contain %q", detail, part)
				}
			}
			want := []string{"GET /servers/srv-1", "POST /servers/srv-1/action", "GET /servers/srv-1"}
			if got := m.received(); !slices.Equal(got, want) {
				t.Fatalf("requests = %v, want %v", got, want)
			}
		})
	}
}

// An instance that was in ERROR before the move is refused by Nova before it
// touches anything; still in ERROR in its project afterward, nothing moved.
func TestChangeTenantActionRefusalOfInstanceInError(t *testing.T) {
	m := &tenantMoveNova{fakeNova: newFakeNova(t, novaRoutes{
		"GET /servers/srv-1": changeTenantServer("p1", "ERROR"),
		"POST /servers/srv-1/action": reply(http.StatusConflict,
			`{"conflict": {"code": 409, "message": "Cannot 'changeTenant' instance srv-1 while it is in vm_state error"}}`),
	})}
	resp, _ := invokeChangeTenant(t, m, "srv-1", "p2")
	if resp.Diagnostics.ErrorsCount() != 1 || resp.Diagnostics[0].Summary() != "Instance not moved" {
		t.Fatalf("diags = %v, want one error \"Instance not moved\"", resp.Diagnostics)
	}
	if detail := resp.Diagnostics[0].Detail(); !strings.Contains(detail, "vm_state error") || !strings.Contains(detail, "not locked") {
		t.Fatalf("detail = %q, want Nova's message and the 409 hint", detail)
	}
	want := []string{"GET /servers/srv-1", "POST /servers/srv-1/action", "GET /servers/srv-1"}
	if got := m.received(); !slices.Equal(got, want) {
		t.Fatalf("requests = %v, want %v", got, want)
	}
}

// A failed request that is no Nova refusal (a server error, a gateway's
// answer, a dropped connection) may come after Nova moved the instance, so
// the action does not claim it did not move: it says the outcome is unknown
// and how to find out.
func TestChangeTenantActionMoveOutcomeUnknown(t *testing.T) {
	for name, tc := range map[string]struct {
		post http.HandlerFunc
		want string
	}{
		"500":                {reply(http.StatusInternalServerError, `{"computeFault": {"code": 500, "message": "Unexpected API Error."}}`), "500"},
		"502":                {badGateway, "502"},
		"503":                {reply(http.StatusServiceUnavailable, ""), "503"},
		"504":                {reply(http.StatusGatewayTimeout, ""), "504"},
		"dropped connection": {changeTenantDropConnection, "EOF"},
	} {
		t.Run(name, func(t *testing.T) {
			m := &tenantMoveNova{fakeNova: newFakeNova(t, novaRoutes{
				"GET /servers/srv-1":         changeTenantServer("p1", "ACTIVE"),
				"POST /servers/srv-1/action": tc.post,
			})}
			resp, _ := invokeChangeTenant(t, m, "srv-1", "p2")
			if resp.Diagnostics.ErrorsCount() != 1 || resp.Diagnostics[0].Summary() != "Move outcome unknown" {
				t.Fatalf("diags = %v, want one error \"Move outcome unknown\"", resp.Diagnostics)
			}
			detail := resp.Diagnostics[0].Detail()
			for _, part := range append([]string{tc.want, "may have moved the instance"}, changeTenantRecoveryParts...) {
				if !strings.Contains(detail, part) {
					t.Fatalf("detail = %q, want it to contain %q", detail, part)
				}
			}
			if got := m.received(); !slices.Equal(got, []string{"GET /servers/srv-1", "POST /servers/srv-1/action"}) {
				t.Fatalf("requests = %v", got)
			}
		})
	}
}

// A 202 whose move never shows up fails the action.
func TestChangeTenantActionMoveNotConfirmed(t *testing.T) {
	shortPolls(t)
	old := changeTenantConfirmTimeout
	changeTenantConfirmTimeout = 200 * time.Millisecond
	t.Cleanup(func() { changeTenantConfirmTimeout = old })
	m := newTenantMoveNova(t, http.StatusAccepted, "", "p1")
	resp, _ := invokeChangeTenant(t, m, "srv-1", "p2")
	if resp.Diagnostics.ErrorsCount() != 1 || resp.Diagnostics[0].Summary() != "Move not confirmed" ||
		!strings.Contains(resp.Diagnostics[0].Detail(), "still reports project p1, not p2") {
		t.Fatalf("diags = %v", resp.Diagnostics)
	}
}

// A read that fails after Nova accepted the move ends the wait at once: the
// error says Nova had accepted the move, carries the read's error, and says
// what to do next.
func TestChangeTenantActionMoveNotConfirmedReadFails(t *testing.T) {
	shortPolls(t)
	m := &tenantMoveNova{fakeNova: newFakeNova(t, novaRoutes{
		"GET /servers/srv-1": changeTenantInTurn(changeTenantServer("p1", "ACTIVE"),
			reply(http.StatusInternalServerError, `{"computeFault": {"code": 500, "message": "Unexpected API Error."}}`)),
		"POST /servers/srv-1/action": reply(http.StatusAccepted, ""),
	})}
	resp, _ := invokeChangeTenant(t, m, "srv-1", "p2")
	if resp.Diagnostics.ErrorsCount() != 1 || resp.Diagnostics[0].Summary() != "Move not confirmed" {
		t.Fatalf("diags = %v, want one error \"Move not confirmed\"", resp.Diagnostics)
	}
	detail := resp.Diagnostics[0].Detail()
	for _, part := range []string{"could not be read after Nova accepted the move", "500", "Unexpected API Error",
		"only after it has moved the instance", "under an admin token", "reports project p2, the move finished",
		"another project or is in ERROR, something changed it after Nova accepted this move",
		"\"Move outcome unknown\" in the importing guide apply"} {
		if !strings.Contains(detail, part) {
			t.Fatalf("detail = %q, want it to contain %q", detail, part)
		}
	}
	want := []string{"GET /servers/srv-1", "POST /servers/srv-1/action", "GET /servers/srv-1"}
	if got := m.received(); !slices.Equal(got, want) {
		t.Fatalf("requests = %v, want %v", got, want)
	}
}

// An instance ID that cannot go into the URL path is an error on instance_id,
// and nothing is sent.
func TestChangeTenantActionRefusesMalformedID(t *testing.T) {
	m := newTenantMoveNova(t, http.StatusAccepted, "", "p1")
	resp, _ := invokeChangeTenant(t, m, "../srv-1", "p2")
	if !instanceIDError(resp.Diagnostics) {
		t.Fatalf("diags = %v, want one error on instance_id", resp.Diagnostics)
	}
	if got := m.received(); len(got) != 0 {
		t.Fatalf("requests = %v, want none", got)
	}
}

// A project ID that is not a Keystone project ID in lower case is an error
// on project_id, and nothing is sent.
func TestChangeTenantActionRefusesMalformedProjectID(t *testing.T) {
	// Should a malformed ID get through, the wait ends soon instead of after
	// two minutes.
	shortPolls(t)
	old := changeTenantConfirmTimeout
	changeTenantConfirmTimeout = 200 * time.Millisecond
	t.Cleanup(func() { changeTenantConfirmTimeout = old })
	for _, target := range []string{"P1", "a/b"} {
		t.Run(target, func(t *testing.T) {
			m := newTenantMoveNova(t, http.StatusAccepted, "", "p1")
			resp, _ := invokeChangeTenant(t, m, "srv-1", target)
			if !projectIDError(resp.Diagnostics) || !strings.Contains(resp.Diagnostics[0].Detail(), "in lower case") {
				t.Fatalf("diags = %v, want one error on project_id asking for lower case", resp.Diagnostics)
			}
			if got := m.received(); len(got) != 0 {
				t.Fatalf("requests = %v, want none", got)
			}
		})
	}
}

// The schema's validators refuse a malformed and an empty instance_id and
// project_id, and a project_id in upper case, at plan time.
func TestChangeTenantActionValidators(t *testing.T) {
	ctx := context.Background()
	var sch action.SchemaResponse
	(&instanceChangeTenantAction{}).Schema(ctx, action.SchemaRequest{}, &sch)
	for attr, values := range map[string][]string{
		"instance_id": {"", "a/b", ".."},
		"project_id":  {"", "a/b", "P1"},
	} {
		validators := sch.Schema.Attributes[attr].(schema.StringAttribute).Validators
		for _, value := range values {
			var diags diag.Diagnostics
			for _, v := range validators {
				var resp validator.StringResponse
				v.ValidateString(ctx, validator.StringRequest{Path: path.Root(attr), ConfigValue: types.StringValue(value)}, &resp)
				diags.Append(resp.Diagnostics...)
			}
			if !diags.HasError() {
				t.Errorf("%s = %q passed the schema's validators", attr, value)
			}
		}
		var resp validator.StringResponse
		for _, v := range validators {
			v.ValidateString(ctx, validator.StringRequest{Path: path.Root(attr), ConfigValue: types.StringValue("0a1b-c_2")}, &resp)
		}
		if resp.Diagnostics.HasError() {
			t.Errorf("%s = \"0a1b-c_2\" refused: %v", attr, resp.Diagnostics)
		}
	}
}

// A 200 without the server is an error, and no move is sent.
func TestChangeTenantActionAnswerWithoutServer(t *testing.T) {
	m := &tenantMoveNova{fakeNova: newFakeNova(t, novaRoutes{
		"GET /servers/srv-1": reply(http.StatusOK, `{}`),
	})}
	resp, _ := invokeChangeTenant(t, m, "srv-1", "p2")
	if resp.Diagnostics.ErrorsCount() != 1 || resp.Diagnostics[0].Summary() != "compute: reading instance" {
		t.Fatalf("diags = %v, want one error reading the instance", resp.Diagnostics)
	}
	if got := m.received(); !slices.Equal(got, []string{"GET /servers/srv-1"}) {
		t.Fatalf("requests = %v, want the read alone", got)
	}
}

// An answer for another server is an error on instance_id that names both
// IDs, and no move is sent.
func TestChangeTenantActionAnswerForAnotherServer(t *testing.T) {
	m := &tenantMoveNova{fakeNova: newFakeNova(t, novaRoutes{
		"GET /servers/srv-1": reply(http.StatusOK,
			`{"server": {"id": "srv-2", "name": "vm-2", "status": "ACTIVE", "tenant_id": "p1", "user_id": "u1"}}`),
	})}
	resp, _ := invokeChangeTenant(t, m, "srv-1", "p2")
	if !instanceIDError(resp.Diagnostics) {
		t.Fatalf("diags = %v, want one error on instance_id", resp.Diagnostics)
	}
	if detail := resp.Diagnostics[0].Detail(); !strings.Contains(detail, "srv-1") || !strings.Contains(detail, "srv-2") ||
		strings.Contains(detail, "lower case") {
		t.Fatalf("detail = %q, want both IDs and no lower-case advice", detail)
	}
	if got := m.received(); !slices.Equal(got, []string{"GET /servers/srv-1"}) {
		t.Fatalf("requests = %v, want the read alone", got)
	}
}
