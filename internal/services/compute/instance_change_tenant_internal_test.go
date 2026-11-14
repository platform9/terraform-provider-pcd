// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package compute

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"
)

// tenantMoveNova is Nova for instance srv-1: GET reports the instance in
// projects[0], then each later project in turn (the last one repeats), and
// POST /action answers postStatus with postBody, recording each body and its
// microversion header.
type tenantMoveNova struct {
	*fakeNova
	actions []string
}

func newTenantMoveNova(t *testing.T, postStatus int, postBody string, projects ...string) *tenantMoveNova {
	t.Helper()
	m := &tenantMoveNova{}
	gets := 0
	m.fakeNova = newFakeNova(t, novaRoutes{
		"GET /servers/srv-1": func(w http.ResponseWriter, _ *http.Request) {
			project := projects[min(gets, len(projects)-1)]
			gets++
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"server": {"id": "srv-1", "name": "vm-1", "status": "ACTIVE", "tenant_id": %q, "user_id": "u1"}}`, project)
		},
		"POST /servers/srv-1/action": func(w http.ResponseWriter, r *http.Request) {
			raw, _ := io.ReadAll(r.Body)
			m.actions = append(m.actions, string(raw)+" @"+r.Header.Get("OpenStack-API-Version"))
			reply(postStatus, postBody)(w, r)
		},
	})
	return m
}

// actionLog returns the recorded actions; reading the request log first
// orders it after the server's writes.
func (m *tenantMoveNova) actionLog() []string {
	m.received()
	return append([]string(nil), m.actions...)
}

// The body is the PCD UI's, at the base microversion.
func TestMoveInstanceToProjectSendsTheUIBody(t *testing.T) {
	m := newTenantMoveNova(t, http.StatusAccepted, "", "p1")
	client, err := m.config.ComputeV2Client()
	if err != nil {
		t.Fatal(err)
	}
	if err := moveInstanceToProject(context.Background(), client, "srv-1", "p2"); err != nil {
		t.Fatal(err)
	}
	if got := m.actionLog(); !slices.Equal(got, []string{`{"changeTenant":{"project_id":"p2"}} @`}) {
		t.Fatalf("actions = %q; want one changeTenant with no microversion header", got)
	}
}

// Nova's refusal keeps Nova's message, and the hint names what Nova checks
// for that status.
func TestMoveInstanceToProjectRefusals(t *testing.T) {
	for _, tc := range []struct {
		status          int
		body, msg, hint string
	}{
		{http.StatusBadRequest, `{"badRequest": {"code": 400, "message": "Instance has floating IPs associated; disassociate them first."}}`,
			"floating IPs associated", "has no floating IP"},
		{http.StatusConflict, `{"conflict": {"code": 409, "message": "Instance srv-1 is locked"}}`,
			"is locked", "not locked"},
		{http.StatusForbidden, `{"forbidden": {"code": 403, "message": "Quota exceeded for instances"}}`,
			"Quota exceeded", "needs an admin token"},
		{http.StatusNotFound, `{"itemNotFound": {"code": 404, "message": "Project p9 could not be found."}}`,
			"could not be found", "no such project"},
	} {
		t.Run(http.StatusText(tc.status), func(t *testing.T) {
			m := newTenantMoveNova(t, tc.status, tc.body, "p1")
			client, err := m.config.ComputeV2Client()
			if err != nil {
				t.Fatal(err)
			}
			err = moveInstanceToProject(context.Background(), client, "srv-1", "p9")
			if err == nil || !strings.Contains(err.Error(), tc.msg) {
				t.Fatalf("err = %v, want Nova's message %q", err, tc.msg)
			}
			if hint := changeTenantRefusalHint(err); !strings.Contains(hint, tc.hint) {
				t.Fatalf("hint = %q, want it to mention %q", hint, tc.hint)
			}
		})
	}
	t.Run("no such action", func(t *testing.T) {
		m := newTenantMoveNova(t, http.StatusBadRequest,
			`{"badRequest": {"code": 400, "message": "There is no such action: changeTenant"}}`, "p1")
		client, err := m.config.ComputeV2Client()
		if err != nil {
			t.Fatal(err)
		}
		err = moveInstanceToProject(context.Background(), client, "srv-1", "p9")
		hint := changeTenantRefusalHint(err)
		if !strings.Contains(hint, "does not have the changeTenant action") || strings.Contains(hint, "floating IP") {
			t.Fatalf("hint = %q, want one saying Nova lacks changeTenant, not the pre-checks", hint)
		}
	})
	if hint := changeTenantRefusalHint(fmt.Errorf("connection reset")); hint != "" {
		t.Fatalf("hint for a transport error = %q, want none", hint)
	}
}

// An instance ID that is not Nova's shape is refused before any request, by
// the move and by the read-back alike, since the ID goes into the URL path.
func TestMoveInstanceToProjectRefusesMalformedIDs(t *testing.T) {
	for name, id := range map[string]string{"empty": "", "slash": "a/b", "query": "a?b", "parent": "../x", "dot": ".", "dotdot": ".."} {
		t.Run(name, func(t *testing.T) {
			m := newTenantMoveNova(t, http.StatusAccepted, "", "p1")
			client, err := m.config.ComputeV2Client()
			if err != nil {
				t.Fatal(err)
			}
			if err := moveInstanceToProject(context.Background(), client, id, "p2"); err == nil || !strings.Contains(err.Error(), "instance_id") {
				t.Fatalf("move: err = %v, want one naming instance_id", err)
			}
			if _, err := waitForInstanceProject(context.Background(), client, id, "p2", func(string) {}); err == nil || !strings.Contains(err.Error(), "instance_id") {
				t.Fatalf("wait: err = %v, want one naming instance_id", err)
			}
			if got := m.received(); len(got) != 0 {
				t.Fatalf("requests = %q, want none", got)
			}
		})
	}
}

// The read-back waits until Nova reports the new project.
func TestWaitForInstanceProject(t *testing.T) {
	shortPolls(t)
	m := newTenantMoveNova(t, http.StatusAccepted, "", "p1", "p1", "p2")
	client, err := m.config.ComputeV2Client()
	if err != nil {
		t.Fatal(err)
	}
	var progress []string
	server, err := waitForInstanceProject(context.Background(), client, "srv-1", "p2", func(msg string) { progress = append(progress, msg) })
	if err != nil {
		t.Fatal(err)
	}
	if server.TenantID != "p2" || len(progress) != 2 {
		t.Fatalf("tenant = %s, progress = %q; want p2 after two reads that showed p1", server.TenantID, progress)
	}
}

// An answer for another instance than the one asked for is an error naming
// both, not a confirmation: Nova answers with its IDs in lower case.
func TestWaitForInstanceProjectRefusesAnotherServer(t *testing.T) {
	shortPolls(t)
	f := newFakeNova(t, novaRoutes{
		"GET /servers/SRV-1": reply(http.StatusOK,
			`{"server": {"id": "srv-1", "name": "vm-1", "status": "ACTIVE", "tenant_id": "p2", "user_id": "u1"}}`),
	})
	client, err := f.config.ComputeV2Client()
	if err != nil {
		t.Fatal(err)
	}
	_, err = waitForInstanceProject(context.Background(), client, "SRV-1", "p2", func(string) {})
	if !errors.Is(err, errInstanceIDMismatch) || !strings.Contains(err.Error(), "SRV-1") || !strings.Contains(err.Error(), "srv-1") ||
		!strings.Contains(err.Error(), "Nova answers in lower case") {
		t.Fatalf("err = %v, want errInstanceIDMismatch naming SRV-1 and srv-1, with the lower-case advice", err)
	}
}

// An answer for an instance whose ID differs in more than case names both IDs
// and gives no lower-case advice, which would not help.
func TestWaitForInstanceProjectRefusesAnotherServerWithoutCaseAdvice(t *testing.T) {
	shortPolls(t)
	f := newFakeNova(t, novaRoutes{
		"GET /servers/srv-1": reply(http.StatusOK,
			`{"server": {"id": "srv-2", "name": "vm-2", "status": "ACTIVE", "tenant_id": "p2", "user_id": "u1"}}`),
	})
	client, err := f.config.ComputeV2Client()
	if err != nil {
		t.Fatal(err)
	}
	_, err = waitForInstanceProject(context.Background(), client, "srv-1", "p2", func(string) {})
	if !errors.Is(err, errInstanceIDMismatch) || !strings.Contains(err.Error(), "asked for instance srv-1, Nova answered for srv-2") ||
		strings.Contains(err.Error(), "lower case") {
		t.Fatalf("err = %v, want errInstanceIDMismatch naming srv-1 and srv-2, without the lower-case advice", err)
	}
}

// An instance that Nova sets to ERROR, in a project other than the target,
// ends the wait with an error naming the instance, its project and Nova's
// fault, without waiting for the timeout.
func TestWaitForInstanceProjectReportsError(t *testing.T) {
	shortPolls(t)
	old := changeTenantConfirmTimeout
	changeTenantConfirmTimeout = time.Second
	t.Cleanup(func() { changeTenantConfirmTimeout = old })
	f := newFakeNova(t, novaRoutes{
		"GET /servers/srv-1": reply(http.StatusOK,
			`{"server": {"id": "srv-1", "name": "vm-1", "status": "ERROR", "tenant_id": "p1", "user_id": "u1", `+
				`"fault": {"code": 500, "message": "Rollback of port pt-1 failed"}}}`),
	})
	client, err := f.config.ComputeV2Client()
	if err != nil {
		t.Fatal(err)
	}
	_, err = waitForInstanceProject(context.Background(), client, "srv-1", "p2", func(string) {})
	if err == nil || !strings.Contains(err.Error(), "instance srv-1 is in ERROR in project p1") ||
		!strings.Contains(err.Error(), "Rollback of port pt-1 failed") {
		t.Fatalf("err = %v, want one naming the instance, project p1 and Nova's fault", err)
	}
	if got := f.received(); len(got) != 1 {
		t.Fatalf("requests = %q, want one read", got)
	}
}

// An instance in ERROR without a fault Nova recorded gets no empty fault
// suffix.
func TestWaitForInstanceProjectReportsErrorWithoutFault(t *testing.T) {
	shortPolls(t)
	f := newFakeNova(t, novaRoutes{
		"GET /servers/srv-1": reply(http.StatusOK,
			`{"server": {"id": "srv-1", "name": "vm-1", "status": "ERROR", "tenant_id": "p1", "user_id": "u1"}}`),
	})
	client, err := f.config.ComputeV2Client()
	if err != nil {
		t.Fatal(err)
	}
	_, err = waitForInstanceProject(context.Background(), client, "srv-1", "p2", func(string) {})
	if err == nil || err.Error() != "instance srv-1 is in ERROR in project p1" {
		t.Fatalf("err = %v, want exactly \"instance srv-1 is in ERROR in project p1\"", err)
	}
}

// Once the instance reports the target project the move is confirmed, even
// in ERROR: the project is checked before the status.
func TestWaitForInstanceProjectTargetInError(t *testing.T) {
	shortPolls(t)
	f := newFakeNova(t, novaRoutes{
		"GET /servers/srv-1": reply(http.StatusOK,
			`{"server": {"id": "srv-1", "name": "vm-1", "status": "ERROR", "tenant_id": "p2", "user_id": "u1"}}`),
	})
	client, err := f.config.ComputeV2Client()
	if err != nil {
		t.Fatal(err)
	}
	server, err := waitForInstanceProject(context.Background(), client, "srv-1", "p2", func(string) {})
	if err != nil || server.TenantID != "p2" {
		t.Fatalf("server = %v, err = %v; want the instance in p2 and no error", server, err)
	}
}

// A read that fails during the wait ends it at once, with an error saying
// that Nova had accepted the move and that the read failed.
func TestWaitForInstanceProjectReadFails(t *testing.T) {
	shortPolls(t)
	f := newFakeNova(t, novaRoutes{
		"GET /servers/srv-1": reply(http.StatusInternalServerError,
			`{"computeFault": {"code": 500, "message": "Unexpected API Error."}}`),
	})
	client, err := f.config.ComputeV2Client()
	if err != nil {
		t.Fatal(err)
	}
	_, err = waitForInstanceProject(context.Background(), client, "srv-1", "p2", func(string) {})
	if err == nil || !strings.Contains(err.Error(), "instance srv-1 could not be read after Nova accepted the move") ||
		!strings.Contains(err.Error(), "500") {
		t.Fatalf("err = %v, want one saying the read failed after Nova accepted the move, with the 500", err)
	}
	if got := f.received(); len(got) != 1 {
		t.Fatalf("requests = %q, want one read", got)
	}
}

// A cancel of the caller's context ends the wait at once, with an error that
// says the wait was interrupted and wraps context.Canceled.
func TestWaitForInstanceProjectCanceled(t *testing.T) {
	shortPolls(t)
	old := changeTenantConfirmTimeout
	changeTenantConfirmTimeout = 10 * time.Second
	t.Cleanup(func() { changeTenantConfirmTimeout = old })
	m := newTenantMoveNova(t, http.StatusAccepted, "", "p1")
	client, err := m.config.ComputeV2Client()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)
	start := time.Now()
	_, err = waitForInstanceProject(ctx, client, "srv-1", "p2", func(string) {})
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("the wait took %s after a cancel at 50ms", elapsed)
	}
	if !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), "interrupted after Nova accepted the move") ||
		strings.Contains(err.Error(), "10s") {
		t.Fatalf("err = %v, want one saying the wait was interrupted, wrapping context.Canceled", err)
	}
}

// The default bound on the wait.
func TestChangeTenantConfirmTimeoutDefault(t *testing.T) {
	if changeTenantConfirmTimeout != 2*time.Minute {
		t.Fatalf("changeTenantConfirmTimeout = %s, want 2m0s", changeTenantConfirmTimeout)
	}
}

// A project that never changes ends the wait with an error naming both; a
// read that never answers ends it with an error saying no read answered.
func TestWaitForInstanceProjectTimesOut(t *testing.T) {
	shortPolls(t)
	old := changeTenantConfirmTimeout
	changeTenantConfirmTimeout = 200 * time.Millisecond
	t.Cleanup(func() { changeTenantConfirmTimeout = old })
	m := newTenantMoveNova(t, http.StatusAccepted, "", "p1")
	client, err := m.config.ComputeV2Client()
	if err != nil {
		t.Fatal(err)
	}
	_, err = waitForInstanceProject(context.Background(), client, "srv-1", "p2", func(string) {})
	if err == nil || !strings.Contains(err.Error(), "still reports project p1, not p2") || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want one naming both projects, wrapping the deadline", err)
	}

	changeTenantConfirmTimeout = 20 * time.Millisecond
	hung := newFakeNova(t, novaRoutes{
		"GET /servers/srv-1": func(_ http.ResponseWriter, r *http.Request) {
			select {
			case <-r.Context().Done():
			case <-time.After(5 * time.Second):
			}
		},
	})
	client, err = hung.config.ComputeV2Client()
	if err != nil {
		t.Fatal(err)
	}
	_, err = waitForInstanceProject(context.Background(), client, "srv-1", "p2", func(string) {})
	if err == nil || !strings.Contains(err.Error(), "instance srv-1 could not be read within 20ms") ||
		!errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want one saying no read answered, wrapping the deadline", err)
	}
}
