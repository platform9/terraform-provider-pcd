// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package resmgr

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gophercloud/gophercloud/v2"
)

// A `terraform destroy` of a pcd_cluster_blueprint used to remove it from state
// without ever calling resmgr, so the blueprint outlived the destroy (PCD-9783).
// The delete has to reach DELETE /resmgr/v2/blueprint/<name>.
func TestDeleteBlueprintCallsTheAPI(t *testing.T) {
	var method, path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method, path = r.Method, r.URL.Path
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	client := &gophercloud.ServiceClient{ProviderClient: &gophercloud.ProviderClient{}, Endpoint: srv.URL + "/"}

	if err := deleteBlueprint(context.Background(), client, "ts-bp"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if method != http.MethodDelete || path != "/blueprint/ts-bp" {
		t.Fatalf("got %s %s, want DELETE /blueprint/ts-bp", method, path)
	}
}

func TestDeleteBlueprintStatusHandling(t *testing.T) {
	for _, tc := range []struct {
		name    string
		status  int
		wantErr bool
	}{
		{name: "200 is success", status: http.StatusOK},
		{name: "202 is success", status: http.StatusAccepted},
		{name: "204 is success", status: http.StatusNoContent},
		// Already gone is the outcome destroy wants; it must not fail the destroy.
		{name: "404 is already gone", status: http.StatusNotFound},
		// Anything else means the blueprint is still there, and a destroy that
		// reports success over it is the very bug being fixed.
		{name: "409 is an error", status: http.StatusConflict, wantErr: true},
		{name: "500 is an error", status: http.StatusInternalServerError, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
			}))
			defer srv.Close()
			client := &gophercloud.ServiceClient{ProviderClient: &gophercloud.ProviderClient{}, Endpoint: srv.URL + "/"}

			err := deleteBlueprint(context.Background(), client, "ts-bp")
			if tc.wantErr && err == nil {
				t.Fatalf("status %d: got no error; destroy would report success over a blueprint that still exists", tc.status)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("status %d: unexpected error: %v", tc.status, err)
			}
		})
	}
}
