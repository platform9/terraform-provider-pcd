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

// resmgr answers 200 with a body of `null` for an object that is gone, so absence
// has to be read out of the body rather than the status code. Decoded straight into
// a struct that is a nil unmarshal: no error, every field zero — which a Read would
// report as present-but-blank, never removing the resource from state.
func TestGetJSONTreatsANullBodyAsAbsent(t *testing.T) {
	type cluster struct {
		Name string `json:"name"`
	}

	for _, tc := range []struct {
		name       string
		status     int
		body       string
		wantAbsent bool
		wantName   string
	}{
		{name: "null body is absence", status: 200, body: "null", wantAbsent: true},
		{name: "null body with whitespace is absence", status: 200, body: "  null\n", wantAbsent: true},
		{name: "empty body is absence", status: 200, body: "", wantAbsent: true},
		{name: "404 is absence", status: 404, body: `{"message":"not found"}`, wantAbsent: true},
		{name: "an object decodes", status: 200, body: `{"name":"ts-cluster"}`, wantName: "ts-cluster"},
		// Defensive: no caller fetches a list through getJSON today, and an empty one
		// must not be mistaken for a missing object if one ever does.
		{name: "an empty list is not absence", status: 200, body: `[]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()

			client := &gophercloud.ServiceClient{
				ProviderClient: &gophercloud.ProviderClient{},
				Endpoint:       srv.URL + "/",
			}

			var out any = &cluster{}
			if tc.body == `[]` {
				out = &[]cluster{}
			}
			err := getJSON(context.Background(), client, client.ServiceURL("clusters", "ts-cluster"), out)

			if tc.wantAbsent {
				if err == nil {
					t.Fatalf("got no error, want one the Read paths can recognise as absence")
				}
				if !isNotFound(err) {
					t.Fatalf("isNotFound(%v) = false; a Read would keep a destroyed object in state", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tc.wantName != "" && out.(*cluster).Name != tc.wantName {
				t.Fatalf("name = %q, want %q", out.(*cluster).Name, tc.wantName)
			}
		})
	}
}

func TestIsNullJSON(t *testing.T) {
	for _, raw := range []string{"null", " null ", "\n", ""} {
		if !isNullJSON([]byte(raw)) {
			t.Errorf("isNullJSON(%q) = false, want true", raw)
		}
	}
	for _, raw := range []string{`{}`, `[]`, `{"name":"x"}`, `"null"`} {
		if isNullJSON([]byte(raw)) {
			t.Errorf("isNullJSON(%q) = true, want false", raw)
		}
	}
}
