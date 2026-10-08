// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package compute

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

// detachFake answers os-interface DELETEs and GETs for srv-1/p-1 from scripts
// (HTTP status codes, the last one repeating) and counts them.
type detachFake struct {
	mu            sync.Mutex
	deletes, gets []int
	nDel, nGet    int
}

func (f *detachFake) next(script []int, n *int) int {
	code := script[min(*n, len(script)-1)]
	*n++
	return code
}

// deleteAttachment runs the resource's Delete against the fake.
func deleteAttachment(t *testing.T, f *detachFake) *resource.DeleteResponse {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		switch r.Method + " " + r.URL.Path {
		case "DELETE /servers/srv-1/os-interface/p-1":
			w.WriteHeader(f.next(f.deletes, &f.nDel))
		case "GET /servers/srv-1/os-interface/p-1":
			code := f.next(f.gets, &f.nGet)
			w.WriteHeader(code)
			if code == http.StatusOK {
				_, _ = w.Write([]byte(`{"interfaceAttachment": {"port_id": "p-1", "net_id": "net-2", "port_state": "ACTIVE", "mac_addr": "fa:16:3e:00:00:01"}}`))
			}
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotImplemented)
		}
	}))
	t.Cleanup(srv.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	r := &interfaceAttachResource{config: fakeConfig(srv.URL)}
	state := newState(t, schemaOf(t, r), &interfaceAttachModel{
		ID: types.StringValue("srv-1/p-1"), InstanceID: types.StringValue("srv-1"), PortID: types.StringValue("p-1"),
		NetworkID: types.StringValue("net-2"), FixedIP: types.StringNull(), MAC: types.StringValue("fa:16:3e:00:00:01"),
		PortState: types.StringValue("ACTIVE"), Region: types.StringValue("region-one"),
	})
	resp := &resource.DeleteResponse{State: state}
	r.Delete(ctx, resource.DeleteRequest{State: state}, resp)
	return resp
}

// Nova detaches an interface after it answers the DELETE. Returning on the
// 202 let the same apply delete the port's subnet while Nova still held the
// port, which Neutron refuses (409 SubnetInUse). Delete now waits for the
// interface to disappear, re-sending the DELETE while it is listed.
func TestInterfaceAttachDeleteWaitsForTheDetach(t *testing.T) {
	cases := []struct {
		name               string
		deletes, gets      []int
		wantErr            string // diagnostic summary; "" means success
		wantDels, wantGets int
	}{
		{name: "gone on the second poll",
			deletes: []int{http.StatusAccepted}, gets: []int{http.StatusOK, http.StatusNotFound},
			wantDels: 2, wantGets: 2},
		{name: "a repeat refused while the detach runs",
			deletes: []int{http.StatusAccepted, http.StatusBadRequest}, gets: []int{http.StatusOK, http.StatusOK, http.StatusNotFound},
			wantDels: 3, wantGets: 3},
		{name: "already detached",
			deletes: []int{http.StatusNotFound}, gets: []int{http.StatusOK},
			wantDels: 1, wantGets: 0},
		{name: "a failed poll is an error",
			deletes: []int{http.StatusAccepted}, gets: []int{http.StatusInternalServerError},
			wantErr: "compute: waiting for interface detach", wantDels: 1, wantGets: 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			shortPolls(t)
			f := &detachFake{deletes: tc.deletes, gets: tc.gets}
			resp := deleteAttachment(t, f)
			switch {
			case tc.wantErr == "" && resp.Diagnostics.HasError():
				t.Fatalf("delete: %v", resp.Diagnostics)
			case tc.wantErr != "" && (!resp.Diagnostics.HasError() || resp.Diagnostics.Errors()[0].Summary() != tc.wantErr):
				t.Fatalf("diagnostics = %v, want %q", resp.Diagnostics, tc.wantErr)
			}
			if f.nDel != tc.wantDels || f.nGet != tc.wantGets {
				t.Fatalf("%d DELETEs and %d GETs, want %d and %d", f.nDel, f.nGet, tc.wantDels, tc.wantGets)
			}
		})
	}
}

// An interface the guest never releases ends the destroy with an error after
// the timeout, instead of reporting a detach that did not happen.
func TestInterfaceAttachDeleteTimesOut(t *testing.T) {
	shortPolls(t)
	old := interfaceDetachTimeout
	interfaceDetachTimeout = 20 * time.Millisecond
	t.Cleanup(func() { interfaceDetachTimeout = old })

	resp := deleteAttachment(t, &detachFake{deletes: []int{http.StatusAccepted}, gets: []int{http.StatusOK}})
	if !resp.Diagnostics.HasError() || resp.Diagnostics.Errors()[0].Summary() != "compute: waiting for interface detach" {
		t.Fatalf("diagnostics = %v, want a detach timeout", resp.Diagnostics)
	}
}
