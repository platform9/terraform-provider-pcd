// Copyright (c) Platform9 Systems, Inc.
// SPDX-License-Identifier: MPL-2.0

package clients

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/extensions/security/groups"
)

// createSecGroupAnswered sends a security group create to a server that
// answers it with status and body, and returns what Extract decoded.
func createSecGroupAnswered(t *testing.T, status int, body string) (*groups.SecGroup, error) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		fmt.Fprint(w, body)
	}))
	t.Cleanup(srv.Close)
	client := &gophercloud.ServiceClient{
		ProviderClient: &gophercloud.ProviderClient{HTTPClient: http.Client{Transport: &http.Transport{}}},
		Endpoint:       srv.URL + "/",
		ResourceBase:   srv.URL + "/v2.0/",
	}
	return groups.Create(context.Background(), client, groups.CreateOpts{Name: "sg"}).Extract()
}

// gophercloud decodes a 2xx answer without the object's key to a nil object
// and no error, which every caller then dereferenced. RequireObject must turn
// that into an error.
func TestRequireObjectRefusesASuccessWithoutTheObject(t *testing.T) {
	t.Parallel()
	sg, err := RequireObject(createSecGroupAnswered(t, http.StatusCreated, `{}`))
	if !errors.Is(err, ErrNoObject) {
		t.Fatalf("err = %v, want ErrNoObject", err)
	}
	if sg != nil {
		t.Fatalf("object = %+v, want nil", sg)
	}
}

// An answer that holds the object passes through unchanged.
func TestRequireObjectPassesTheObjectThrough(t *testing.T) {
	t.Parallel()
	sg, err := RequireObject(createSecGroupAnswered(t, http.StatusCreated, `{"security_group": {"id": "sg-1"}}`))
	if err != nil || sg == nil || sg.ID != "sg-1" {
		t.Fatalf("got %+v, %v; want sg-1 and no error", sg, err)
	}
}

// An error passes through unchanged, so a caller can still test it, for
// example for a 404.
func TestRequireObjectPassesAnErrorThrough(t *testing.T) {
	t.Parallel()
	_, err := RequireObject(createSecGroupAnswered(t, http.StatusNotFound, `{}`))
	if !gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
		t.Fatalf("err = %v, want the 404", err)
	}
}
